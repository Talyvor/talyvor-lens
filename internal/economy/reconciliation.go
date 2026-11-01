package economy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/talyvor/lens/internal/partners"
)

// reconciliation.go — B30.11: DAILY RECONCILIATION AGAINST THE PARTNER, AND THE SAFEGUARDING VIEW.
//
// Each day, for each currency, ReconcileDay compares what the ledger moved through partner accounts with the account
// partner's statement lines for the accounts they mirror, matched on the partner's payment reference, and writes one
// reconciliation_runs row with every break (migration 0236):
//
//   - missing: the ledger moved money the partner's statement does not show;
//   - extra: the statement shows money the ledger never moved;
//   - amount_differs: both have the payment, at different amounts.
//
// A posting reads as the statement does — money in positive — so a partner account's posting is negated. Postings on
// a partner account that names no account at the partner are missing: there is no statement to find them on.
//
// The same run keeps what customers hold in the currency — their company, agent, pot, hold and suspense accounts —
// against what the partner's statements say it holds for them: the safeguarding view (Safeguarding) is the latest run
// in each currency.

// Kinds of reconciliation break.
const (
	BreakMissing       = "missing"
	BreakExtra         = "extra"
	BreakAmountDiffers = "amount_differs"
)

// ReconciliationBreak is one payment the ledger and the partner's statement disagree on.
type ReconciliationBreak struct {
	Kind           string `json:"kind"`
	WorkspaceID    string `json:"workspace_id"`
	AccountID      string `json:"account_id"`  // the partner money account
	PaymentRef     string `json:"payment_ref"` // the partner's reference; "entry:<id>" for an entry that named none
	LedgerMinor    int64  `json:"ledger_minor"`
	StatementMinor int64  `json:"statement_minor"`
	AmountMinor    int64  `json:"amount_minor"` // statement minus ledger: what the partner shows that the ledger does not
}

// ReconciliationRun is one currency's reconciliation for one day.
type ReconciliationRun struct {
	ID                 string                `json:"id"`
	Day                string                `json:"day"` // YYYY-MM-DD, UTC
	Currency           string                `json:"currency"`
	Funding            string                `json:"funding"`
	Partner            string                `json:"partner"`
	CustomersHoldMinor int64                 `json:"customers_hold_minor"`
	PartnerHoldsMinor  int64                 `json:"partner_holds_minor"`
	ShortfallMinor     int64                 `json:"shortfall_minor"` // what customers hold beyond what the partner holds; 0 when covered
	BreakCount         int                   `json:"break_count"`
	Breaks             []ReconciliationBreak `json:"breaks"`
	RanAt              time.Time             `json:"ran_at"`
}

