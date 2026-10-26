package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	PrepareBillsPayment(ctx context.Context, workspaceID, by, idempotencyKey, client string, billIDs []string) (economy.PaymentRequest, bool, error)
	Bills(ctx context.Context, workspaceID string) ([]economy.Bill, error)
	PayeesNamed(ctx context.Context, workspaceID, name string) ([]economy.OutsidePayee, error)
}

// dueBill is a bill as list_bills_due shows it (B45.17): what to pay and to whom, never the bank details printed on
// it — the bill is paid to its saved payee, so an assistant has no use for them.
type dueBill struct {
	ID            string   `json:"id"`
	Supplier      string   `json:"supplier"`
	InvoiceNumber string   `json:"invoice_number,omitempty"`
	AmountMinor   int64    `json:"amount_minor"`
	Currency      string   `json:"currency"`
	DueDate       string   `json:"due_date,omitempty"`
	PayeeID       string   `json:"payee_id,omitempty"`
	PayeeMatch    string   `json:"payee_match"`
	Missing       []string `json:"missing"`
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
		{"name": "prepare_bills_payment",
			"description": "Prepares paying the workspace's bills — those named, or every bill due — as one request the person approves once with a passkey. It pays nothing. Returns the count, the total and every payee.",
			"inputSchema": map[string]any{"type": "object", "properties": with(nil, map[string]any{"bill_ids": map[string]any{"type": "array",
				"items": str, "description": "Leave out to pay every bill due."}}), "required": []string{"idempotency_key"}}},
		{"name": "list_bills_due",
			"description": "The workspace's bills that are due, soonest first: supplier, invoice number, amount, due date and whether the supplier is a saved payee, with the total per currency. Pay them with prepare_bills_payment.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
		{"name": "prepare_transfer_to_contact",
			"description": "Prepares one payment to the person's saved, confirmed contact named contact (\"Mike\") for them to approve. It pays nothing, and takes no payment details: when no contact or several match, it returns a question to ask the person instead.",
			"inputSchema": map[string]any{"type": "object", "properties": with(map[string]any{"contact": map[string]any{"type": "string", "maxLength": 140,
				"description": "The contact's name as the person said it."}, "currency": item["currency"], "amount_minor": item["amount_minor"], "reference": item["reference"]}, nil),
				"required": []string{"idempotency_key", "contact", "currency", "amount_minor"}}},
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

	if name == "list_bills_due" { // B45.17
		if agentID != "" {
			return nil, &toolRefusal{"the workspace's bills are its owner's or an admin's, not an agent's key"}
		}
		bills, err := s.paymentRequests.Bills(ctx, ws)
		if err != nil {
			return nil, err
		}
		out, totals := []dueBill{}, map[string]int64{}
		for _, b := range bills {
			if b.Status != "due" {
				continue
			}
			out = append(out, dueBill{b.ID, b.Supplier, b.InvoiceNumber, b.AmountMinor, b.Currency, b.DueDate, b.PayeeID, b.PayeeMatch, b.Missing})
			if b.Currency != "" {
				totals[b.Currency] += b.AmountMinor
			}
		}
		return map[string]any{"bills": out, "count": len(out), "totals": totals}, nil
	}
	if err := economy.RefuseAssistantPayeeDetails(raw); err != nil {
		return refused(err)
	}
	if name == "prepare_bills_payment" { // B41.102
		var a struct {
			IdempotencyKey string   `json:"idempotency_key"`
			Client         string   `json:"client"`
			BillIDs        []string `json:"bill_ids"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, &toolRefusal{"the arguments are not the tool's: " + err.Error()}
		}
		if agentID != "" {
			return nil, &toolRefusal{"the workspace's bills are paid by its owner or an admin, not an agent's key"}
		}
		client, err := economy.PaymentRequestClient(keyID, "", a.Client)
		if err != nil {
			return refused(err)
		}
		r, _, err := s.paymentRequests.PrepareBillsPayment(ctx, ws, by, a.IdempotencyKey, client, a.BillIDs)
		if err != nil {
			return refused(err)
		}
		return r, nil
	}
	if name == "prepare_transfer_to_contact" {
		return s.prepareTransferToContact(ctx, ws, by, keyID, agentID, raw, refused)
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

// prepareTransferToContact — B45.14: "Transfer $500 to Mike through Taly". The name resolves only to a saved payee
// whose name matched its account or the owner confirmed: exactly one prepares one payment for approval; none, an
// unconfirmed one or several return a question for the assistant to ask, and prepare nothing.
func (s *Server) prepareTransferToContact(ctx context.Context, ws, by, keyID, agentID string, raw json.RawMessage, refused func(error) (any, error)) (any, error) {
	var a struct {
		IdempotencyKey string `json:"idempotency_key"`
		Client         string `json:"client"`
		Contact        string `json:"contact"`
		Currency       string `json:"currency"`
		AmountMinor    int64  `json:"amount_minor"`
		Reference      string `json:"reference"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, &toolRefusal{"the arguments are not the tool's: " + err.Error()}
	}
	named, err := s.paymentRequests.PayeesNamed(ctx, ws, a.Contact)
	if errors.Is(err, economy.ErrPayee) {
		return nil, &toolRefusal{err.Error()}
	}
	if err != nil {
		return nil, err
	}
	type contact struct {
		PayeeID string `json:"payee_id"`
		Name    string `json:"name"`
	}
	confirmed, unconfirmed := []contact{}, []string{}
	for _, p := range named {
		if p.NeedsConfirmation {
			unconfirmed = append(unconfirmed, p.Name)
		} else {
			confirmed = append(confirmed, contact{p.ID, p.Name})
		}
	}
	ask := func(q string) (any, error) { return map[string]any{"question": q, "contacts": confirmed}, nil }
	switch {
	case len(confirmed) > 1:
		return ask(fmt.Sprintf("Which %s? Ask the person which saved contact they mean, then prepare_payment to that contact's payee_id.", strings.TrimSpace(a.Contact)))
	case len(confirmed) == 0 && len(unconfirmed) > 0:
		return ask(fmt.Sprintf("%s is saved but not confirmed yet: ask the person to confirm them in the app first.", strings.Join(unconfirmed, ", ")))
	case len(confirmed) == 0:
		return ask(fmt.Sprintf("No saved contact is called %q: ask the person to add them in the app. Payment details from the chat are never used.", strings.TrimSpace(a.Contact)))
	}
	client, err := economy.PaymentRequestClient(keyID, agentID, a.Client)
	if err != nil {
		return refused(err)
	}
	r, _, err := s.paymentRequests.PreparePaymentRequest(ctx, ws, by, a.IdempotencyKey, economy.PaymentRequestIn{Client: client, AgentID: agentID,
		Items: []economy.PaymentRequestItemIn{{PayeeID: confirmed[0].PayeeID, Currency: a.Currency, AmountMinor: a.AmountMinor, Reference: a.Reference}}})
	if err != nil {
		return refused(err)
	}
	return r, nil
}
