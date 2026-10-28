package market

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// agent_payment.go — B19.15: AN AGENT PAYS ANOTHER COMPANY'S AGENT, THROUGH THE MARKETPLACE.
//
// Payments among one company's agents stay inside its closed loop (economy.PayAgent). A payment to another
// company's agent leaves it, so it goes through the marketplace like a use of a listing: one billed
// market_uses row (no listing; payee_agent_id names the agent paid), metered onto the paying company's
// monthly bill by MeterPending, cleared when that bill is paid, earned by the payee's company, payable after
// the 14-day holdback and paid out through B20.5 — and reversed if the bill is refunded or charged back. The
// paying agent's rules judge it first (economy), and the single-party detector refuses a wash trade.

// CompanyPayeeRefusal says why payerWorkspaceID may not pay an agent of payeeWorkspaceID — the two share a
// card or an owner, so it would be a company paying itself — or "" when it may.
func (s *Store) CompanyPayeeRefusal(ctx context.Context, payerWorkspaceID, payeeWorkspaceID string) (string, error) {
	linked, err := s.linked(ctx, payerWorkspaceID, payeeWorkspaceID)
	if err != nil {
		return "", err
	}
	if linked {
		return "the two companies share a card or an owner, and a company cannot pay itself through the marketplace", nil
	}
	if err := s.sellable(ctx, payerWorkspaceID); errors.Is(err, ErrNotSoldHere) { // B32.39
		return err.Error(), nil
	} else if err != nil {
		return "", err
	}
	return "", nil
}

// ChargeAgentPayment records, in tx, fromAgentID's payment of amount µLXC to toAgentID of payeeWorkspaceID as
// one billed use on payerWorkspaceID's marketplace bill — the use id, or a new one when id is "" — and returns its id.
func (s *Store) ChargeAgentPayment(ctx context.Context, tx pgx.Tx, id, payerWorkspaceID, fromAgentID, payeeWorkspaceID, toAgentID string,
	amount int64, memo string, at time.Time) (string, error) {
	if id == "" {
		id = "use_" + uuid.NewString()
	}
	if _, err := tx.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, agent_id, payee_agent_id, memo,
		price_ulxc, charge, used_at, ran_at)
		VALUES ($1, '', 0, $2, $3, $4, $5, $6, $7, 'billed', $8, $8)`,
		id, payeeWorkspaceID, payerWorkspaceID, fromAgentID, toAgentID, memo, amount, at); err != nil {
		return "", fmt.Errorf("market: record the payment to agent %s: %w", toAgentID, err)
	}
	return id, nil
}