// ReconcileDay reconciles day's (UTC) postings through partner accounts with p's statement lines, in every currency,
// and writes and answers one run per currency. p is the Test partner for test money and a real one for live money.
func (s *DualTokenStore) ReconcileDay(ctx context.Context, p partners.AccountPartner, day time.Time) ([]ReconciliationRun, error) {
	from := day.UTC().Truncate(24 * time.Hour)
	until := from.Add(24 * time.Hour)
	funding := FundingLive
	if p.Name() == "test" {
		funding = FundingTest
	}

	type key struct{ account, ref string }
	type account struct{ workspace, currency, ref string }
	accounts := map[string]account{}
	// B48.6: the partner accounts agents' crypto wallets move through are reconciled against the chain (ChainIndexer.Reconcile).
	rows, err := s.pool.Query(ctx, `SELECT id, workspace_id, currency, partner_account_ref FROM money_accounts WHERE purpose = 'partner'
		AND partner_account_ref NOT LIKE 'chain:%'`)
	if err != nil {
		return nil, fmt.Errorf("economy: reconcile: %w", err)
	}
	for rows.Next() {
		var id string
		var a account
		if err := rows.Scan(&id, &a.workspace, &a.currency, &a.ref); err != nil {
			rows.Close()
			return nil, fmt.Errorf("economy: reconcile: %w", err)
		}
		accounts[id] = a
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("economy: reconcile: %w", err)
	}

	ledger := map[key]int64{}
	rows, err = s.pool.Query(ctx, `SELECT p.account_id, CASE WHEN e.partner_ref <> '' THEN e.partner_ref ELSE 'entry:' || e.id END,
		-sum(p.amount_minor)::bigint
		FROM money_postings p JOIN money_entries e ON e.id = p.entry_id JOIN money_accounts a ON a.id = p.account_id
		WHERE a.purpose = 'partner' AND a.partner_account_ref NOT LIKE 'chain:%' AND p.funding = $1 AND e.created_at >= $2 AND e.created_at < $3 GROUP BY 1, 2`, funding, from, until)
	if err != nil {
		return nil, fmt.Errorf("economy: reconcile: %w", err)
	}
	for rows.Next() {
		var k key
		var minor int64
		if err := rows.Scan(&k.account, &k.ref, &minor); err != nil {
			rows.Close()
			return nil, fmt.Errorf("economy: reconcile: %w", err)
		}
		ledger[k] = minor
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("economy: reconcile: %w", err)
	}

	statement := map[key]int64{}
	partnerHolds := map[string]int64{}
	for id, a := range accounts {
		if a.ref == "" {
			continue
		}
		lines, err := p.StatementLines(ctx, a.ref, time.Time{})
		if errors.Is(err, partners.ErrNotFound) {
			continue // the partner does not know the account: everything posted to it is missing
		}
		if err != nil {
			return nil, fmt.Errorf("economy: reconcile: the partner's statement for %s: %w", a.ref, err)
		}
		for _, l := range lines {
			partnerHolds[a.currency] += l.Amount.Minor
			if !l.At.Before(from) && l.At.Before(until) {
				statement[key{id, l.PaymentRef}] += l.Amount.Minor
			}
		}
	}

	customersHold := map[string]int64{}
	rows, err = s.pool.Query(ctx, `SELECT a.currency, sum(b.balance_minor)::bigint FROM money_accounts a
		JOIN money_account_balances b ON b.account_id = a.id AND b.funding = $1
		WHERE a.purpose IN ('company', 'agent', 'pot', 'hold', 'suspense') GROUP BY a.currency`, funding)
	if err != nil {
		return nil, fmt.Errorf("economy: reconcile: %w", err)
	}
	for rows.Next() {
		var currency string
		var minor int64
		if err := rows.Scan(&currency, &minor); err != nil {
			rows.Close()
			return nil, fmt.Errorf("economy: reconcile: %w", err)
		}
		customersHold[currency] = minor
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("economy: reconcile: %w", err)
	}

	breaks := map[string][]ReconciliationBreak{}
	seen := map[key]bool{}
	for _, side := range []map[key]int64{ledger, statement} {
		for k := range side {
			if seen[k] {
				continue
			}
			seen[k] = true
			l, inLedger := ledger[k]
			st, inStatement := statement[k]
			if l == st { // a payment and its return net to nothing on both sides
				continue
			}
			kind := BreakAmountDiffers
			switch {
			case !inStatement:
				kind = BreakMissing
			case !inLedger:
				kind = BreakExtra
			}
			a := accounts[k.account]
			breaks[a.currency] = append(breaks[a.currency], ReconciliationBreak{Kind: kind, WorkspaceID: a.workspace, AccountID: k.account,
				PaymentRef: k.ref, LedgerMinor: l, StatementMinor: st, AmountMinor: st - l})
		}
	}

	currencies := make([]string, 0, len(MoneyCurrencies))
	for c := range MoneyCurrencies {
		currencies = append(currencies, c)
	}
	sort.Strings(currencies)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("economy: reconcile: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	runs := make([]ReconciliationRun, 0, len(currencies))
	for _, c := range currencies {
		bs := breaks[c]
		if bs == nil {
			bs = []ReconciliationBreak{}
		}
		sort.Slice(bs, func(i, j int) bool {
			if bs[i].AccountID != bs[j].AccountID {
				return bs[i].AccountID < bs[j].AccountID
			}
			return bs[i].PaymentRef < bs[j].PaymentRef
		})
		raw, err := json.Marshal(bs)
		if err != nil {
			return nil, fmt.Errorf("economy: reconcile: %w", err)
		}
		r := ReconciliationRun{ID: "rec_" + uuid.NewString(), Day: from.Format(time.DateOnly), Currency: c, Funding: funding, Partner: p.Name(),
			CustomersHoldMinor: customersHold[c], PartnerHoldsMinor: partnerHolds[c], BreakCount: len(bs), Breaks: bs}
		r.ShortfallMinor = max(r.CustomersHoldMinor-r.PartnerHoldsMinor, 0)
		if err := tx.QueryRow(ctx, `INSERT INTO reconciliation_runs (id, day, currency, funding, partner, customers_hold_minor,
			partner_holds_minor, break_count, breaks) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING ran_at`,
			r.ID, from, c, funding, r.Partner, r.CustomersHoldMinor, r.PartnerHoldsMinor, r.BreakCount, string(raw)).Scan(&r.RanAt); err != nil {
			return nil, fmt.Errorf("economy: reconcile: %w", err)
		}
		runs = append(runs, r)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("economy: reconcile: %w", err)
	}
	return runs, nil
}

const reconciliationColumns = `id, day::text, currency, funding, partner, customers_hold_minor, partner_holds_minor, break_count, breaks, ran_at`

// ReconciliationRuns is the reconciliation runs, newest first, at most limit.
func (s *DualTokenStore) ReconciliationRuns(ctx context.Context, limit int) ([]ReconciliationRun, error) {
	return s.reconciliationRuns(ctx, `SELECT `+reconciliationColumns+` FROM reconciliation_runs ORDER BY ran_at DESC, currency LIMIT $1`, limit)
}

// Safeguarding is the safeguarding view: the latest run in each currency and funding — what customers hold against
// what the partner reports holding for them, and any shortfall.
func (s *DualTokenStore) Safeguarding(ctx context.Context) ([]ReconciliationRun, error) {
	return s.reconciliationRuns(ctx, `SELECT DISTINCT ON (currency, funding) `+reconciliationColumns+`
		FROM reconciliation_runs ORDER BY currency, funding, ran_at DESC`)
}

func (s *DualTokenStore) reconciliationRuns(ctx context.Context, sql string, args ...any) ([]ReconciliationRun, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("economy: reconciliation runs: %w", err)
	}
	defer rows.Close()
	out := []ReconciliationRun{}
	for rows.Next() {
		var r ReconciliationRun
		var raw []byte
		if err := rows.Scan(&r.ID, &r.Day, &r.Currency, &r.Funding, &r.Partner, &r.CustomersHoldMinor, &r.PartnerHoldsMinor,
			&r.BreakCount, &raw, &r.RanAt); err != nil {
			return nil, fmt.Errorf("economy: reconciliation runs: %w", err)
		}
		if err := json.Unmarshal(raw, &r.Breaks); err != nil {
			return nil, fmt.Errorf("economy: reconciliation runs: %w", err)
		}
		r.ShortfallMinor = max(r.CustomersHoldMinor-r.PartnerHoldsMinor, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}
