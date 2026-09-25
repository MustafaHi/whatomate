package whatsmeow

import (
	"context"
	"fmt"
	"sync"
	"time"

	wa "go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
)

// outboundTTL bounds how long uploaded media bytes wait for their Send call.
// The unified send path uploads and then sends synchronously, so entries live
// for milliseconds; the TTL only cleans up after failures.
const outboundTTL = 5 * time.Minute

// recvIndexMax bounds the recent-inbound index used for read receipts.
const recvIndexMax = 1000

// session is one connected WhatsApp multidevice session. It implements
// whatsapp.Sender over the whatsmeow client.
type session struct {
	mgr       *Manager
	accountID uuid.UUID
	phoneID   string // own phone number digits
	client    *wa.Client

	mu        sync.Mutex
	connected bool
	outbound  map[string]outboundEntry // staged UploadMedia bytes, keyed by synthetic media ID
	recvIndex map[string]recvEntry     // inbound message ID -> chat/sender for MarkRead
	recvOrder []string                 // recvIndex insertion order for eviction
}

type outboundEntry struct {
	data     []byte
	mimeType string
	filename string
	at       time.Time
}

type recvEntry struct {
	chat   types.JID
	sender types.JID
	at     time.Time
}

// handleEvent is the whatsmeow event handler. Registered before Connect.
func (s *session) handleEvent(evt any) {
	switch e := evt.(type) {
	case *events.Message:
		s.handleInbound(e)
	case *events.Receipt:
		status := MapReceipt(e.Type)
		if status == "" {
			return
		}
		for _, id := range e.MessageIDs {
			s.mgr.onStatus(s.phoneID, StatusUpdate{MessageID: string(id), Status: status})
		}
	case *events.Connected:
		s.mu.Lock()
		s.connected = true
		s.mu.Unlock()
		s.mgr.Log.Info("whatsmeow session connected", "account_id", s.accountID, "phone", s.phoneID)
	case *events.Disconnected:
		s.mu.Lock()
		s.connected = false
		s.mu.Unlock()
		s.mgr.Log.Warn("whatsmeow session disconnected", "account_id", s.accountID, "phone", s.phoneID)
	case *events.LoggedOut:
		s.mu.Lock()
		s.connected = false
		s.mu.Unlock()
		// ponytail: no automatic re-pair flow — the account shows as
		// disconnected and the user re-pairs from the UI.
		s.mgr.Log.Error("whatsmeow session logged out — re-pair required", "account_id", s.accountID, "phone", s.phoneID)
	}
}

func (s *session) handleInbound(evt *events.Message) {
	msg := MapInbound(evt)
	if msg == nil {
		return
	}

	// Remember for read receipts (MarkMessageRead only gets the message ID).
	s.mu.Lock()
	s.recvIndex[msg.ID] = recvEntry{chat: evt.Info.Chat, sender: evt.Info.Sender, at: evt.Info.Timestamp}
	s.recvOrder = append(s.recvOrder, msg.ID)
	for len(s.recvOrder) > recvIndexMax {
		delete(s.recvIndex, s.recvOrder[0])
		s.recvOrder = s.recvOrder[1:]
	}
	s.mu.Unlock()

	if isMediaPart(evt.Message) {
		ctx, cancel := context.WithTimeout(s.mgr.ctx, 60*time.Second)
		data, err := s.client.DownloadAny(ctx, evt.Message)
		cancel()
		if err != nil {
			// Matches the Meta path: log and store the message without media.
			s.mgr.Log.Error("Failed to download inbound media", "error", err, "message_id", msg.ID)
		} else {
			msg.MediaData = data
		}
	}

	s.mgr.onInbound(s.phoneID, *msg)
}

func isMediaPart(msg *waE2E.Message) bool {
	return msg.GetImageMessage() != nil || msg.GetVideoMessage() != nil ||
		msg.GetAudioMessage() != nil || msg.GetDocumentMessage() != nil ||
		msg.GetStickerMessage() != nil
}

// ============================================================================
// whatsapp.Sender implementation
// ============================================================================

