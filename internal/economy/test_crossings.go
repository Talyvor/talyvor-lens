package economy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// test_crossings.go — B26.15: THE TEST MONEY THAT REACHED REAL AGENTS BEFORE THE WALL.
//
// Before B25.1 a transfer, an escrow or a market use could join a test (synthetic) workspace and a real one, so
// test money could sit in a real balance and on its statement, and real money in a test one. B25.1 refuses new
// ones and reverses none. TestCrossings lists every such row; ReverseTestCrossing reverses one, once, recording
// it in test_crossing_reversals (0177). `lens synthetic-crossings` is the operator's command for both.

// LXCTypeTestCrossingReversal marks credits taken back from, or returned to, a workspace when a crossing is
// reversed.
const LXCTypeTestCrossingReversal = "test_crossing_reversal"

// ErrCrossingSettled: nothing of the crossing is left to reverse (it was given back, returned, or never billed).
var ErrCrossingSettled = errors.New("economy: nothing of this crossing is left to reverse")

// ErrCrossingReversed: the crossing has already been reversed.
var ErrCrossingReversed = errors.New("economy: this crossing has already been reversed")

// The sources a crossing is read from.
const (
	CrossingTransfer  = "agent_transfers"
	CrossingEscrow    = "agent_escrows"
	CrossingMarketUse = "market_uses"
)

// TestCrossing is one money row whose two sides are a test workspace and a real one.
type TestCrossing struct {
	Source          string     `json:"source"` // agent_transfers | agent_escrows | market_uses
	ID              string     `json:"id"`
	FromWorkspaceID string     `json:"from_workspace_id"` // the side that paid: sender, payer, buyer
	FromAgentID     string     `json:"from_agent_id,omitempty"`
	FromTest        bool       `json:"from_test"`       // the paying side is the test workspace
	ToWorkspaceID   string     `json:"to_workspace_id"` // the side that was paid: receiver, payee, seller
	ToAgentID       string     `json:"to_agent_id,omitempty"`
	AmountULXC      int64      `json:"amount_ulxc"`
	TestFundedULXC  int64      `json:"test_funded_ulxc"`
	State           string     `json:"state"`             // an escrow's status, a use's charge, a transfer's links
	Settled         string     `json:"settled,omitempty"` // why nothing is left to reverse; "" when something is
	Metered         bool       `json:"metered,omitempty"` // a market use on its buyer's Stripe bill
	CreatedAt       time.Time  `json:"created_at"`
	ReversedAt      *time.Time `json:"reversed_at,omitempty"`
}

// crossingNote is the reason every reversal gives.
const crossingNote = "money crossed the wall between a test workspace and a real one (B26.15)"

