package market

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// escrow.go — B32.17: THE HOLDBACK IS THE ESCROW.
//
// A cleared earning waits in its seller's holdback (journal.go) until its payable_at, 14 days after the buyer's
// invoice was paid. ReleaseDue — run every few minutes beside the pending meter (cmd/lens) — then moves it to the
// seller's available balance with one release entry, once per earning, unless an open hold names its use.
//
// A hold (a buyer's dispute, an IP claim) is how money stays in escrow past its 14 days. market_earnings is
// append-only, so a hold is its own row (market_holds, migration 0200): opened while the earning is still inside
// its 14 days, or before its use clears, and released once with the decision — the earning is released then and
// there if its 14 days are over. Payouts and credits leave available (payout.go), and a refund of an earning
// already released comes back out of available (migration 0200): after a payout it goes below zero, which is what
// payout.go calls owed. JournalCheck reconciles a seller's journal with sellerBalance and sellerFunds.

// The reasons a hold keeps an earning in escrow.
const (
	HoldDispute = "dispute"
	HoldIPClaim = "ip_claim"
)

// ErrOutOfHoldback: a hold keeps only an earning still inside its holdback.
var ErrOutOfHoldback = errors.New("market: the earning is already past its holdback. A hold keeps only an earning still inside it")

// Hold is one market_holds row.
type Hold struct {
	ID         string     `json:"id"`
	UseID      string     `json:"use_id"`
	Reason     string     `json:"reason"`
	OpenedBy   string     `json:"opened_by"`
	OpenedAt   time.Time  `json:"opened_at"`
	ReleasedAt *time.Time `json:"released_at,omitempty"`
	Decision   string     `json:"decision,omitempty"`
}

// lockUse orders everything that moves one use's earning between the seller's accounts: its release, a hold on
// it, and its refund (whose journal trigger takes the same lock, migration 0200).
func lockUse(ctx context.Context, tx pgx.Tx, useID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('market_use:' || $1, 0))`, useID)
	return err
}

// lockPayees takes the seller lock of every payee of useID — its seller and every ancestor its royalties pay
// (B32.26) — in one order, so two transactions taking several never wait on each other.
func lockPayees(ctx context.Context, tx pgx.Tx, useID, seller string) error {
	rows, err := tx.Query(ctx, `SELECT $2::text UNION SELECT seller_workspace_id FROM market_earnings WHERE use_id = $1 ORDER BY 1`, useID, seller)
	if err != nil {
		return err
	}
	payees, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, ws := range payees {
		if err := lockSeller(ctx, tx, ws); err != nil {
			return err
		}
	}
	return nil
}

// OpenHold keeps the earning of useID in its seller's holdback until the hold is released. The use may not have
// cleared yet; once its earning is past its holdback the hold is refused (ErrOutOfHoldback), since the money may
// already be paid out.
func (s *Store) OpenHold(ctx context.Context, useID, reason, openedBy string, now time.Time) (Hold, error) {
	if reason != HoldDispute && reason != HoldIPClaim {
		return Hold{}, invalid("a hold's reason is dispute or ip_claim")
	}
	if openedBy == "" {
		return Hold{}, invalid("a hold names who opened it")
	}
	h := Hold{ID: "mhd_" + uuid.NewString(), UseID: useID, Reason: reason, OpenedBy: openedBy}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var seller string
		if err := tx.QueryRow(ctx, `SELECT seller_workspace_id FROM market_uses WHERE id = $1`, useID).Scan(&seller); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		// The payees' locks order this with a payout, which reads the holds; the use's, with its release.
		if err := lockPayees(ctx, tx, useID, seller); err != nil {
			return err
		}
		if err := lockUse(ctx, tx, useID); err != nil {
			return err
		}
		var out bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM market_earnings WHERE use_id = $1 AND payable_at <= $2)
			OR EXISTS (SELECT 1 FROM market_journal_entries WHERE kind = 'release' AND ref = $1)`, useID, now).Scan(&out); err != nil {
			return err
		}
		if out {
			return ErrOutOfHoldback
		}
		return tx.QueryRow(ctx, `INSERT INTO market_holds (id, use_id, reason, opened_by, opened_at) VALUES ($1, $2, $3, $4, $5) RETURNING opened_at`,
			h.ID, useID, reason, openedBy, now).Scan(&h.OpenedAt)
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrOutOfHoldback) {
			return Hold{}, err
		}
		return Hold{}, fmt.Errorf("market: hold use %s: %w", useID, err)
	}
	return h, nil
}

