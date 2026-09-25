// Package whatsmeow bridges unofficial WhatsApp multidevice sessions
// (QR-linked via go.mau.fi/whatsmeow) into whatomate's messaging pipeline.
package whatsmeow

import (
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// InboundMessage is the neutral shape handed to the handlers layer for every
// incoming message. Media blobs are already decrypted when MediaData is set.
type InboundMessage struct {
	ID       string // WhatsApp message ID (used for dedup + status tracking)
	From     string // sender phone digits, no "+"
	Type     string // text|image|video|audio|document|sticker|reaction|location
	Text     string // body or caption
	MimeType string
	Filename string

	// location
	Lat, Long    float64
	PlaceName    string
	PlaceAddress string

	// reaction
	ReactionTarget string
	ReactionEmoji  string

	ContextID string // WhatsApp ID of the quoted message, if this is a reply

	Timestamp time.Time
	PushName  string
	MediaData []byte
}

// StatusUpdate mirrors a Meta webhook status for one message.
type StatusUpdate struct {
	MessageID string
	Status    string // delivered|read
}

// NormalizeDigits strips everything but digits from a phone/JID user string.
func NormalizeDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SenderPhone picks the phone-number JID for a message, preferring the
// phone-number alternative when the chat is LID-addressed.
// ponytail: best-effort LID handling; revisit if @lid-only contacts appear.
func SenderPhone(info types.MessageInfo) string {
	if info.AddressingMode == types.AddressingModeLID && !info.SenderAlt.IsEmpty() && info.SenderAlt.Server == types.DefaultUserServer {
		return NormalizeDigits(info.SenderAlt.User)
	}
	return NormalizeDigits(info.Sender.User)
}

// MapInbound converts a whatsmeow message event into the neutral inbound
// shape. Returns nil for messages whatomate should ignore (groups, own sends,
// protocol/service messages, status broadcasts).
func MapInbound(evt *events.Message) *InboundMessage {
	if evt == nil || evt.Message == nil || evt.Info.ID == "" {
		return nil
	}
	info := evt.Info
	if info.IsGroup || info.IsFromMe || info.DeviceSentMeta != nil {
		// ponytail: messages sent from the user's own phone are skipped;
		// Meta surfaces those as smb_message_echoes webhooks, whatsmeow
		// gives no clean echo path for v1.
		return nil
	}
	if info.Chat.Server != types.DefaultUserServer && info.Chat.Server != types.HiddenUserServer {
		return nil // status broadcasts, newsletters, lists
	}
	if evt.Message.GetProtocolMessage() != nil || evt.Message.GetSenderKeyDistributionMessage() != nil {
		return nil
	}

	sender := SenderPhone(info)
	if sender == "" {
		return nil
	}

	msg := &InboundMessage{
		ID:        string(info.ID),
		From:      sender,
		Timestamp: info.Timestamp,
		PushName:  info.PushName,
	}

	switch {
	case evt.Message.GetConversation() != "":
		msg.Type = "text"
		msg.Text = evt.Message.GetConversation()

	case evt.Message.GetExtendedTextMessage() != nil:
		ext := evt.Message.GetExtendedTextMessage()
		msg.Type = "text"
		msg.Text = ext.GetText()
		if ctxInfo := ext.GetContextInfo(); ctxInfo.GetStanzaID() != "" {
			msg.ContextID = ctxInfo.GetStanzaID()
		}

	case evt.Message.GetImageMessage() != nil:
		img := evt.Message.GetImageMessage()
		msg.Type = "image"
		msg.MimeType = img.GetMimetype()
		msg.Text = img.GetCaption()

	case evt.Message.GetVideoMessage() != nil:
		vid := evt.Message.GetVideoMessage()
		msg.Type = "video"
		msg.MimeType = vid.GetMimetype()
		msg.Text = vid.GetCaption()

	case evt.Message.GetAudioMessage() != nil:
		msg.Type = "audio"
		msg.MimeType = evt.Message.GetAudioMessage().GetMimetype()

	case evt.Message.GetDocumentMessage() != nil:
		doc := evt.Message.GetDocumentMessage()
		msg.Type = "document"
		msg.MimeType = doc.GetMimetype()
		msg.Filename = doc.GetFileName()
		msg.Text = doc.GetCaption()

	case evt.Message.GetStickerMessage() != nil:
		msg.Type = "sticker"
		msg.MimeType = evt.Message.GetStickerMessage().GetMimetype()

	case evt.Message.GetReactionMessage() != nil:
		re := evt.Message.GetReactionMessage()
		msg.Type = "reaction"
		msg.ReactionTarget = re.GetKey().GetID()
		msg.ReactionEmoji = re.GetText()

	case evt.Message.GetLocationMessage() != nil:
		loc := evt.Message.GetLocationMessage()
		msg.Type = "location"
		msg.Lat = loc.GetDegreesLatitude()
		msg.Long = loc.GetDegreesLongitude()
		msg.PlaceName = loc.GetName()
		msg.PlaceAddress = loc.GetAddress()

	default:
		return nil
	}

	return msg
}

// MapReceipt converts a whatsmeow receipt type to the status string used by
// the Meta webhook pipeline. Empty string means "ignore this receipt".
func MapReceipt(rt types.ReceiptType) string {
	switch rt {
	case types.ReceiptTypeDelivered:
		return "delivered"
	case types.ReceiptTypeRead, types.ReceiptTypePlayed:
		return "read"
	default:
		return ""
	}
}
