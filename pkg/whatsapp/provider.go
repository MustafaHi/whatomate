package whatsapp

import (
	"context"
	"errors"
)

// Provider identifiers for whatsapp.Account.Provider.
const (
	ProviderMeta      = "meta"      // Meta WhatsApp Cloud API (default)
	ProviderWhatsmeow = "whatsmeow" // unofficial QR-linked multidevice session
)

// ErrUnsupported is returned by providers that cannot perform an operation,
// e.g. whatsmeow cannot send template or flow messages.
var ErrUnsupported = errors.New("operation not supported by this provider")

// Sender is the provider-agnostic messaging surface. *Client (Meta Cloud API)
// satisfies it directly; other providers (whatsmeow) implement the same
// methods over their own transport. Callers resolve a Sender per account via
// the account's Provider field.
type Sender interface {
	SendTextMessage(ctx context.Context, account *Account, rcpt Recipient, text string, replyToMsgID ...string) (string, error)
	SendImageMessage(ctx context.Context, account *Account, rcpt Recipient, mediaID, caption string) (string, error)
	SendDocumentMessage(ctx context.Context, account *Account, rcpt Recipient, mediaID, filename, caption string) (string, error)
	SendVideoMessage(ctx context.Context, account *Account, rcpt Recipient, mediaID, caption string) (string, error)
	SendAudioMessage(ctx context.Context, account *Account, rcpt Recipient, mediaID string) (string, error)
	SendInteractiveButtons(ctx context.Context, account *Account, rcpt Recipient, bodyText string, buttons []Button) (string, error)
	SendCTAURLButton(ctx context.Context, account *Account, rcpt Recipient, bodyText, buttonText, url string) (string, error)
	SendTemplateMessage(ctx context.Context, account *Account, rcpt Recipient, templateName, languageCode string, components []map[string]any) (string, error)
	SendFlowMessage(ctx context.Context, account *Account, rcpt Recipient, flowID, headerText, bodyText, ctaText, flowToken, firstScreen string) (string, error)
	SendVoiceCallButton(ctx context.Context, account *Account, rcpt Recipient, bodyText, displayText string, ttlMinutes int, payload string) (string, error)
	MarkMessageRead(ctx context.Context, account *Account, messageID string) error
	UploadMedia(ctx context.Context, account *Account, data []byte, mimeType, filename string) (string, error)
	GetMediaURL(ctx context.Context, mediaID string, account *Account) (string, error)
	DownloadMedia(ctx context.Context, mediaURL string, accessToken string) ([]byte, error)
}

// Compile-time proof that the Meta client satisfies the provider surface.
var _ Sender = (*Client)(nil)
