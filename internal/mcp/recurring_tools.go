package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// recurring_tools.go — B45.13: prepare_recurring_payments turns a list of daily, weekly and monthly payments to saved
// payees into standing orders and one permission mandate draft for the owner to sign. It pays nothing and nothing runs
// until the owner signs; then each payment inside the mandate pays on its own, and anything else asks the owner first.
// Only the workspace's owner or an admin prepares one: it is their money.

// RecurringPlanner prepares recurring payments (economy.DualTokenStore).
type RecurringPlanner interface {
	PrepareRecurringPayments(ctx context.Context, workspaceID, by, idempotencyKey string, in economy.RecurringPlanIn) (economy.RecurringPlan, bool, error)
}

// SetRecurringPayments offers prepare_recurring_payments; nil leaves it out.
func (s *Server) SetRecurringPayments(p RecurringPlanner) { s.recurring = p }

func recurringToolDefinitions() []map[string]any {
	str := map[string]any{"type": "string"}
	payment := map[string]any{
		"payee_id":     map[string]any{"type": "string", "description": "A saved payee's id."},
		"currency":     map[string]any{"type": "string", "enum": []string{"GBP", "EUR", "USD"}},
		"amount_minor": map[string]any{"type": "integer", "minimum": 1, "description": "In the currency's minor units: pence, cents."},
		"every":        map[string]any{"type": "string", "enum": []string{"day", "week", "month"}},
		"first_run_at": map[string]any{"type": "string", "format": "date-time", "description": "When the first payment is due; now if left out."},
		"reference":    map[string]any{"type": "string", "maxLength": 140},
	}
	return []map[string]any{{
		"name": "prepare_recurring_payments",
		"description": "Saves a list of daily, weekly and monthly payments to saved payees as standing orders, and drafts one mandate for the " +
			"owner to sign in the Talyvor app. It pays nothing. Once signed, each payment's first run waits for the owner's approval; later " +
			"runs inside the mandate pay on their own and tell the owner, and any payment above ask_above still asks.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"idempotency_key": str,
			"owner_name":      map[string]any{"type": "string", "description": "The name of the person the mandate speaks for."},
			"until":           map[string]any{"type": "string", "format": "date-time", "description": "The last day to pay; the mandate ends then."},
			"ask_above": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "integer", "minimum": 1},
				"description": "The owner's rule: by currency, the amount in minor units above which a payment always asks first."},
			"payments": map[string]any{"type": "array", "minItems": 1, "maxItems": 50, "items": map[string]any{"type": "object", "properties": payment,
				"required": []string{"payee_id", "currency", "amount_minor", "every"}}},
		}, "required": []string{"idempotency_key", "owner_name", "until", "payments"}},
	}}
}

func (s *Server) toolPrepareRecurringPayments(ctx context.Context, raw json.RawMessage) (any, error) {
	ws := effectiveWorkspace(ctx, "")
	if ws == "" {
		return nil, &toolRefusal{"recurring payments are a workspace's: use a workspace's key"}
	}
	by, owner := storedanswers.OwnerOrAdmin(ctx)
	if !owner {
		return nil, &toolRefusal{"only the workspace's owner or an admin may prepare recurring payments"}
	}
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := economy.RefuseAssistantPayeeDetails(raw); err != nil {
		return nil, &toolRefusal{err.Error()}
	}
	var a struct {
		IdempotencyKey string                       `json:"idempotency_key"`
		OwnerName      string                       `json:"owner_name"`
		Until          time.Time                    `json:"until"`
		AskAbove       map[string]int64             `json:"ask_above"`
		Payments       []economy.RecurringPaymentIn `json:"payments"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, &toolRefusal{"the arguments are not the tool's: " + err.Error()}
	}
	who := by
	if i := strings.Index(by, ":user:"); i >= 0 {
		who = strings.SplitN(by[i+len(":user:"):], ":", 2)[0]
	}
	p, _, err := s.recurring.PrepareRecurringPayments(ctx, ws, by, a.IdempotencyKey, economy.RecurringPlanIn{
		Principal: economy.MandatePrincipal{Type: "person", ID: who, Name: strings.TrimSpace(a.OwnerName)},
		Payments:  a.Payments, AskAbove: a.AskAbove, Until: a.Until})
	if errors.Is(err, economy.ErrRecurringPlan) || errors.Is(err, economy.ErrPermissionMandate) || errors.Is(err, economy.ErrPaymentRequestConflict) {
		return nil, &toolRefusal{err.Error()}
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}
