package economy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// assistant_limits.go — B41.114: limits per assistant. The person caps what each client — Claude or ChatGPT through
// the connector ("mcp:<client id>"), voice, Talyvor Chat, an agent's own key ("key:<id>") — may prepare in payment
// requests in a UTC day, in each currency. A request that would take its client over is refused whole, before
// anything is kept; every other client is judged as before. What counts is every item not blocked, of every request
// that client prepared today and nobody cancelled. The cap is on top of the mandates and approvals, never instead of
// them: raising it lets more be prepared, and each payment still needs its mandate or the person's approval.
// Class: rules_approvals_statements_pots, GREEN — it refuses requests and moves nothing.

// ErrAssistantLimit: the request would take its client over its daily limit.
var ErrAssistantLimit = fmt.Errorf("%w: over the client's daily limit", ErrPaymentRequest)

// AssistantLimit is one client's daily limit in one currency, and what it has prepared today.
type AssistantLimit struct {
	Client             string    `json:"client"`
	Currency           string    `json:"currency"`
	PerDayMinor        int64     `json:"per_day_minor"`
	PreparedTodayMinor int64     `json:"prepared_today_minor"`
	UpdatedBy          string    `json:"updated_by"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// limitsSQL is each limit l of workspace $1 with what its client prepared today in its currency.
const limitsSQL = `SELECT l.client, l.currency, l.per_day_minor, (SELECT COALESCE(sum(i.amount_minor), 0)
	FROM payment_request_items i JOIN payment_requests r ON r.id = i.request_id
	WHERE r.workspace_id = l.workspace_id AND r.client = l.client AND i.currency = l.currency AND i.decision <> 'blocked' AND r.status <> 'cancelled'
	AND r.created_at >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'),
	l.updated_by, l.updated_at FROM assistant_limits l WHERE l.workspace_id = $1`

func scanAssistantLimit(r pgx.CollectableRow) (AssistantLimit, error) {
	var l AssistantLimit
	return l, r.Scan(&l.Client, &l.Currency, &l.PerDayMinor, &l.PreparedTodayMinor, &l.UpdatedBy, &l.UpdatedAt)
}

// AssistantLimits lists the workspace's limits, by client then currency.
func (s *DualTokenStore) AssistantLimits(ctx context.Context, workspaceID string) ([]AssistantLimit, error) {
	rows, err := s.pool.Query(ctx, limitsSQL+` ORDER BY l.client, l.currency`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: assistant limits: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanAssistantLimit)
	if err != nil {
		return nil, fmt.Errorf("economy: assistant limits: %w", err)
	}
	return out, nil
}

// SetAssistantLimit sets client's daily limit in currency; perDayMinor 0 removes it.
func (s *DualTokenStore) SetAssistantLimit(ctx context.Context, workspaceID, by, client, currency string, perDayMinor int64) error {
	client, err := PaymentRequestClient("", "", client)
	if err != nil {
		return err
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if _, ok := MoneyCurrencies[currency]; !ok {
		return fmt.Errorf("%w: %q is not a currency Talyvor holds", ErrPaymentRequest, currency)
	}
	switch {
	case perDayMinor < 0:
		return fmt.Errorf("%w: per_day_minor is 0 (no limit) or more", ErrPaymentRequest)
	case perDayMinor == 0:
		_, err = s.pool.Exec(ctx, `DELETE FROM assistant_limits WHERE workspace_id = $1 AND client = $2 AND currency = $3`, workspaceID, client, currency)
	default:
		_, err = s.pool.Exec(ctx, `INSERT INTO assistant_limits (workspace_id, client, currency, per_day_minor, updated_by) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (workspace_id, client, currency) DO UPDATE SET per_day_minor = EXCLUDED.per_day_minor, updated_by = EXCLUDED.updated_by, updated_at = now()`,
			workspaceID, client, currency, perDayMinor, by)
	}
	if err != nil {
		return fmt.Errorf("economy: assistant limit: %w", err)
	}
	return nil
}

// refuseOverAssistantLimit refuses items that would take client over its daily limit in any currency. It runs under
// the workspace's payment request lock, so two requests at once cannot both fit under the same remainder.
func refuseOverAssistantLimit(ctx context.Context, tx pgx.Tx, workspaceID, client string, items []PaymentRequestItem) error {
	adding := map[string]int64{}
	for _, it := range items {
		if a := adding[it.Currency]; it.Decision != ItemBlocked {
			adding[it.Currency] = a + min(it.AmountMinor, math.MaxInt64-a) // saturates: huge amounts must not wrap under the limit
		}
	}
	for currency, add := range adding {
		rows, _ := tx.Query(ctx, limitsSQL+` AND l.client = $2 AND l.currency = $3`, workspaceID, client, currency)
		l, err := pgx.CollectExactlyOneRow(rows, scanAssistantLimit)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			continue
		case err != nil:
			return fmt.Errorf("economy: assistant limit: %w", err)
		case add > l.PerDayMinor-l.PreparedTodayMinor:
			return fmt.Errorf("%w: %s may prepare up to %s a day and has prepared %s today; this request adds %s",
				ErrAssistantLimit, client, formatMinor(currency, l.PerDayMinor), formatMinor(currency, l.PreparedTodayMinor), formatMinor(currency, add))
		}
	}
	return nil
}
