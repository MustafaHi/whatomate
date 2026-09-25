package handlers

import (
	"strconv"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/whatsmeow"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// Compile-time: the whatsmeow manager satisfies the handlers-side resolver.
var _ MeowResolver = (*whatsmeow.Manager)(nil)

// ============================================================================
// whatsmeow (QR-linked provider) — inbound funnel + pairing endpoints
// ============================================================================

// HandleMeowInbound adapts a whatsmeow inbound message into the Meta-webhook
// pipeline: save media locally, stage it under a synthetic ID, then reuse
// processIncomingMessage unchanged.
func (a *App) HandleMeowInbound(phoneID string, msg whatsmeow.InboundMessage) {
	// The lookup validates the account exists and primes the phone_id cache.
	if _, err := a.getWhatsAppAccountCached(phoneID); err != nil {
		a.Log.Error("WhatsApp account not found for whatsmeow message", "phone_id", phoneID, "error", err)
		return
	}

	itm := IncomingTextMessage{
		From:      msg.From,
		ID:        msg.ID,
		Timestamp: strconv.FormatInt(msg.Timestamp.Unix(), 10),
		Type:      msg.Type,
	}

	if msg.ContextID != "" {
		itm.Context = &struct {
			From string `json:"from"`
			ID   string `json:"id"`
		}{From: msg.From, ID: msg.ContextID}
	}

	// whatsmeow hands over decrypted media bytes, while the shared extraction
	// path expects a media ID it can download — so persist the blob now and
	// stage it under a synthetic ID for DownloadAndSaveMedia to pick up.
	// saveMediaLocally derives the extension from the mime type.
	syntheticID := ""
	if len(msg.MediaData) > 0 {
		filename := msg.Filename
		if filename == "" {
			filename = "media"
		}
		if localPath, err := a.saveMediaLocally(msg.MediaData, msg.MimeType, filename); err == nil {
			syntheticID = "meow:" + uuid.New().String()
			a.StageMedia(syntheticID, localPath)
		} else {
			a.Log.Error("Failed to save whatsmeow media", "error", err, "message_id", msg.ID)
		}
	}

	switch msg.Type {
	case "image":
		itm.Image = &struct {
			ID       string `json:"id"`
			MimeType string `json:"mime_type"`
			SHA256   string `json:"sha256"`
			Caption  string `json:"caption,omitempty"`
		}{ID: syntheticID, MimeType: msg.MimeType, Caption: msg.Text}
	case "video":
		itm.Video = &struct {
			ID       string `json:"id"`
			MimeType string `json:"mime_type"`
			SHA256   string `json:"sha256"`
			Caption  string `json:"caption,omitempty"`
		}{ID: syntheticID, MimeType: msg.MimeType, Caption: msg.Text}
	case "audio":
		itm.Audio = &struct {
			ID       string `json:"id"`
			MimeType string `json:"mime_type"`
		}{ID: syntheticID, MimeType: msg.MimeType}
	case "document":
		itm.Document = &struct {
			ID       string `json:"id"`
			MimeType string `json:"mime_type"`
			SHA256   string `json:"sha256"`
			Filename string `json:"filename,omitempty"`
			Caption  string `json:"caption,omitempty"`
		}{ID: syntheticID, MimeType: msg.MimeType, Filename: msg.Filename, Caption: msg.Text}
	case "sticker":
		itm.Sticker = &struct {
			ID       string `json:"id"`
			MimeType string `json:"mime_type"`
			SHA256   string `json:"sha256"`
			Animated bool   `json:"animated,omitempty"`
		}{ID: syntheticID, MimeType: msg.MimeType}
	case "reaction":
		itm.Reaction = &struct {
			MessageID string `json:"message_id"`
			Emoji     string `json:"emoji"`
		}{MessageID: msg.ReactionTarget, Emoji: msg.ReactionEmoji}
	case "location":
		itm.Location = &struct {
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
			Name      string  `json:"name,omitempty"`
			Address   string  `json:"address,omitempty"`
		}{Latitude: msg.Lat, Longitude: msg.Long, Name: msg.PlaceName, Address: msg.PlaceAddress}
	}

	a.processIncomingMessage(phoneID, itm, msg.PushName)
}

// HandleMeowStatus adapts a whatsmeow receipt into the status pipeline.
func (a *App) HandleMeowStatus(phoneID string, st whatsmeow.StatusUpdate) {
	a.processStatusUpdate(phoneID, WebhookStatus{ID: st.MessageID, Status: st.Status})
}

// MeowPairStart starts a QR pairing session for a whatsmeow account.
func (a *App) MeowPairStart(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceAccounts, models.ActionWrite)
	if err != nil {
		return nil
	}
	id, err := parsePathUUID(r, "id", "account")
	if err != nil {
		return nil
	}
	account, err := findByIDAndOrg[models.WhatsAppAccount](a.DB, r, id, orgID, "Account")
	if err != nil {
		return nil
	}
	if account.Provider != "whatsmeow" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Account is not a whatsmeow account", nil, "")
	}
	if a.Meow == nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "whatsmeow support unavailable", nil, "")
	}

	var req struct {
		Force bool `json:"force"` // required to re-pair a currently-connected account
	}
	_ = r.Decode(&req, "json")

	// Re-pairing changes the number — drop any cached copy of the old one.
	a.InvalidateWhatsAppAccountCache(account.PhoneID)
	if err := a.Meow.StartPairing(account, req.Force); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	return r.SendEnvelope(map[string]string{"status": "starting"})
}

// MeowPairStatus polls pairing progress; qr_png carries the QR as a data URL.
func (a *App) MeowPairStatus(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceAccounts, models.ActionRead)
	if err != nil {
		return nil
	}
	id, err := parsePathUUID(r, "id", "account")
	if err != nil {
		return nil
	}
	if _, err := findByIDAndOrg[models.WhatsAppAccount](a.DB, r, id, orgID, "Account"); err != nil {
		return nil
	}
	if a.Meow == nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "whatsmeow support unavailable", nil, "")
	}
	status, qrPNG, errMsg := a.Meow.PairingStatus(id)
	resp := map[string]any{"status": status}
	if qrPNG != "" {
		resp["qr_png"] = qrPNG
	}
	if errMsg != "" {
		resp["error"] = errMsg
	}
	return r.SendEnvelope(resp)
}

// MeowConnection reports the live connection state of a whatsmeow account.
func (a *App) MeowConnection(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceAccounts, models.ActionRead)
	if err != nil {
		return nil
	}
	id, err := parsePathUUID(r, "id", "account")
	if err != nil {
		return nil
	}
	account, err := findByIDAndOrg[models.WhatsAppAccount](a.DB, r, id, orgID, "Account")
	if err != nil {
		return nil
	}
	if account.Provider != "whatsmeow" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Account is not a whatsmeow account", nil, "")
	}
	connected := a.Meow != nil && a.Meow.IsConnected(account)
	return r.SendEnvelope(map[string]any{"connected": connected})
}