func (s *session) SendTextMessage(ctx context.Context, account *whatsapp.Account, rcpt whatsapp.Recipient, text string, replyToMsgID ...string) (string, error) {
	to, err := s.recipientJID(rcpt)
	if err != nil {
		return "", err
	}

	var msg *waE2E.Message
	if len(replyToMsgID) > 0 && replyToMsgID[0] != "" {
		// ponytail: quoted body is unknown at this seam, so the reply carries
		// only the stanza reference — WhatsApp still threads it in most clients.
		msg = &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String(text),
				ContextInfo: &waE2E.ContextInfo{
					StanzaID:      proto.String(replyToMsgID[0]),
					Participant:   proto.String(to.String()),
					QuotedMessage: &waE2E.Message{Conversation: proto.String("")},
				},
			},
		}
	} else {
		msg = &waE2E.Message{Conversation: proto.String(text)}
	}
	return s.send(ctx, to, msg)
}

func (s *session) SendImageMessage(ctx context.Context, account *whatsapp.Account, rcpt whatsapp.Recipient, mediaID, caption string) (string, error) {
	return s.sendMedia(ctx, rcpt, mediaID, wa.MediaImage,
		func(up wa.UploadResponse, mime string) *waE2E.Message {
			return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
				URL:           proto.String(up.URL),
				DirectPath:    proto.String(up.DirectPath),
				MediaKey:      up.MediaKey,
				FileSHA256:    up.FileSHA256,
				FileEncSHA256: up.FileEncSHA256,
				FileLength:    proto.Uint64(up.FileLength),
				Mimetype:      proto.String(mime),
				Caption:       proto.String(caption),
			}}
		})
}

func (s *session) SendVideoMessage(ctx context.Context, account *whatsapp.Account, rcpt whatsapp.Recipient, mediaID, caption string) (string, error) {
	return s.sendMedia(ctx, rcpt, mediaID, wa.MediaVideo,
		func(up wa.UploadResponse, mime string) *waE2E.Message {
			return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
				URL:           proto.String(up.URL),
				DirectPath:    proto.String(up.DirectPath),
				MediaKey:      up.MediaKey,
				FileSHA256:    up.FileSHA256,
				FileEncSHA256: up.FileEncSHA256,
				FileLength:    proto.Uint64(up.FileLength),
				Mimetype:      proto.String(mime),
				Caption:       proto.String(caption),
			}}
		})
}

func (s *session) SendAudioMessage(ctx context.Context, account *whatsapp.Account, rcpt whatsapp.Recipient, mediaID string) (string, error) {
	return s.sendMedia(ctx, rcpt, mediaID, wa.MediaAudio,
		func(up wa.UploadResponse, mime string) *waE2E.Message {
			return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
				URL:           proto.String(up.URL),
				DirectPath:    proto.String(up.DirectPath),
				MediaKey:      up.MediaKey,
				FileSHA256:    up.FileSHA256,
				FileEncSHA256: up.FileEncSHA256,
				FileLength:    proto.Uint64(up.FileLength),
				Mimetype:      proto.String(mime),
			}}
		})
}

func (s *session) SendDocumentMessage(ctx context.Context, account *whatsapp.Account, rcpt whatsapp.Recipient, mediaID, filename, caption string) (string, error) {
	return s.sendMedia(ctx, rcpt, mediaID, wa.MediaDocument,
		func(up wa.UploadResponse, mime string) *waE2E.Message {
			return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
				URL:           proto.String(up.URL),
				DirectPath:    proto.String(up.DirectPath),
				MediaKey:      up.MediaKey,
				FileSHA256:    up.FileSHA256,
				FileEncSHA256: up.FileEncSHA256,
				FileLength:    proto.Uint64(up.FileLength),
				Mimetype:      proto.String(mime),
				FileName:      proto.String(filename),
				Caption:       proto.String(caption),
			}}
		})
}

