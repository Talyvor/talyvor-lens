package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
)

// instant_tools.go — B59.18: THE CONNECTOR'S instant_transfer_quote AND instant_transfer_send (launch pack 4.5).
//
// An agent's own key quotes an Instant transfer to a Talyvor account and sends against the quote. A send through the
// connector only prepares the payment for the owner to approve: it is judged by the agent's rules as the HTTP route
// judges it, and then it always waits for the owner's signed approval, whatever its rules allow, so the first send
// leaves an approval and moves nothing. Once the owner has approved it, the same send again — at a new quote, the
// first one's 30 seconds will be over — goes through once.
//
// Only a hybrid workspace holds both sides of an Instant transfer, so a fiat-only or stablecoin-only workspace is not
// offered either tool: tools/list leaves them out and tools/call answers unknown tool. The operator's admin key sees
// them, as it sees every tool. Refusals are fields: a missing rule is {"code": "agent_rule", "field": ...}, a workspace
// of the wrong kind {"code": "wrong_money_kind", ...}.

// InstantTransferer is what the Instant transfer tools need of the economy store.
type InstantTransferer interface {
	WorkspaceMoneyKind(ctx context.Context, workspaceID string) (string, error)
	QuoteInstantTransfer(ctx context.Context, workspaceID string, in economy.InstantQuoteRequest) (economy.InstantQuote, error)
	SendInstantTransfer(ctx context.Context, workspaceID, agentID string, in economy.InstantSend) (economy.InstantTransfer, error)
}

// SetInstantTransfers offers the Instant transfer tools to a hybrid workspace's agents; nil leaves them out.
func (s *Server) SetInstantTransfers(t InstantTransferer) { s.instant = t }

func isInstantTool(name string) bool { return strings.HasPrefix(name, "instant_transfer_") }

func instantToolDefinitions() []map[string]any {
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	fiat := []string{economy.CurrencyGBP, economy.CurrencyEUR, economy.CurrencyUSD}
	return []map[string]any{
		{"name": "instant_transfer_quote",
			"description": "Price an Instant transfer from your account to a Talyvor account: what you send, the fee, the rate, what they get, " +
				"and whether it arrives in seconds. The quote is locked for 30 seconds; send it with instant_transfer_send.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"send_minor":       map[string]any{"type": "integer", "minimum": 1, "description": "what you send, the fee included, in pence or cents"},
				"send_currency":    map[string]any{"type": "string", "enum": fiat},
				"receive_currency": map[string]any{"type": "string", "enum": append(fiat, economy.CurrencyUSDC), "description": "the currency the Talyvor account holds"},
			}, "required": []string{"send_minor", "send_currency", "receive_currency"}}},
		{"name": "instant_transfer_send",
			"description": "Prepare an Instant transfer at a quote from instant_transfer_quote for the owner to approve. It moves nothing: it returns " +
				"the approval waiting for the owner. Once the owner has approved it, send it again with the same idempotency_key and a new quote_id; " +
				"it is sent once.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"quote_id":        str("the quote, from instant_transfer_quote"),
				"to_account_id":   str("the Talyvor account it goes to"),
				"idempotency_key": str(idempotencyKeyDescription),
			}, "required": []string{"quote_id", "to_account_id", "idempotency_key"}}},
	}
}

// instantOpen is whether the caller is offered the Instant transfer tools: the admin key, or a hybrid workspace's.
func (s *Server) instantOpen(ctx context.Context) (bool, error) {
	ws, isAdmin := auth.WorkspaceIdentity(ctx)
	switch {
	case s.instant == nil:
		return false, nil
	case isAdmin:
		return true, nil
	case ws == "":
		return false, nil
	}
	kind, err := s.instant.WorkspaceMoneyKind(ctx, ws)
	if err != nil {
		return false, err
	}
	return kind == economy.MoneyKindHybrid, nil
}

// withoutInstant leaves the Instant transfer tools out of tools.
func withoutInstant(tools []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if name, _ := t["name"].(string); !isInstantTool(name) {
			out = append(out, t)
		}
	}
	return out
}

// instantRefusal is a refusal the agent reads as fields.
func instantRefusal(fields map[string]any) error {
	raw, _ := json.Marshal(fields)
	return &toolRefusal{string(raw)}
}

func (s *Server) runInstantTool(ctx context.Context, name, workspaceID, agentID string, raw json.RawMessage) (any, error) {
	if s.instant == nil {
		return nil, errors.New("the Instant transfer tools are not configured")
	}
	var a struct {
		SendMinor       int64  `json:"send_minor"`
		SendCurrency    string `json:"send_currency"`
		ReceiveCurrency string `json:"receive_currency"`
		QuoteID         string `json:"quote_id"`
		ToAccountID     string `json:"to_account_id"`
		Key             string `json:"idempotency_key"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, &toolRefusal{"invalid arguments: " + err.Error()}
		}
	}
	var res any
	var err error
	if name == "instant_transfer_quote" {
		res, err = s.instant.QuoteInstantTransfer(ctx, workspaceID, economy.InstantQuoteRequest{SendMinor: a.SendMinor,
			SendCurrency: a.SendCurrency, ReceiveCurrency: a.ReceiveCurrency, To: economy.InstantToAccount})
	} else {
		// Whatever the agent's rules allow, a send through the connector waits for the owner's approval.
		res, err = s.instant.SendInstantTransfer(economy.WithConnectPrepareOnly(ctx), workspaceID, agentID,
			economy.InstantSend{QuoteID: a.QuoteID, ToAccountID: a.ToAccountID, IdempotencyKey: a.Key})
	}
	var need *economy.ApprovalNeededError
	var rule *economy.InstantRuleRefusal
	var kind *economy.MoneyKindRefusal
	switch {
	case err == nil:
		return res, nil
	case errors.As(err, &need):
		return map[string]any{"status": "awaiting_approval", "approval_id": need.ApprovalID,
			"message": "Prepared for the owner to approve. Nothing has moved."}, nil
	case errors.As(err, &rule):
		return nil, instantRefusal(map[string]any{"code": "agent_rule", "field": rule.Field, "error": rule.Error()})
	case errors.As(err, &kind):
		return nil, instantRefusal(map[string]any{"code": "wrong_money_kind", "field": kind.Field, "money_kind": kind.MoneyKind, "error": kind.Error()})
	case errors.Is(err, economy.ErrInstantTransfer), errors.Is(err, money.ErrInvalid), errors.Is(err, economy.ErrConnectApprovalsWaiting),
		errors.Is(err, economy.ErrApprovalDeclined):
		return nil, &toolRefusal{err.Error()}
	}
	for _, e := range walletRefusals {
		if errors.Is(err, e) {
			return nil, &toolRefusal{err.Error()}
		}
	}
	return nil, err
}
