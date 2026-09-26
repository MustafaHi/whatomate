package whatsmeow

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestReplyQuote(t *testing.T) {
	contact := types.NewJID("15553334444", types.DefaultUserServer)
	own := types.NewJID("15551112222", types.DefaultUserServer)
	lid := types.NewJID("987654321", types.HiddenUserServer)

	// Real sender JIDs carry a device part that quotes must not.
	withDevice, err := types.ParseJID("15553334444:24@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}

	inboundBody := &waE2E.Message{Conversation: proto.String("hello")}
	ownBody := &waE2E.Message{Conversation: proto.String("my earlier text")}

	cases := []struct {
		name        string
		ref         recentEntry
		found       bool
		own         types.JID
		to          types.JID
		wantAuthor  string
		wantBodyNil bool // empty quote proto, no content
	}{
		{
			name:       "inbound quote points at the contact",
			ref:        recentEntry{chat: contact, sender: contact, msg: inboundBody},
			found:      true,
			to:         contact,
			wantAuthor: "15553334444@s.whatsapp.net",
		},
		{
			name:       "self reply points at our own JID",
			ref:        recentEntry{chat: contact, sender: contact, msg: ownBody, fromMe: true},
			found:      true,
			own:        own,
			to:         contact,
			wantAuthor: "15551112222@s.whatsapp.net",
		},
		{
			name:       "lid chat quotes the lid author",
			ref:        recentEntry{chat: lid, sender: lid, msg: inboundBody},
			found:      true,
			to:         contact,
			wantAuthor: "987654321@lid",
		},
		{
			name:       "device part stripped from author jid",
			ref:        recentEntry{chat: contact, sender: withDevice, msg: inboundBody},
			found:      true,
			to:         contact,
			wantAuthor: "15553334444@s.whatsapp.net",
		},
		{
			name:        "unknown id falls back to contact with empty body",
			found:       false,
			to:          contact,
			wantAuthor:  "15553334444@s.whatsapp.net",
			wantBodyNil: true,
		},
	}

	for _, tc := range cases {
		participant, quoted := replyQuote(tc.ref, tc.found, tc.own, tc.to)
		if participant != tc.wantAuthor {
			t.Errorf("%s: participant = %q, want %q", tc.name, participant, tc.wantAuthor)
		}
		if tc.wantBodyNil {
			if quoted.GetConversation() != "" || quoted.GetExtendedTextMessage() != nil {
				t.Errorf("%s: expected empty quote body", tc.name)
			}
			continue
		}
		if quoted.GetConversation() != tc.ref.msg.GetConversation() {
			t.Errorf("%s: quote body = %q, want %q", tc.name, quoted.GetConversation(), tc.ref.msg.GetConversation())
		}
	}
}