// TestCrossings reads every transfer, escrow and market use whose two workspaces are one test and one real,
// oldest first.
func (s *DualTokenStore) TestCrossings(ctx context.Context) ([]TestCrossing, error) {
	var all []TestCrossing
	rows, err := s.pool.Query(ctx, `SELECT t.id, t.from_workspace_id, t.from_agent_id, COALESCE(f.synthetic, false), t.to_workspace_id,
		t.to_agent_id, t.amount_ulxc, t.test_funded_ulxc, t.refund_of, t.loan_id, t.schedule_id,
		COALESCE((SELECT r.id FROM agent_transfers r WHERE r.refund_of = t.id), ''), t.created_at,
		(SELECT v.reversed_at FROM test_crossing_reversals v WHERE v.source = 'agent_transfers' AND v.original_id = t.id)
		FROM agent_transfers t LEFT JOIN workspaces f ON f.id = t.from_workspace_id LEFT JOIN workspaces o ON o.id = t.to_workspace_id
		WHERE COALESCE(f.synthetic, false) <> COALESCE(o.synthetic, false)`)
	if err != nil {
		return nil, fmt.Errorf("economy: crossing transfers: %w", err)
	}
	transfers, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (TestCrossing, error) {
		c := TestCrossing{Source: CrossingTransfer}
		var refundOf, loan, schedule, givenBackBy string
		if err := row.Scan(&c.ID, &c.FromWorkspaceID, &c.FromAgentID, &c.FromTest, &c.ToWorkspaceID, &c.ToAgentID, &c.AmountULXC,
			&c.TestFundedULXC, &refundOf, &loan, &schedule, &givenBackBy, &c.CreatedAt, &c.ReversedAt); err != nil {
			return c, err
		}
		state := []string{"transfer"}
		for _, l := range [][2]string{{"of loan ", loan}, {"by schedule ", schedule}, {"gives back ", refundOf}, {"given back by ", givenBackBy}} {
			if l[1] != "" {
				state = append(state, l[0]+l[1])
			}
		}
		c.State = strings.Join(state, ", ")
		// A transfer and its giving back cross the same wall in opposite directions and net to nothing.
		switch {
		case refundOf != "":
			c.Settled = "it gives back " + refundOf
		case givenBackBy != "":
			c.Settled = "it was given back by " + givenBackBy
		}
		return c, nil
	})
	if err != nil {
		return nil, fmt.Errorf("economy: crossing transfers: %w", err)
	}
	all = append(all, transfers...)

	rows, err = s.pool.Query(ctx, `SELECT e.id, e.payer_workspace_id, e.payer_agent_id, COALESCE(p.synthetic, false), e.payee_workspace_id,
		e.payee_agent_id, e.amount_ulxc, e.test_funded_ulxc, e.status, e.created_at,
		(SELECT v.reversed_at FROM test_crossing_reversals v WHERE v.source = 'agent_escrows' AND v.original_id = e.id)
		FROM agent_escrows e LEFT JOIN workspaces p ON p.id = e.payer_workspace_id LEFT JOIN workspaces q ON q.id = e.payee_workspace_id
		WHERE COALESCE(p.synthetic, false) <> COALESCE(q.synthetic, false)`)
	if err != nil {
		return nil, fmt.Errorf("economy: crossing escrows: %w", err)
	}
	escrows, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (TestCrossing, error) {
		c := TestCrossing{Source: CrossingEscrow}
		var status string
		if err := row.Scan(&c.ID, &c.FromWorkspaceID, &c.FromAgentID, &c.FromTest, &c.ToWorkspaceID, &c.ToAgentID, &c.AmountULXC,
			&c.TestFundedULXC, &status, &c.CreatedAt, &c.ReversedAt); err != nil {
			return c, err
		}
		c.State = "escrow " + status
		if status == "returned" && c.ReversedAt == nil {
			c.Settled = "it was returned to its payer"
		}
		return c, nil
	})
	if err != nil {
		return nil, fmt.Errorf("economy: crossing escrows: %w", err)
	}
	all = append(all, escrows...)

	rows, err = s.pool.Query(ctx, `SELECT u.id, u.buyer_workspace_id, u.agent_id, COALESCE(b.synthetic, false), u.seller_workspace_id,
		u.payee_agent_id, u.price_ulxc, u.charge, u.ran_at IS NOT NULL, u.metered_at IS NOT NULL, u.cleared_at IS NOT NULL,
		COALESCE(r.cause, ''), u.used_at,
		(SELECT v.reversed_at FROM test_crossing_reversals v WHERE v.source = 'market_uses' AND v.original_id = u.id)
		FROM market_uses u LEFT JOIN workspaces b ON b.id = u.buyer_workspace_id LEFT JOIN workspaces s ON s.id = u.seller_workspace_id
		LEFT JOIN market_refunds r ON r.use_id = u.id
		WHERE COALESCE(b.synthetic, false) <> COALESCE(s.synthetic, false)`)
	if err != nil {
		return nil, fmt.Errorf("economy: crossing market uses: %w", err)
	}
	uses, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (TestCrossing, error) {
		c := TestCrossing{Source: CrossingMarketUse}
		var charge, refund string
		var ran, metered, cleared bool
		if err := row.Scan(&c.ID, &c.FromWorkspaceID, &c.FromAgentID, &c.FromTest, &c.ToWorkspaceID, &c.ToAgentID, &c.AmountULXC,
			&charge, &ran, &metered, &cleared, &refund, &c.CreatedAt, &c.ReversedAt); err != nil {
			return c, err
		}
		state := []string{"market use " + charge}
		for _, l := range []struct {
			on   bool
			what string
		}{{metered, "metered"}, {cleared, "cleared"}, {refund != "", "refunded (" + refund + ")"}} {
			if l.on {
				state = append(state, l.what)
			}
		}
		c.State, c.Metered = strings.Join(state, ", "), metered
		switch {
		case c.ReversedAt != nil:
		case charge != "billed":
			c.Settled = "it was " + charge + ": nothing was charged"
		case !ran:
			c.Settled = "its run failed: it was never billed"
		case refund != "":
			c.Settled = "it was refunded (" + refund + ")"
		}
		return c, nil
	})
	if err != nil {
		return nil, fmt.Errorf("economy: crossing market uses: %w", err)
	}
	all = append(all, uses...)

	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.Before(all[j].CreatedAt)
		}
		return all[i].ID < all[j].ID
	})
	return all, nil
}

