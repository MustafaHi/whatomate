package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// Custom endpoints: org-defined inbound URLs that start a chatbot flow.
// CRUD lives under /api/custom-endpoints (flows.chatbot permission — an
// endpoint is just a trigger bolted onto a flow); invocation is public at
// POST /api/e/{token} with the token acting as the credential.

// CustomEndpointRequest is the create/update body.
type CustomEndpointRequest struct {
	Name        string    `json:"name"`
	AccountName string    `json:"account_name"`
	FlowID      uuid.UUID `json:"flow_id"`
	IsActive    *bool     `json:"is_active,omitempty"`
}

// CustomEndpointResponse never includes the token — it is shown once, on
// create, same contract as API keys.
type CustomEndpointResponse struct {
	ID          uuid.UUID  `json:"id"`
	Name        string     `json:"name"`
	AccountName string     `json:"account_name"`
	FlowID      uuid.UUID  `json:"flow_id"`
	FlowName    string     `json:"flow_name,omitempty"`
	IsActive    bool       `json:"is_active"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	CreatedAt   string     `json:"created_at"`
}

// CustomEndpointCreateResponse adds the token + invoke path, returned only
// on create.
type CustomEndpointCreateResponse struct {
	CustomEndpointResponse
	Token     string `json:"token"`
	InvokeURL string `json:"invoke_url"` // path only; prepend your host
}

// generateEndpointToken returns a 32-char hex token for the invoke URL.
func generateEndpointToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func customEndpointToResponse(ep models.CustomEndpoint) CustomEndpointResponse {
	resp := CustomEndpointResponse{
		ID:          ep.ID,
		Name:        ep.Name,
		AccountName: ep.AccountName,
		FlowID:      ep.FlowID,
		IsActive:    ep.IsActive,
		LastUsedAt:  ep.LastUsedAt,
		CreatedAt:   ep.CreatedAt.Format(time.RFC3339),
	}
	if ep.Flow != nil {
		resp.FlowName = ep.Flow.Name
	}
	return resp
}

// validateEndpointRefs checks the account and flow exist in the org and the
// flow has a v2 graph (otherwise invoke would silently no-op).
func (a *App) validateEndpointRefs(orgID uuid.UUID, accountName string, flowID uuid.UUID) error {
	var account models.WhatsAppAccount
	if err := a.DB.Where("name = ? AND organization_id = ?", accountName, orgID).First(&account).Error; err != nil {
		return fmt.Errorf("WhatsApp account %q not found", accountName)
	}
	var flow models.ChatbotFlow
	if err := a.DB.Where("id = ? AND organization_id = ?", flowID, orgID).First(&flow).Error; err != nil {
		return fmt.Errorf("chatbot flow not found")
	}
	if _, err := parseChatGraph(flow.Graph); err != nil {
		return fmt.Errorf("flow %q has no valid v2 graph", flow.Name)
	}
	return nil
}

// ListCustomEndpoints returns all custom endpoints for the organization.
func (a *App) ListCustomEndpoints(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceFlowsChatbot, models.ActionRead)
	if err != nil {
		return nil
	}

	var endpoints []models.CustomEndpoint
	if err := a.DB.Preload("Flow").Where("organization_id = ?", orgID).
		Order("created_at DESC").Find(&endpoints).Error; err != nil {
		a.Log.Error("Failed to list custom endpoints", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list custom endpoints", nil, "")
	}

	result := make([]CustomEndpointResponse, len(endpoints))
	for i, ep := range endpoints {
		result[i] = customEndpointToResponse(ep)
	}
	return r.SendEnvelope(map[string]any{"custom_endpoints": result})
}

// CreateCustomEndpoint creates a custom endpoint and returns its token.
func (a *App) CreateCustomEndpoint(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceFlowsChatbot, models.ActionWrite)
	if err != nil {
		return nil
	}

	var req CustomEndpointRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if req.Name == "" || req.AccountName == "" || req.FlowID == uuid.Nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "name, account_name and flow_id are required", nil, "")
	}
	if err := a.validateEndpointRefs(orgID, req.AccountName, req.FlowID); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	token, err := generateEndpointToken()
	if err != nil {
		a.Log.Error("Failed to generate endpoint token", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create custom endpoint", nil, "")
	}

	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}

	ep := models.CustomEndpoint{
		OrganizationID: orgID,
		Name:           req.Name,
		Token:          token,
		AccountName:    req.AccountName,
		FlowID:         req.FlowID,
		IsActive:       isActive,
	}
	if err := a.DB.Create(&ep).Error; err != nil {
		a.Log.Error("Failed to create custom endpoint", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create custom endpoint", nil, "")
	}

	a.Log.Info("Custom endpoint created", "endpoint_id", ep.ID, "name", ep.Name, "flow_id", ep.FlowID)
	return r.SendEnvelope(CustomEndpointCreateResponse{
		CustomEndpointResponse: customEndpointToResponse(ep),
		Token:                  token,
		InvokeURL:              "/api/e/" + token,
	})
}

// UpdateCustomEndpoint updates name/account/flow/active. Changing the flow
// or account re-validates them; the token never changes (delete + recreate
// to rotate).
func (a *App) UpdateCustomEndpoint(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceFlowsChatbot, models.ActionWrite)
	if err != nil {
		return nil
	}

	epID, err := parsePathUUID(r, "id", "endpoint")
	if err != nil {
		return nil
	}

	ep, err := findByIDAndOrg[models.CustomEndpoint](a.DB, r, epID, orgID, "Custom endpoint")
	if err != nil {
		return nil
	}

	var req CustomEndpointRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	if req.Name != "" {
		ep.Name = req.Name
	}
	if req.AccountName != "" && req.AccountName != ep.AccountName {
		if err := a.validateEndpointRefs(orgID, req.AccountName, ep.FlowID); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
		}
		ep.AccountName = req.AccountName
	}
	if req.FlowID != uuid.Nil && req.FlowID != ep.FlowID {
		if err := a.validateEndpointRefs(orgID, ep.AccountName, req.FlowID); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
		}
		ep.FlowID = req.FlowID
	}
	if req.IsActive != nil {
		ep.IsActive = *req.IsActive
	}

	if err := a.DB.Save(ep).Error; err != nil {
		a.Log.Error("Failed to update custom endpoint", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update custom endpoint", nil, "")
	}

	a.Log.Info("Custom endpoint updated", "endpoint_id", ep.ID)
	return r.SendEnvelope(customEndpointToResponse(*ep))
}

// DeleteCustomEndpoint deletes a custom endpoint.
func (a *App) DeleteCustomEndpoint(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceFlowsChatbot, models.ActionDelete)
	if err != nil {
		return nil
	}

	epID, err := parsePathUUID(r, "id", "endpoint")
	if err != nil {
		return nil
	}

	result := a.DB.Where("id = ? AND organization_id = ?", epID, orgID).Delete(&models.CustomEndpoint{})
	if result.Error != nil {
		a.Log.Error("Failed to delete custom endpoint", "error", result.Error)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete custom endpoint", nil, "")
	}
	if result.RowsAffected == 0 {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Custom endpoint not found", nil, "")
	}

	a.Log.Info("Custom endpoint deleted", "endpoint_id", epID)
	return r.SendEnvelope(map[string]string{"status": "deleted"})
}

// InvokeCustomEndpoint is the public entry point: POST /api/e/{token} with
// {"phone": "...", "variables": {...}}. Auth is the token itself. Variables
// are merged into the session data so flow messages can use {{var}}.
func (a *App) InvokeCustomEndpoint(r *fastglue.Request) error {
	token := r.RequestCtx.UserValue("token").(string)

	var ep models.CustomEndpoint
	if err := a.DB.Where("token = ? AND is_active = ?", token, true).First(&ep).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Unknown endpoint", nil, "")
	}

	var req struct {
		Phone     string         `json:"phone"`
		Variables map[string]any `json:"variables"`
	}
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if req.Phone == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "phone is required", nil, "")
	}

	var account models.WhatsAppAccount
	if err := a.DB.Where("name = ? AND organization_id = ?", ep.AccountName, ep.OrganizationID).
		First(&account).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Endpoint account no longer exists", nil, "")
	}

	flow, err := a.getChatbotFlowByIDCached(ep.OrganizationID, ep.FlowID)
	if err != nil || flow == nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Endpoint flow no longer exists", nil, "")
	}
	if flow.Graph == nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Endpoint flow has no graph", nil, "")
	}

	// Find or create contact from phone number (same as SendMessage).
	var contact models.Contact
	if err := a.DB.Where("phone_number = ? AND organization_id = ?", req.Phone, ep.OrganizationID).
		First(&contact).Error; err != nil {
		contact = models.Contact{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: ep.OrganizationID,
			PhoneNumber:    req.Phone,
		}
		if err := a.DB.Create(&contact).Error; err != nil {
			a.Log.Error("Failed to create contact from endpoint", "error", err, "phone", req.Phone)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create contact", nil, "")
		}
	}

	settings, err := a.getChatbotSettingsCached(ep.OrganizationID, ep.AccountName)
	if err != nil {
		settings = &models.ChatbotSettings{}
	}

	session, _ := a.getOrCreateSession(ep.OrganizationID, contact.ID, ep.AccountName, req.Phone, settings.SessionTimeoutMins)

	// External trigger wins over an in-progress flow — same semantics as a
	// keyword trigger starting fresh.
	session.CurrentFlowID = &flow.ID
	session.CurrentStep = ""
	session.StepRetries = 0
	if session.SessionData == nil {
		session.SessionData = models.JSONB{}
	}
	for k, v := range req.Variables {
		session.SessionData[k] = v
	}
	session.SessionData["_flow_id"] = flow.ID.String()
	session.SessionData["_flow_name"] = flow.Name

	if err := a.runChatGraph(&account, &contact, session, flow, "", "", nil); err != nil {
		// The trigger succeeded; flow failures are visible in the chat UI
		// and logs, not the caller's problem.
		a.Log.Error("Custom endpoint flow run failed", "error", err, "endpoint_id", ep.ID, "flow_id", flow.ID)
	}

	now := time.Now()
	a.DB.Model(&models.CustomEndpoint{}).Where("id = ?", ep.ID).Update("last_used_at", now)

	a.Log.Info("Custom endpoint invoked", "endpoint_id", ep.ID, "session_id", session.ID)
	return r.SendEnvelope(map[string]any{"status": "accepted", "session_id": session.ID})
}
