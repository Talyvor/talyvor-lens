package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// payment_request_tools.go — B40.3: an assistant prepares payments, never makes them. prepare_payment,
// prepare_batch and request_marketplace_purchase prepare a request for the person to approve, each item judged
// against their permission mandates; cancel_request and get_request_status follow it. The same gate as
// /v1/money/payment-requests: the owner or an admin, or an agent's own key as itself. No tool here moves money.

// PaymentRequester prepares, reads and cancels payment requests (economy.DualTokenStore).
type PaymentRequester interface {
	PreparePaymentRequest(ctx context.Context, workspaceID, by, idempotencyKey string, in economy.PaymentRequestIn) (economy.PaymentRequest, bool, error)
	PaymentRequest(ctx context.Context, workspaceID, agentID, id string) (economy.PaymentRequest, error)
	CancelPaymentRequest(ctx context.Context, workspaceID, agentID, id, by string) (economy.PaymentRequest, error)
}

// SetPaymentRequests offers the payment request tools; nil leaves them out.
func (s *Server) SetPaymentRequests(p PaymentRequester) { s.paymentRequests = p }

func paymentRequestToolDefinitions() []map[string]any {
	str := map[string]any{"type": "string"}
	item := map[string]any{
		"payee_id":     map[string]any{"type": "string", "description": "A saved payee's id."},
		"currency":     map[string]any{"type": "string", "enum": []string{"GBP", "EUR", "USD", "USDC"}},
		"amount_minor": map[string]any{"type": "integer", "minimum": 1, "description": "In the currency's minor units: pence, cents."},
		"reference":    map[string]any{"type": "string", "maxLength": 140},
	}
	with := func(props map[string]any, extra map[string]any) map[string]any {
		out := map[string]any{"idempotency_key": str, "client": map[string]any{"type": "string",
			"description": `"chat", "voice" or "mcp:<client id>"; an agent's key is always itself.`}}
		for k, v := range props {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	byID := map[string]any{"type": "object", "properties": map[string]any{"request_id": str}, "required": []string{"request_id"}}
	return []map[string]any{
		{"name": "prepare_payment",
			"description": "Prepares one payment to a saved payee for the person to approve. It pays nothing. Returns the request's status, its items' decisions and the approval link.",
			"inputSchema": map[string]any{"type": "object", "properties": with(item, nil),
				"required": []string{"idempotency_key", "payee_id", "currency", "amount_minor"}}},
		{"name": "prepare_batch",
			"description": "Prepares several payments to saved payees as one request for the person to approve. It pays nothing.",
			"inputSchema": map[string]any{"type": "object", "properties": with(nil, map[string]any{"items": map[string]any{"type": "array", "minItems": 1,
				"items": map[string]any{"type": "object", "properties": item, "required": []string{"payee_id", "currency", "amount_minor"}}}}),
				"required": []string{"idempotency_key", "items"}}},
		{"name": "request_marketplace_purchase",
			"description": "Prepares the purchase of a marketplace listing at its price for the person to approve. It pays nothing.",
			"inputSchema": map[string]any{"type": "object", "properties": with(nil, map[string]any{"listing_id": str, "reference": item["reference"]}),
				"required": []string{"idempotency_key", "listing_id"}}},
		{"name": "cancel_request", "description": "Cancels a prepared payment request.", "inputSchema": byID},
		{"name": "get_request_status", "description": "A prepared payment request: its status, items, decisions and approval link.", "inputSchema": byID},
	}
}

func (s *Server) callPaymentRequestTool(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	ws := effectiveWorkspace(ctx, "")
	if ws == "" {
		return nil, &toolRefusal{"payment requests are a workspace's: use a workspace's key"}
	}
	keyID, agentID := "", ""
	if actx := auth.GetAuthContext(ctx); actx != nil {
		keyID = actx.APIKeyID
	}
	by, owner := storedanswers.OwnerOrAdmin(ctx)
	if !owner {
		if keyID != "" && s.agentBank != nil {
			a, _, err := s.agentBank.AgentOfKey(ctx, keyID)
			if err != nil && !errors.Is(err, economy.ErrAgentNotFound) {
				return nil, err
			}
			agentID = a
		}
		if agentID == "" {
			return nil, &toolRefusal{"only the workspace's owner, an admin or an agent's own key may prepare payment requests"}
		}
		by = "agent:" + agentID
	}
	refused := func(err error) (any, error) {
		if errors.Is(err, economy.ErrPaymentRequest) || errors.Is(err, economy.ErrPaymentRequestConflict) || errors.Is(err, economy.ErrPaymentRequestNotFound) {
			return nil, &toolRefusal{err.Error()}
		}
		return nil, err
	}
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if name == "cancel_request" || name == "get_request_status" {
		var a struct {
			RequestID string `json:"request_id"`
		}
		_ = json.Unmarshal(raw, &a)
		var r economy.PaymentRequest
		var err error
		if name == "cancel_request" {
			r, err = s.paymentRequests.CancelPaymentRequest(ctx, ws, agentID, a.RequestID, by)
		} else {
			r, err = s.paymentRequests.PaymentRequest(ctx, ws, agentID, a.RequestID)
		}
		if err != nil {
			return refused(err)
		}
		return r, nil
	}

	if err := economy.RefuseAssistantPayeeDetails(raw); err != nil {
		return refused(err)
	}
	var a struct {
		IdempotencyKey string                         `json:"idempotency_key"`
		Client         string                         `json:"client"`
		Items          []economy.PaymentRequestItemIn `json:"items"`
		economy.PaymentRequestItemIn
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, &toolRefusal{"the arguments are not the tool's: " + err.Error()}
	}
	in := economy.PaymentRequestIn{AgentID: agentID, Items: a.Items}
	if name != "prepare_batch" {
		if name == "prepare_payment" {
			a.ListingID = ""
		} else {
			a.PaymentRequestItemIn = economy.PaymentRequestItemIn{ListingID: a.ListingID, Reference: a.Reference}
		}
		in.Items = []economy.PaymentRequestItemIn{a.PaymentRequestItemIn}
	}
	var err error
	if in.Client, err = economy.PaymentRequestClient(keyID, agentID, a.Client); err != nil {
		return refused(err)
	}
	r, _, err := s.paymentRequests.PreparePaymentRequest(ctx, ws, by, a.IdempotencyKey, in)
	if err != nil {
		return refused(err)
	}
	return r, nil
}