// ReleaseHold releases a hold with the decision that ended it, and — when no other hold names the use and its
// 14 days are over — releases its earning to the seller's available balance in the same transaction.
func (s *Store) ReleaseHold(ctx context.Context, holdID, decision string, now time.Time) (Hold, error) {
	if decision == "" {
		return Hold{}, invalid("a hold is released with the decision that ended it")
	}
	var h Hold
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var seller string
		if err := tx.QueryRow(ctx, `SELECT h.use_id, COALESCE(u.seller_workspace_id, '') FROM market_holds h LEFT JOIN market_uses u ON u.id = h.use_id
			WHERE h.id = $1`, holdID).Scan(&h.UseID, &seller); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if err := lockPayees(ctx, tx, h.UseID, seller); err != nil {
			return err
		}
		if err := lockUse(ctx, tx, h.UseID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `UPDATE market_holds SET released_at = $2, decision = $3 WHERE id = $1 AND released_at IS NULL
			RETURNING id, use_id, reason, opened_by, opened_at, released_at, decision`, holdID, now, decision).
			Scan(&h.ID, &h.UseID, &h.Reason, &h.OpenedBy, &h.OpenedAt, &h.ReleasedAt, &h.Decision)
		if errors.Is(err, pgx.ErrNoRows) {
			return invalid("the hold was already released")
		}
		if err != nil {
			return err
		}
		_, err = releaseUseTx(ctx, tx, h.UseID, now)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) {
			return Hold{}, err
		}
		return Hold{}, fmt.Errorf("market: release hold %s: %w", holdID, err)
	}
	return h, nil
}

// dueAt is true of an earning e that is due for release at the time bound to placeholder now: past its holdback,
// held by nothing, never refunded, not yet released, and with a share to move.
func dueAt(now string) string {
	return `e.payable_at <= ` + now + ` AND e.share_usd_micros > 0 AND NOT ` + heldSQL + `
	AND NOT EXISTS (SELECT 1 FROM market_refunds r WHERE r.use_id = e.use_id)
	AND NOT EXISTS (SELECT 1 FROM market_journal_entries j WHERE j.kind = 'release' AND j.ref = e.use_id)`
}