// ReverseTestCrossing reverses c, which TestCrossings read, in its own transaction, and records it:
//   - a transfer, or a released escrow: the agent that was paid gives the amount back to the agent that paid —
//     one agent_postings entry of kind 'reversal' and an lxc_ledger row on each side, each naming the original;
//   - a held or disputed escrow: returned to its payer by the operator;
//   - a billed market use: refunded (cause 'test_crossing'), so it is never billed if it was not yet, and its
//     seller's earning, if it had cleared, is reversed.
//
// The agent that was paid must still hold the amount; if it does not, nothing is reversed and the error says
// what it holds.
func (s *DualTokenStore) ReverseTestCrossing(ctx context.Context, c TestCrossing, operator string) error {
	switch {
	case c.ReversedAt != nil:
		return ErrCrossingReversed
	case c.Settled != "":
		return fmt.Errorf("%w: %s", ErrCrossingSettled, c.Settled)
	case strings.TrimSpace(operator) == "":
		return errors.New("economy: a reversal needs who made it")
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// What is taken back, from whom, and to whom it returns.
		from, to, entry := c.ToWorkspaceID, c.FromWorkspaceID, ""
		var err error
		switch c.Source {
		case CrossingTransfer:
			entry, err = reverseCrossingTx(ctx, tx, c, c.ToWorkspaceID, c.ToAgentID, c.FromWorkspaceID, c.FromAgentID, c.TestFundedULXC)
		case CrossingEscrow:
			var e Escrow
			if e, err = scanEscrow(tx.QueryRow(ctx, `SELECT `+escrowColumns+` FROM agent_escrows WHERE id = $1 FOR UPDATE`, c.ID)); err != nil {
				return fmt.Errorf("economy: escrow %s: %w", c.ID, err)
			}
			switch e.Status {
			case "held", "disputed": // still in escrow, in the payer's workspace: it goes back to the payer
				from = e.PayerWorkspaceID
				if err = settleEscrowTx(ctx, tx, e, EscrowEvent{Kind: "returned", Actor: "operator", Operator: operator, Detail: crossingNote}); err == nil {
					err = tx.QueryRow(ctx, `SELECT entry_id FROM agent_escrow_events WHERE escrow_id = $1 ORDER BY id DESC LIMIT 1`, e.ID).Scan(&entry)
				}
			case "released":
				entry, err = reverseCrossingTx(ctx, tx, c, e.PayeeWorkspaceID, e.PayeeAgentID, e.PayerWorkspaceID, e.PayerAgentID, e.TestFundedULXC)
			default:
				return fmt.Errorf("%w: it was returned to its payer", ErrCrossingSettled)
			}
		case CrossingMarketUse:
			err = refundCrossingUseTx(ctx, tx, c.ID)
		default:
			return fmt.Errorf("economy: no crossing source %q", c.Source)
		}
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO test_crossing_reversals (source, original_id, amount_ulxc, from_workspace_id, to_workspace_id,
			entry_id, operator) VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (source, original_id) DO NOTHING`,
			c.Source, c.ID, c.AmountULXC, from, to, entry, operator)
		if err != nil {
			return fmt.Errorf("economy: record the reversal: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrCrossingReversed
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%s %s: %w", c.Source, c.ID, err)
	}
	return nil
}

// reverseCrossingTx moves c.AmountULXC from paidAgent (of paidWS), which received it, back to payerAgent (of
// payerWS), with its test-funded part, inside tx, and returns the entry's id.
func reverseCrossingTx(ctx context.Context, tx pgx.Tx, c TestCrossing, paidWS, paidAgent, payerWS, payerAgent string, testFunded int64) (string, error) {
	// Both agents' rows in id order, then both balance rows in id order: the lock order of every transfer.
	type side struct{ ws, agent string }
	agents := []side{{paidWS, paidAgent}, {payerWS, payerAgent}}
	if agents[1].agent < agents[0].agent {
		agents[0], agents[1] = agents[1], agents[0]
	}
	for _, a := range agents {
		if err := lockAgent(ctx, tx, a.ws, a.agent); err != nil {
			return "", fmt.Errorf("agent %s: %w", a.agent, err)
		}
	}
	held, err := accountBalance(ctx, tx, paidWS, agentAccount(paidAgent))
	if err != nil {
		return "", err
	}
	if held < c.AmountULXC {
		return "", fmt.Errorf("%w: agent %s, which received it, holds %d µLXC of the %d to take back", ErrAgentFunds, paidAgent, held, c.AmountULXC)
	}
	wss := []string{paidWS, payerWS}
	if wss[1] < wss[0] {
		wss[0], wss[1] = wss[1], wss[0]
	}
	for _, ws := range wss {
		if _, _, _, err := readLXCBalance(ctx, tx, ws); err != nil {
			return "", err
		}
	}
	reverses := c.Source + ":" + c.ID
	for _, m := range []struct {
		ws, other, otherAgent, desc string
		delta                       int64
	}{
		{paidWS, payerWS, payerAgent, "money from a test workspace, taken back", -c.AmountULXC},
		{payerWS, paidWS, paidAgent, "money from a test workspace, returned", c.AmountULXC},
	} {
		bal, minted, spent, err := readLXCBalance(ctx, tx, m.ws)
		if err != nil {
			return "", err
		}
		if bal+m.delta < 0 {
			return "", fmt.Errorf("%w: workspace %s holds %d µLXC", ErrInsufficientLXC, m.ws, bal)
		}
		// The test-funded part goes back where it came from: off the paid side before its balance falls (which
		// would otherwise clamp it a second time), onto the payer's after its balance rises.
		if m.delta < 0 && testFunded > 0 {
			if _, err := tx.Exec(ctx, `UPDATE lxc_balances SET test_funded_ulxc = GREATEST(test_funded_ulxc - $2, 0) WHERE workspace_id = $1`,
				m.ws, testFunded); err != nil {
				return "", fmt.Errorf("economy: take back test-funded credits: %w", err)
			}
		}
		if err := insertLXCLedger(ctx, tx, m.ws, m.delta, bal+m.delta, LXCTypeTestCrossingReversal, m.desc, map[string]interface{}{
			"reverses": reverses, "source": c.Source, "original_id": c.ID,
			"counterparty_workspace_id": m.other, "counterparty_agent_id": m.otherAgent}); err != nil {
			return "", err
		}
		if err := writeLXCBalance(ctx, tx, m.ws, bal+m.delta, minted, spent); err != nil {
			return "", err
		}
		if m.delta > 0 {
			if err := addTestFunded(ctx, tx, m.ws, testFunded); err != nil {
				return "", err
			}
		}
	}
	entry := uuid.New()
	for _, p := range []struct {
		ws, account string
		amount      int64
	}{{paidWS, agentAccount(paidAgent), -c.AmountULXC}, {payerWS, agentAccount(payerAgent), c.AmountULXC}} {
		if _, err := tx.Exec(ctx, `INSERT INTO agent_postings (entry_id, workspace_id, account, amount_ulxc, kind, ref) VALUES ($1, $2, $3, $4, 'reversal', $5)`,
			entry, p.ws, p.account, p.amount, c.ID); err != nil {
			return "", fmt.Errorf("economy: post reversal: %w", err)
		}
	}
	return entry.String(), nil
}

// ulxcPerUSDMicro is market.ulxcPerUSDMicro: the µLXC a micro-dollar of a market use's price is.
var ulxcPerUSDMicro = int64(math.Round(1 / LXCUSDValue))

// refundCrossingUseTx refunds billed market use useID, inside tx: it is never billed if it was not yet, and its
// seller's earning, if it cleared, is reversed.
func refundCrossingUseTx(ctx context.Context, tx pgx.Tx, useID string) error {
	// The use's lock orders this against ClearInvoice, as market's own refunds do.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM market_uses WHERE id = $1 FOR UPDATE`, useID); err != nil {
		return fmt.Errorf("economy: lock use: %w", err)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO market_refunds (use_id, listing_id, buyer_workspace_id, seller_workspace_id, price_ulxc,
		gross_usd_micros, reversed_share_usd_micros, reason, cause)
		SELECT u.id, u.listing_id, u.buyer_workspace_id, u.seller_workspace_id, u.price_ulxc, u.price_ulxc / $2,
		       COALESCE((SELECT sum(e.share_usd_micros) FROM market_earnings e WHERE e.use_id = u.id), 0), $3, 'test_crossing'
		FROM market_uses u
		WHERE u.id = $1 AND u.charge = 'billed' AND u.ran_at IS NOT NULL
		ON CONFLICT (use_id) DO NOTHING`, useID, ulxcPerUSDMicro, crossingNote)
	if err != nil {
		return fmt.Errorf("economy: refund use: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: it was refunded or never billed", ErrCrossingSettled)
	}
	return nil
}