// sendMedia uploads staged bytes and sends the message built from the upload
// response. The whatsapp.Sender flow is upload-then-send-by-ID, but whatsmeow
// needs the raw bytes at send time — hence the short-lived staging map.
func (s *session) sendMedia(ctx context.Context, rcpt whatsapp.Recipient, mediaID string, mediaType wa.MediaType, build func(wa.UploadResponse, string) *waE2E.Message) (string, error) {
	to, err := s.recipientJID(rcpt)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	entry, ok := s.outbound[mediaID]
	if ok {
		delete(s.outbound, mediaID)
	}
	s.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("media not found: %s (re-upload the file before sending on this provider)", mediaID)
	}

	up, err := s.client.Upload(ctx, entry.data, mediaType)
	if err != nil {
		return "", fmt.Errorf("failed to upload media: %w", err)
	}

	return s.send(ctx, to, build(up, entry.mimeType))
}

func (s *session) send(ctx context.Context, to types.JID, msg *waE2E.Message) (string, error) {
	resp, err := s.client.SendMessage(ctx, to, msg)
	if err != nil {
		return "", err
	}
	return string(resp.ID), nil
}

func (s *session) UploadMedia(_ context.Context, account *whatsapp.Account, data []byte, mimeType, filename string) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("empty media upload")
	}
	mediaID := "meow:" + uuid.New().String()
	s.mu.Lock()
	// Lazy sweep of expired entries.
	now := time.Now()
	for id, e := range s.outbound {
		if now.Sub(e.at) > outboundTTL {
			delete(s.outbound, id)
		}
	}
	s.outbound[mediaID] = outboundEntry{data: data, mimeType: mimeType, filename: filename, at: now}
	s.mu.Unlock()
	return mediaID, nil
}

// MarkMessageRead sends a read receipt. The chat/sender JIDs come from the
// inbound index — the Sender interface only carries the message ID.
func (s *session) MarkMessageRead(ctx context.Context, account *whatsapp.Account, messageID string) error {
	s.mu.Lock()
	ref, ok := s.recvIndex[messageID]
	s.mu.Unlock()
	if !ok {
		// Never seen inbound here (e.g. restart) — receipt is best-effort.
		return nil
	}
	return s.client.MarkRead(ctx, []types.MessageID{types.MessageID(messageID)}, time.Now(), ref.chat, ref.sender)
}

func (s *session) GetMediaURL(_ context.Context, mediaID string, account *whatsapp.Account) (string, error) {
	// Inbound whatsmeow media is staged locally by the inbound adapter, so
	// DownloadAndSaveMedia never reaches here.
	return "", whatsapp.ErrUnsupported
}

func (s *session) DownloadMedia(_ context.Context, mediaURL string, accessToken string) ([]byte, error) {
	return nil, whatsapp.ErrUnsupported
}

func (s *session) SendInteractiveButtons(_ context.Context, account *whatsapp.Account, rcpt whatsapp.Recipient, bodyText string, buttons []whatsapp.Button) (string, error) {
	return "", whatsapp.ErrUnsupported
}

func (s *session) SendCTAURLButton(_ context.Context, account *whatsapp.Account, rcpt whatsapp.Recipient, bodyText, buttonText, url string) (string, error) {
	return "", whatsapp.ErrUnsupported
}

func (s *session) SendTemplateMessage(_ context.Context, account *whatsapp.Account, rcpt whatsapp.Recipient, templateName, languageCode string, components []map[string]any) (string, error) {
	return "", whatsapp.ErrUnsupported
}

func (s *session) SendFlowMessage(_ context.Context, account *whatsapp.Account, rcpt whatsapp.Recipient, flowID, headerText, bodyText, ctaText, flowToken, firstScreen string) (string, error) {
	return "", whatsapp.ErrUnsupported
}

func (s *session) SendVoiceCallButton(_ context.Context, account *whatsapp.Account, rcpt whatsapp.Recipient, bodyText, displayText string, ttlMinutes int, payload string) (string, error) {
	return "", whatsapp.ErrUnsupported
}

// recipientJID converts a Recipient to a user JID. BSUID-only recipients
// (username users) are not supported over whatsmeow.
func (s *session) recipientJID(rcpt whatsapp.Recipient) (types.JID, error) {
	digits := NormalizeDigits(rcpt.Phone)
	if digits == "" {
		return types.JID{}, fmt.Errorf("whatsmeow requires a phone number (BSUID-only messaging is not supported)")
	}
	return types.NewJID(digits, types.DefaultUserServer), nil
}