// releaseUseTx releases the earnings of useID if they are due at now, on tx under the use's lock: for each of its
// payees (B32.26), +share to their holdback and −share to their available balance, funded as the earning was — one
// release entry for the use. It reports whether it did.
func releaseUseTx(ctx context.Context, tx pgx.Tx, useID string, now time.Time) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT e.seller_workspace_id, e.share_usd_micros, e.livemode, e.test FROM market_earnings e
		WHERE e.use_id = $1 AND `+dueAt("$2")+` ORDER BY e.kind <> 'sale', e.depth, e.id`, useID, now)
	if err != nil {
		return false, err
	}
	type due struct {
		payee          string
		share          int64
		livemode, test bool
	}
	earnings, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (due, error) {
		var d due
		return d, row.Scan(&d.payee, &d.share, &d.livemode, &d.test)
	})
	if err != nil || len(earnings) == 0 {
		return false, err
	}
	postings := make([]Posting, 0, 2*len(earnings))
	for _, d := range earnings {
		postings = append(postings,
			Posting{Account: SellerHoldback(d.payee), AmountUSDMicros: d.share, Funding: funding(d.livemode, d.test)},
			Posting{Account: SellerAvailable(d.payee), AmountUSDMicros: -d.share, Funding: funding(d.livemode, d.test)})
	}
	_, err = PostJournalTx(ctx, tx, JournalRelease, useID, postings[0].Funding, now, postings...)
	return err == nil, err
}

// ReleaseDue releases every earning due at now from its seller's holdback to their available balance, each in
// its own transaction, and answers how many it released.
func (s *Store) ReleaseDue(ctx context.Context, now time.Time) (int, error) {
	n := 0
	for {
		rows, err := s.pool.Query(ctx, `SELECT e.use_id FROM market_earnings e WHERE `+dueAt("$1")+`
			GROUP BY e.use_id ORDER BY min(e.payable_at), e.use_id LIMIT 500`, now)
		if err != nil {
			return n, fmt.Errorf("market: earnings due for release: %w", err)
		}
		due, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return n, fmt.Errorf("market: earnings due for release: %w", err)
		}
		released := 0
		for _, useID := range due {
			err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
				if err := lockUse(ctx, tx, useID); err != nil {
					return err
				}
				ok, err := releaseUseTx(ctx, tx, useID, now)
				if ok {
					released++
				}
				return err
			})
			if err != nil {
				return n + released, fmt.Errorf("market: release use %s: %w", useID, err)
			}
		}
		n += released
		if len(due) < 500 || released == 0 {
			return n, nil
		}
	}
}

// JournalReconciliation is one seller's journal set beside what payout.go reads from their earnings and payouts,
// in µUSD. A balance here is what the seller is owed — the negation of the account's balance on the journal — so
// a negative available balance is owed back by the seller.
type JournalReconciliation struct {
	WorkspaceID string `json:"workspace_id"`
	// The journal's seller:<ws>:holdback and seller:<ws>:available, and available by funding.
	JournalHoldbackUSDMicros  int64 `json:"journal_holdback_usd_micros"`
	JournalAvailableUSDMicros int64 `json:"journal_available_usd_micros"`
	JournalTestUSDMicros      int64 `json:"journal_available_test_usd_micros"`
	JournalLiveUSDMicros      int64 `json:"journal_available_live_usd_micros"`
	// Earnings past their holdback the release job has not moved yet: still in the journal's holdback.
	PendingReleaseUSDMicros int64 `json:"pending_release_usd_micros"`
	// payout.go: sellerBalance (released − paid out is available; below zero, owed) and sellerFunds.
	InHoldbackUSDMicros int64    `json:"in_holdback_usd_micros"`
	AvailableUSDMicros  int64    `json:"available_usd_micros"`
	TestLeftUSDMicros   int64    `json:"test_left_usd_micros"`
	LiveLeftUSDMicros   int64    `json:"live_left_usd_micros"`
	Mismatches          []string `json:"mismatches"`
}

// OK reports whether the seller's journal reconciles.
func (r JournalReconciliation) OK() bool { return len(r.Mismatches) == 0 }

// JournalCheck reconciles a seller's journal with their earnings and payouts, read at one snapshot:
//
//   - each of their accounts' balance (market_journal_balances) equals the sum of its postings;
//   - the journal's holdback, less what is due but not yet released, is sellerBalance's holdback;
//   - the journal's available, plus what is due, is sellerBalance's released less paid out;
//   - neither kind of money sellerFunds would pay out is more than the journal holds available of it (B22.1).
func (s *Store) JournalCheck(ctx context.Context, workspaceID string) (JournalReconciliation, error) {
	return s.journalCheck(ctx, workspaceID, time.Now())
}

func (s *Store) journalCheck(ctx context.Context, workspaceID string, now time.Time) (JournalReconciliation, error) {
	r := JournalReconciliation{WorkspaceID: workspaceID, Mismatches: []string{}}
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		holdback, available := SellerHoldback(workspaceID), SellerAvailable(workspaceID)
		rows, err := tx.Query(ctx, `SELECT a.account, a.funding, COALESCE(b.balance_usd_micros, 0)::bigint, COALESCE(p.total, 0)::bigint
			FROM (VALUES ($1, 'test'), ($1, 'live'), ($2, 'test'), ($2, 'live')) AS a (account, funding)
			LEFT JOIN market_journal_balances b ON b.account = a.account AND b.funding = a.funding AND b.currency = 'USD'
			LEFT JOIN (SELECT account, funding, sum(amount_usd_micros) AS total FROM market_journal_postings
			           WHERE account IN ($1, $2) AND currency = 'USD' GROUP BY account, funding) p ON p.account = a.account AND p.funding = a.funding`,
			holdback, available)
		if err != nil {
			return err
		}
		for rows.Next() {
			var account, funding string
			var balance, postings int64
			if err := rows.Scan(&account, &funding, &balance, &postings); err != nil {
				rows.Close()
				return err
			}
			if balance != postings {
				r.Mismatches = append(r.Mismatches, fmt.Sprintf("%s (%s) balance reads %d µUSD but its postings sum to %d", account, funding, balance, postings))
			}
			switch {
			case account == holdback:
				r.JournalHoldbackUSDMicros -= postings
			case funding == "test":
				r.JournalTestUSDMicros -= postings
			default:
				r.JournalLiveUSDMicros -= postings
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		r.JournalAvailableUSDMicros = r.JournalTestUSDMicros + r.JournalLiveUSDMicros
		var pendingTest, pendingLive int64
		if err := tx.QueryRow(ctx, `SELECT
			COALESCE(sum(e.share_usd_micros) FILTER (WHERE NOT e.livemode OR e.test), 0)::bigint,
			COALESCE(sum(e.share_usd_micros) FILTER (WHERE e.livemode AND NOT e.test), 0)::bigint
			FROM market_earnings e WHERE e.seller_workspace_id = $1 AND `+dueAt("$2"), workspaceID, now).Scan(&pendingTest, &pendingLive); err != nil {
			return err
		}
		r.PendingReleaseUSDMicros = pendingTest + pendingLive
		released, inHoldback, paid, err := sellerBalance(ctx, tx, workspaceID, now)
		if err != nil {
			return err
		}
		r.InHoldbackUSDMicros, r.AvailableUSDMicros = inHoldback, released-paid
		if r.TestLeftUSDMicros, r.LiveLeftUSDMicros, err = sellerFunds(ctx, tx, workspaceID, now); err != nil {
			return err
		}
		if got := r.JournalHoldbackUSDMicros - r.PendingReleaseUSDMicros; got != r.InHoldbackUSDMicros {
			r.Mismatches = append(r.Mismatches, fmt.Sprintf("the journal holds %d µUSD in holdback (%d of it due for release); the earnings say %d",
				r.JournalHoldbackUSDMicros, r.PendingReleaseUSDMicros, r.InHoldbackUSDMicros))
		}
		if got := r.JournalAvailableUSDMicros + r.PendingReleaseUSDMicros; got != r.AvailableUSDMicros {
			r.Mismatches = append(r.Mismatches, fmt.Sprintf("the journal holds %d µUSD available (and %d due for release); the earnings less the payouts say %d",
				r.JournalAvailableUSDMicros, r.PendingReleaseUSDMicros, r.AvailableUSDMicros))
		}
		if j := max(r.JournalTestUSDMicros+pendingTest, 0); r.TestLeftUSDMicros > j {
			r.Mismatches = append(r.Mismatches, fmt.Sprintf("sellerFunds would pay out %d µUSD of test money; the journal holds %d", r.TestLeftUSDMicros, j))
		}
		if j := max(r.JournalLiveUSDMicros+pendingLive, 0); r.LiveLeftUSDMicros > j {
			r.Mismatches = append(r.Mismatches, fmt.Sprintf("sellerFunds would pay out %d µUSD of live money; the journal holds %d", r.LiveLeftUSDMicros, j))
		}
		return nil
	})
	if err != nil {
		return r, fmt.Errorf("market: journal check of %s: %w", workspaceID, err)
	}
	return r, nil
}

// JournalSellers lists every workspace that has earned, been paid, or has a seller account on the journal.
func (s *Store) JournalSellers(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT seller_workspace_id FROM market_earnings
		UNION SELECT workspace_id FROM market_payouts
		UNION SELECT substring(account FROM '^seller:(.+):(?:holdback|available)$') FROM market_journal_balances WHERE account LIKE 'seller:%'
		ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("market: sellers on the journal: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("market: sellers on the journal: %w", err)
	}
	return out, nil
}
