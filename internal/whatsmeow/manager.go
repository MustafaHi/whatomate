package whatsmeow

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	qrcode "github.com/skip2/go-qrcode"
	"github.com/zerodha/logf"
	wa "go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"gorm.io/gorm"
)

// deviceInfo is the provider_data JSON persisted on the account row.
type deviceInfo struct {
	JID string `json:"jid"` // own device JID, e.g. "1234567890:24@s.whatsapp.net"
}

// pairing tracks an in-flight QR pairing.
type pairing struct {
	status string // starting|qr|paired|timeout|error
	qr     string
	err    string
	cancel context.CancelFunc
}

// Manager owns all whatsmeow sessions and resolves Sender implementations for
// whatsmeow accounts. It satisfies handlers.MeowResolver.
type Manager struct {
	ctx    context.Context
	cancel context.CancelFunc
	DB     *gorm.DB
	sqlDB  *sql.DB
	Log    logf.Logger

	container *sqlstore.Container

	mu       sync.Mutex
	sessions map[uuid.UUID]*session // by account ID
	pairings map[uuid.UUID]*pairing // by account ID

	onInbound func(phoneID string, msg InboundMessage)
	onStatus  func(phoneID string, st StatusUpdate)
}

// New creates a Manager; call SetHandlers, then Start (usually in a goroutine).
func New(ctx context.Context, db *gorm.DB, log logf.Logger) (*Manager, error) {
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to get underlying sql.DB: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	m := &Manager{
		ctx:      ctx,
		cancel:   cancel,
		DB:       db,
		sqlDB:    sqlDB,
		Log:      log,
		sessions: make(map[uuid.UUID]*session),
		pairings: make(map[uuid.UUID]*pairing),
	}
	m.container = sqlstore.NewWithDB(sqlDB, "postgres", waLog.Stdout("whatsmeow-store", "WARN", false))
	return m, nil
}

// SetHandlers wires the inbound/status callbacks into the handlers layer.
func (m *Manager) SetHandlers(onInbound func(phoneID string, msg InboundMessage), onStatus func(phoneID string, st StatusUpdate)) {
	m.onInbound = onInbound
	m.onStatus = onStatus
}

// Start upgrades the whatsmeow store tables and connects every paired
// whatsmeow account. Unpaired accounts wait for StartPairing.
func (m *Manager) Start() error {
	if err := m.container.Upgrade(m.ctx); err != nil {
		return fmt.Errorf("failed to upgrade whatsmeow store: %w", err)
	}

	var accounts []models.WhatsAppAccount
	if err := m.DB.Where("provider = ?", whatsapp.ProviderWhatsmeow).Find(&accounts).Error; err != nil {
		return fmt.Errorf("failed to load whatsmeow accounts: %w", err)
	}
	for i := range accounts {
		m.connectStored(&accounts[i])
	}
	return nil
}

// Stop disconnects everything. Connections are snapshotted under the lock and
// torn down outside it — Disconnect does network I/O.
func (m *Manager) Stop() {
	m.cancel()
	m.mu.Lock()
	sessions := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	pairings := make([]*pairing, 0, len(m.pairings))
	for _, p := range m.pairings {
		pairings = append(pairings, p)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.client.Disconnect()
	}
	for _, p := range pairings {
		p.cancel()
	}
}

func (m *Manager) connectStored(account *models.WhatsAppAccount) {
	var info deviceInfo
	if account.ProviderData == "" || json.Unmarshal([]byte(account.ProviderData), &info) != nil || info.JID == "" {
		return // unpaired — waiting for QR
	}
	jid, err := types.ParseJID(info.JID)
	if err != nil {
		m.Log.Error("Invalid stored whatsmeow JID", "account", account.Name, "jid", info.JID, "error", err)
		return
	}
	device, err := m.container.GetDevice(m.ctx, jid)
	if err != nil || device == nil {
		m.Log.Warn("whatsmeow device not found in store", "account", account.Name, "jid", info.JID)
		return
	}

	s := &session{
		mgr:       m,
		accountID: account.ID,
		phoneID:   NormalizeDigits(jid.User),
		client:    wa.NewClient(device, waLog.Stdout("whatsmeow", "INFO", false)),
		outbound:  make(map[string]outboundEntry),
		recvIndex: make(map[string]recvEntry),
	}
	s.client.AddEventHandler(s.dispatch)
	if err := s.client.Connect(); err != nil {
		m.Log.Error("Failed to connect whatsmeow session", "account", account.Name, "error", err)
		return
	}

	m.mu.Lock()
	m.sessions[account.ID] = s
	m.mu.Unlock()
	m.Log.Info("whatsmeow session connecting", "account", account.Name, "phone", s.phoneID)
}

