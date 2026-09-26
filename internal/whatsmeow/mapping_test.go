package whatsmeow

import (
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func info(chat, sender string, isGroup, isFromMe bool) types.MessageInfo {
	return types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     types.NewJID(chat, types.DefaultUserServer),
			Sender:   types.NewJID(sender, types.DefaultUserServer),
			IsGroup:  isGroup,
			IsFromMe: isFromMe,
		},
		ID:        "MSGID1",
		Timestamp: time.Unix(1700000000, 0),
		PushName:  "Alice",
	}
}

func TestNormalizeDigits(t *testing.T) {
	cases := map[string]string{
		"+15551234567":      "15551234567",
		"15551234567.12:34": "155512345671234",
		"":                  "",
	}
	for in, want := range cases {
		if got := NormalizeDigits(in); got != want {
			t.Errorf("NormalizeDigits(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMapInbound_Text(t *testing.T) {
	evt := &events.Message{
		Info: info("15551112222", "15553334444", false, false),
		Message: &waE2E.Message{
			Conversation: proto.String("hello there"),
		},
	}
	msg := MapInbound(evt)
	if msg == nil {
		t.Fatal("expected message to be mapped")
	}
	if msg.Type != "text" || msg.Text != "hello there" {
		t.Errorf("unexpected mapping: %+v", msg)
	}
	if msg.From != "15553334444" {
		t.Errorf("From = %q, want 15553334444", msg.From)
	}
	if msg.ID != "MSGID1" || msg.PushName != "Alice" {
		t.Errorf("ID/PushName not carried: %+v", msg)
	}
}

func TestMapInbound_Reply(t *testing.T) {
	evt := &events.Message{
		Info: info("15551112222", "15553334444", false, false),
		Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String("a reply"),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:    proto.String("QUOTED123"),
				Participant: proto.String("15553334444@s.whatsapp.net"),
			},
		}},
	}
	msg := MapInbound(evt)
	if msg == nil || msg.ContextID != "QUOTED123" || msg.Text != "a reply" {
		t.Fatalf("unexpected mapping: %+v", msg)
	}
}

func TestMapInbound_Media(t *testing.T) {
	evt := &events.Message{
		Info: info("15551112222", "15553334444", false, false),
		Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			Mimetype: proto.String("image/jpeg"),
			Caption:  proto.String("a pic"),
		}},
	}
	msg := MapInbound(evt)
	if msg == nil || msg.Type != "image" || msg.MimeType != "image/jpeg" || msg.Text != "a pic" {
		t.Fatalf("unexpected mapping: %+v", msg)
	}
	if mediaPart(evt.Message) == nil {
		t.Error("image message should be recognized as media part")
	}
}

func TestMapInbound_ReactionAndLocation(t *testing.T) {
	re := &events.Message{
		Info: info("15551112222", "15553334444", false, false),
		Message: &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
			Key:  &waCommon.MessageKey{ID: proto.String("TARGET1")},
			Text: proto.String("👍"),
		}},
	}
	msg := MapInbound(re)
	if msg == nil || msg.Type != "reaction" || msg.ReactionTarget != "TARGET1" || msg.ReactionEmoji != "👍" {
		t.Fatalf("unexpected reaction mapping: %+v", msg)
	}

	loc := &events.Message{
		Info: info("15551112222", "15553334444", false, false),
		Message: &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
			DegreesLatitude:  proto.Float64(52.5),
			DegreesLongitude: proto.Float64(13.4),
			Name:             proto.String("Brandenburg Gate"),
		}},
	}
	msg = MapInbound(loc)
	if msg == nil || msg.Type != "location" || msg.Lat != 52.5 || msg.PlaceName != "Brandenburg Gate" {
		t.Fatalf("unexpected location mapping: %+v", msg)
	}
}

func TestMapInbound_Skipped(t *testing.T) {
	cases := []struct {
		name string
		evt  *events.Message
	}{
		{"group", &events.Message{Info: info("1203@g.us", "15553334444", true, false),
			Message: &waE2E.Message{Conversation: proto.String("x")}}},
		{"own", &events.Message{Info: info("15551112222", "15559999999", false, true),
			Message: &waE2E.Message{Conversation: proto.String("x")}}},
		{"empty id", &events.Message{Info: types.MessageInfo{},
			Message: &waE2E.Message{Conversation: proto.String("x")}}},
		{"protocol", &events.Message{Info: info("15551112222", "15553334444", false, false),
			Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{}}}},
		{"unsupported type", &events.Message{Info: info("15551112222", "15553334444", false, false),
			Message: &waE2E.Message{ContactsArrayMessage: &waE2E.ContactsArrayMessage{}}},
		},
	}
	for _, tc := range cases {
		if got := MapInbound(tc.evt); got != nil {
			t.Errorf("%s: expected nil, got %+v", tc.name, got)
		}
	}
}

