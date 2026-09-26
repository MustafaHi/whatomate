package handlers

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// TestInvokeCustomEndpoint_StartsFlow exercises the public invoke path end
// to end: token lookup, contact get-or-create, session start at the bound
// flow, and variable injection into session data. The graph is start → end
// (no message nodes) so nothing is sent.
func TestInvokeCustomEndpoint_StartsFlow(t *testing.T) {
	app, org, account, _, _ := newGraphTestFixtures(t)

	flow := &models.ChatbotFlow{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		WhatsAppAccount: account.Name,
		Name:            "endpoint-flow",
		IsEnabled:       true,
		Graph: models.JSONB{
			"version":    2,
			"entry_node": "s1",
			"nodes": []any{
				map[string]any{"id": "s1", "type": "start", "config": map[string]any{}},
				map[string]any{"id": "e1", "type": "end", "config": map[string]any{}},
			},
			"edges": []any{
				map[string]any{"from": "s1", "to": "e1", "condition": "default"},
			},
		},
	}
	require.NoError(t, app.DB.Create(flow).Error)

	endpoint := &models.CustomEndpoint{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "order-shipped",
		Token:          "tok-" + uuid.New().String(),
		AccountName:    account.Name,
		FlowID:         flow.ID,
		IsActive:       true,
	}
	require.NoError(t, app.DB.Create(endpoint).Error)

	req := testutil.NewJSONRequest(t, map[string]any{
		"phone": "+15550001111",
		"variables": map[string]any{
			"order_id": "12345",
		},
	})
	testutil.SetPathParam(req, "token", endpoint.Token)
	require.NoError(t, app.InvokeCustomEndpoint(req))

	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	// Contact was created from the phone number.
	var contact models.Contact
	require.NoError(t, app.DB.Where("phone_number = ? AND organization_id = ?", "+15550001111", org.ID).First(&contact).Error)

	// Session started at the bound flow with variables merged in, and the
	// graph ran to completion (start → end).
	var session models.ChatbotSession
	require.NoError(t, app.DB.Where("contact_id = ?", contact.ID).First(&session).Error)
	require.NotNil(t, session.CurrentFlowID)
	assert.Equal(t, flow.ID, *session.CurrentFlowID)
	assert.Equal(t, models.SessionStatusCompleted, session.Status)
	assert.Equal(t, "12345", session.SessionData["order_id"])

	// LastUsedAt was stamped.
	var updated models.CustomEndpoint
	require.NoError(t, app.DB.First(&updated, endpoint.ID).Error)
	require.NotNil(t, updated.LastUsedAt)
}

// TestInvokeCustomEndpoint_RejectsBadTokenAndInput covers the guards:
// unknown token, inactive endpoint, and missing phone.
func TestInvokeCustomEndpoint_RejectsBadTokenAndInput(t *testing.T) {
	app, org, account, _, _ := newGraphTestFixtures(t)

	endpoint := &models.CustomEndpoint{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "off",
		Token:          "tok-" + uuid.New().String(),
		AccountName:    account.Name,
		FlowID:         uuid.New(),
		IsActive:       false,
	}
	require.NoError(t, app.DB.Create(endpoint).Error)

	// Unknown token → 404.
	req := testutil.NewJSONRequest(t, map[string]any{"phone": "+15550002222"})
	testutil.SetPathParam(req, "token", "no-such-token")
	require.NoError(t, app.InvokeCustomEndpoint(req))
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))

	// Inactive endpoint → 404 (don't reveal it exists).
	req = testutil.NewJSONRequest(t, map[string]any{"phone": "+15550002222"})
	testutil.SetPathParam(req, "token", endpoint.Token)
	require.NoError(t, app.InvokeCustomEndpoint(req))
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))

	// Active endpoint, missing phone → 400.
	endpoint.IsActive = true
	require.NoError(t, app.DB.Save(endpoint).Error)
	req = testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetPathParam(req, "token", endpoint.Token)
	require.NoError(t, app.InvokeCustomEndpoint(req))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}