// StartPairing begins a QR pairing for an unpaired (or re-pairing) account.
// With force=true it also tears down a live session; without it, a connected
// account is refused so an accidental click can't unlink a working number.
func (m *Manager) StartPairing(account *models.WhatsAppAccount, force bool) error {
	if !force && m.IsConnected(account) {
		return fmt.Errorf("account is already connected; re-pairing would unlink it — pass force to confirm")
	}

	// Fresh session keys: drop any existing device for this account first.
	m.Forget(account)

	m.mu.Lock()
	if p, exists := m.pairings[account.ID]; exists {
		if p.status == "starting" || p.status == "qr" {
			m.mu.Unlock()
			return fmt.Errorf("pairing already in progress")
		}
		// Terminal state (paired/timeout/error) — safe to replace.
		p.cancel()
	}
	pctx, cancel := context.WithCancel(m.ctx)
	p := &pairing{status: "starting", cancel: cancel}
	m.pairings[account.ID] = p
	m.mu.Unlock()

	go m.runPairing(pctx, p, account)
	return nil
}

func (m *Manager) runPairing(ctx context.Context, p *pairing, account *models.WhatsAppAccount) {
	defer p.cancel()
	// Terminal states stay in m.pairings for the status poller; the slot is
	// replaced on the next StartPairing for this account.

	device := m.container.NewDevice()
	client := wa.NewClient(device, waLog.Stdout("whatsmeow", "INFO", false))

	qrChan, err := client.GetQRChannel(ctx)
	if err != nil {
		m.finishPairing(account.ID, p, "error", "", fmt.Errorf("failed to start QR channel: %w", err))
		return
	}
	if err := client.Connect(); err != nil {
		m.finishPairing(account.ID, p, "error", "", fmt.Errorf("failed to connect for pairing: %w", err))
		return
	}

	for item := range qrChan {
		switch {
		case item.Event == "code":
			m.setPairing(account.ID, p, "qr", item.Code, "")
		case item.Event == "success":
			if client.Store.ID == nil {
				m.finishPairing(account.ID, p, "error", "", fmt.Errorf("paired but no device identity stored"))
				return
			}
			jid := client.Store.ID.String()
			info, _ := json.Marshal(deviceInfo{JID: jid})
			phone := NormalizeDigits(client.Store.ID.User)
			updates := map[string]any{
				"provider_data": string(info),
				"phone_id":      phone,
				"status":        "active",
			}
			if err := m.DB.Model(&models.WhatsAppAccount{}).Where("id = ?", account.ID).Updates(updates).Error; err != nil {
				m.finishPairing(account.ID, p, "error", "", fmt.Errorf("failed to persist pairing: %w", err))
				return
			}
			account.ProviderData = string(info)
			account.PhoneID = phone

			s := &session{
				mgr:       m,
				accountID: account.ID,
				phoneID:   phone,
				client:    client,
				outbound:  make(map[string]outboundEntry),
				recvIndex: make(map[string]recvEntry),
			}
			// connectStored registers on the restart path; the fresh-pair
			// path must too, or inbound events are dispatched to nobody.
			client.AddEventHandler(s.dispatch)
			m.mu.Lock()
			m.sessions[account.ID] = s
			m.mu.Unlock()
			m.Log.Info("whatsmeow account paired", "account", account.Name, "phone", phone)
			m.finishPairing(account.ID, p, "paired", "", nil)
			return
		case item.Event == "timeout":
			m.finishPairing(account.ID, p, "timeout", "", nil)
			return
		case strings.HasPrefix(item.Event, "err"):
			msg := ""
			if item.Error != nil {
				msg = item.Error.Error()
			}
			m.finishPairing(account.ID, p, "error", "", fmt.Errorf("pairing failed: %s", msg))
			return
		}
	}
	m.finishPairing(account.ID, p, "error", "", fmt.Errorf("QR channel closed unexpectedly"))
}

func (m *Manager) setPairing(accountID uuid.UUID, p *pairing, status, qr, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p.status, p.qr, p.err = status, qr, errMsg
}

func (m *Manager) finishPairing(accountID uuid.UUID, p *pairing, status, qr string, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
		m.Log.Error("whatsmeow pairing failed", "account_id", accountID, "error", msg)
	}
	m.setPairing(accountID, p, status, qr, msg)
}