func TestMapInbound_LIDSenderPrefersAlt(t *testing.T) {
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:           types.NewJID("987654321", types.HiddenUserServer),
				Sender:         types.NewJID("987654321", types.HiddenUserServer),
				SenderAlt:      types.NewJID("15553334444", types.DefaultUserServer),
				AddressingMode: types.AddressingModeLID,
			},
			ID: "MSGID1",
		},
		Message: &waE2E.Message{Conversation: proto.String("lid hello")},
	}
	msg := MapInbound(evt)
	if msg == nil {
		t.Fatal("expected message to be mapped")
	}
	if msg.From != "15553334444" {
		t.Errorf("From = %q, want phone-number alt 15553334444", msg.From)
	}
}

func TestMapInbound_LIDOnlySenderDropped(t *testing.T) {
	// A sender with no phone-number form must not be stored as a contact.
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:           types.NewJID("987654321", types.HiddenUserServer),
				Sender:         types.NewJID("213730742792271", types.HiddenUserServer),
				AddressingMode: types.AddressingModeLID,
			},
			ID: "MSGID1",
		},
		Message: &waE2E.Message{Conversation: proto.String("unresolvable")},
	}
	if msg := MapInbound(evt); msg != nil {
		t.Errorf("LID-only sender should be dropped, got %+v", msg)
	}
}

func TestMapReceipt(t *testing.T) {
	if got := MapReceipt(types.ReceiptTypeDelivered); got != "delivered" {
		t.Errorf("delivered receipt mapped to %q", got)
	}
	if got := MapReceipt(types.ReceiptTypeRead); got != "read" {
		t.Errorf("read receipt mapped to %q", got)
	}
	if got := MapReceipt(types.ReceiptTypeSender); got != "" {
		t.Errorf("sender receipt should be ignored, got %q", got)
	}
}

func TestUnwrapEnvelopes(t *testing.T) {
	if got := UnwrapEnvelopes(&waE2E.Message{Conversation: proto.String("plain")}); got.GetConversation() != "plain" {
		t.Errorf("plain message altered: %+v", got)
	}
	if got := UnwrapEnvelopes(nil); got != nil {
		t.Errorf("nil message altered: %+v", got)
	}

	disappearing := &waE2E.Message{
		EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
			Conversation: proto.String("vanishing text"),
		}},
	}
	if got := UnwrapEnvelopes(disappearing); got.GetConversation() != "vanishing text" {
		t.Errorf("disappearing message not unwrapped: %+v", got)
	}

	nested := &waE2E.Message{
		EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
			ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: &waE2E.Message{
				ImageMessage: &waE2E.ImageMessage{Caption: proto.String("view once")},
			}},
		}},
	}
	if got := UnwrapEnvelopes(nested); got.GetImageMessage() == nil {
		t.Errorf("nested view-once image not unwrapped: %+v", got)
	}

	captionedDoc := &waE2E.Message{
		DocumentWithCaptionMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
			DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("report.pdf")},
		}},
	}
	if got := UnwrapEnvelopes(captionedDoc); got.GetDocumentMessage() == nil {
		t.Errorf("captioned document not unwrapped: %+v", got)
	}
}

func TestMapInbound_DisappearingText(t *testing.T) {
	// handleInbound unwraps before mapping; verify the unwrapped event maps.
	evt := &events.Message{
		Info: info("15551112222", "15553334444", false, false),
		Message: UnwrapEnvelopes(&waE2E.Message{
			EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
				Conversation: proto.String("will disappear"),
			}},
		}),
	}
	msg := MapInbound(evt)
	if msg == nil || msg.Type != "text" || msg.Text != "will disappear" {
		t.Errorf("disappearing text not mapped: %+v", msg)
	}
}

// Compile-time: session and errSender must satisfy the provider Sender surface.
var (
	_ whatsapp.Sender = (*session)(nil)
	_ whatsapp.Sender = errSender{}
)