// PairingStatus returns the pairing status and the current QR code rendered as
// a PNG data URL (empty when not in "qr" state). Fields are copied under the
// lock — runPairing mutates them concurrently.
func (m *Manager) PairingStatus(accountID uuid.UUID) (string, string, string) {
	m.mu.Lock()
	p, ok := m.pairings[accountID]
	var status, qr, errMsg string
	if ok {
		status, qr, errMsg = p.status, p.qr, p.err
	}
	m.mu.Unlock()
	if !ok {
		return "", "", ""
	}
	qrPNG := ""
	if status == "qr" && qr != "" {
		png, err := qrcode.Encode(qr, qrcode.Medium, 256)
		if err == nil {
			qrPNG = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
		}
	}
	return status, qrPNG, errMsg
}

// SenderFor resolves the Sender for an account value struct. Returns an
// unsupported-error sender when the account has no live session.
func (m *Manager) SenderFor(account *whatsapp.Account) whatsapp.Sender {
	digits := NormalizeDigits(account.PhoneID)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.phoneID == digits {
			return s
		}
	}
	return errSender{}
}

// IsConnected reports whether the account has a live session.
func (m *Manager) IsConnected(account *models.WhatsAppAccount) bool {
	m.mu.Lock()
	s, ok := m.sessions[account.ID]
	m.mu.Unlock()
	return ok && s.client != nil && s.client.IsConnected() && s.client.IsLoggedIn()
}

// Disconnect drops the connection but keeps the stored session keys.
func (m *Manager) Disconnect(account *models.WhatsAppAccount) {
	m.mu.Lock()
	s := m.sessions[account.ID]
	delete(m.sessions, account.ID)
	m.mu.Unlock()
	if s != nil {
		s.client.Disconnect()
	}
}

// Forget tears down the session and deletes the stored device identity.
func (m *Manager) Forget(account *models.WhatsAppAccount) {
	m.Disconnect(account)

	var info deviceInfo
	if account.ProviderData == "" || json.Unmarshal([]byte(account.ProviderData), &info) != nil || info.JID == "" {
		return
	}
	if jid, err := types.ParseJID(info.JID); err == nil {
		if device, err := m.container.GetDevice(m.ctx, jid); err == nil && device != nil {
			if err := m.container.DeleteDevice(m.ctx, device); err != nil {
				m.Log.Error("Failed to delete whatsmeow device", "account", account.Name, "error", err)
			}
		}
	}
	if err := m.DB.Model(&models.WhatsAppAccount{}).Where("id = ?", account.ID).
		Updates(map[string]any{"provider_data": "", "phone_id": ""}).Error; err != nil {
		m.Log.Error("Failed to clear whatsmeow pairing data", "account", account.Name, "error", err)
	}
	account.ProviderData = ""
	account.PhoneID = ""
}

// errSender is the placeholder Sender for whatsmeow accounts without a live
// session — every operation fails with a clear error.
type errSender struct {
	unsupportedSender
}

func (errSender) SendTextMessage(_ context.Context, _ *whatsapp.Account, _ whatsapp.Recipient, _ string, _ ...string) (string, error) {
	return "", errSenderNotPaired()
}

func (errSender) SendImageMessage(_ context.Context, _ *whatsapp.Account, _ whatsapp.Recipient, _, _ string) (string, error) {
	return "", errSenderNotPaired()
}

func (errSender) SendDocumentMessage(_ context.Context, _ *whatsapp.Account, _ whatsapp.Recipient, _, _, _ string) (string, error) {
	return "", errSenderNotPaired()
}

func (errSender) SendVideoMessage(_ context.Context, _ *whatsapp.Account, _ whatsapp.Recipient, _, _ string) (string, error) {
	return "", errSenderNotPaired()
}

func (errSender) SendAudioMessage(_ context.Context, _ *whatsapp.Account, _ whatsapp.Recipient, _ string) (string, error) {
	return "", errSenderNotPaired()
}

func (errSender) MarkMessageRead(_ context.Context, _ *whatsapp.Account, _ string) error {
	return errSenderNotPaired()
}

func (errSender) UploadMedia(_ context.Context, _ *whatsapp.Account, _ []byte, _, _ string) (string, error) {
	return "", errSenderNotPaired()
}

func errSenderNotPaired() error {
	return fmt.Errorf("%w: account is not paired or connected — scan the QR code in the account settings", whatsapp.ErrUnsupported)
}
