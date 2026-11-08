package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/partners"
)

// treasury_sweeps.go — B30.59: A COMPANY ACCOUNT'S IDLE MONEY, SWEPT EACH DAY INTO A MONEY-MARKET FUND.
//
// The owner sets a target on a company currency account, for test or live money. Once a day whatever the account
// holds above the target moves into a money-market fund, opened at the broker partner (the Test broker until
// treasury_sweep is cleared) under the broker's reference for it; a withdrawal moves money back at once. The ledger
// keeps what the fund holds in a company pot of its own (migration 0366), so the money stays the company's and is
// counted once. Setting a sweep, sweeping and withdrawing are uses of treasury_sweep, class RED: a frozen workspace, or
// one that has not accepted its terms, does none of them, and live money is refused, naming the class, until it is
// cleared. At the Test broker the fund's return is simulated at SimTreasuryRateBPS and labelled so; it is never posted.
//
// shortcut: the fund's holding is the ledger's, and no order is sent to the broker, since the Test broker keeps its
// fills in memory only and would forget them on a restart; upgrade when a real fund partner adapter is configured.

// TreasuryPartners hands out the broker partner a fund is opened at: *partners.Registry.
type TreasuryPartners interface {
	Broker(ctx context.Context, capability string) (partners.BrokerPartner, error)
}

// SetTreasuryPartners is where a company's money-market fund is opened (B30.59). Until it is set, none opens.
func (s *DualTokenStore) SetTreasuryPartners(p TreasuryPartners) { s.treasuryPartners = p }

// SimTreasuryRateBPS is the yearly return, in basis points, the Test broker's fund is shown to make: simulated, never
// posted. LENS_SIM_TREASURY_RATE_BPS, default 0 — a setting for Nicolai.
var SimTreasuryRateBPS int64

var (
	// ErrTreasurySweep: a sweep that is not one — a negative target, a funding that is not test or live, an account that
	// is not a company's currency account, or a withdrawal with no positive amount or idempotency key.
	ErrTreasurySweep = errors.New("economy: invalid treasury sweep")
	// ErrTreasurySweepNotFound: the account has no sweep.
	ErrTreasurySweepNotFound = errors.New("economy: this account has no treasury sweep")
	// ErrTreasuryFunds: a withdrawal of more than the fund holds.
	ErrTreasuryFunds = errors.New("economy: the money-market fund does not hold that much")
)

// TreasurySweep is a company account's sweep: its target, the fund and what it holds.
type TreasurySweep struct {
	ID                        string    `json:"id"`
	AccountID                 string    `json:"account_id"`
	Currency                  string    `json:"currency"`
	Funding                   string    `json:"funding"`
	TargetMinor               int64     `json:"target_minor"`
	Partner                   string    `json:"partner"`
	PartnerAccountRef         string    `json:"partner_account_ref"`
	FundAccountID             string    `json:"fund_account_id"` // the ledger's pot for what the fund holds
	FundMinor                 int64     `json:"fund_minor"`
	SweptOn                   string    `json:"swept_on,omitempty"` // the last day it ran, UTC
	RateBPS                   int64     `json:"rate_bps,omitempty"`
	SimulatedDailyReturnMinor int64     `json:"simulated_daily_return_minor,omitempty"`
	Return                    string    `json:"return,omitempty"` // "simulated" at the Test broker
	CreatedAt                 time.Time `json:"created_at"`
}

// SetTreasurySweep sets the target above which accountID's funding money is swept, opening its fund the first time.
func (s *DualTokenStore) SetTreasurySweep(ctx context.Context, workspaceID, accountID string, targetMinor int64, funding string) (TreasurySweep, error) {
	if funding == "" {
		funding = FundingTest
	}
	if targetMinor < 0 || (funding != FundingTest && funding != FundingLive) {
		return TreasurySweep{}, fmt.Errorf("%w: it needs a target_minor of 0 or more, and funding test or live", ErrTreasurySweep)
	}
	c, _ := CapabilityByKey(CapabilityTreasurySweep)
	if err := refuseFrozen(ctx, s.pool, workspaceID, c); err != nil {
		return TreasurySweep{}, err
	}
	if err := refuseUnaccepted(ctx, s.pool, workspaceID, c); err != nil {
		return TreasurySweep{}, err
	}
	if funding == FundingLive {
		refusal, err := capabilityLive(ctx, s.pool, workspaceID, c)
		if err != nil {
			return TreasurySweep{}, err
		}
		if refusal != nil {
			return TreasurySweep{}, refusal
		}
	}
	var currency, purpose, holder string
	err := s.pool.QueryRow(ctx, `SELECT a.currency, a.purpose, COALESCE((SELECT name FROM workspaces WHERE id = $2), '')
		FROM money_accounts a WHERE a.id = $1 AND a.workspace_id = $2 AND a.status = 'open'`, accountID, workspaceID).Scan(&currency, &purpose, &holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return TreasurySweep{}, fmt.Errorf("%w: %s", ErrMoneyAccountNotFound, accountID)
	}
	if err != nil {
		return TreasurySweep{}, fmt.Errorf("economy: set treasury sweep: %w", err)
	}
	if purpose != MoneyCompany {
		return TreasurySweep{}, fmt.Errorf("%w: a sweep is set on a company's currency account, not a %s account", ErrTreasurySweep, purpose)
	}
	if sw, err := s.TreasurySweep(ctx, workspaceID, accountID); !errors.Is(err, ErrTreasurySweepNotFound) {
		if err != nil {
			return TreasurySweep{}, err
		}
		return s.retargetTreasurySweep(ctx, workspaceID, sw, targetMinor, funding)
	}

	if s.treasuryPartners == nil {
		return TreasurySweep{}, fmt.Errorf("%w: no broker partner is set", ErrAccountNotOpened)
	}
	p, err := s.treasuryPartners.Broker(ctx, CapabilityTreasurySweep)
	if err != nil {
		return TreasurySweep{}, fmt.Errorf("economy: set treasury sweep: %w", err)
	}
	if holder == "" {
		holder = workspaceID
	}
	id, fund := "tsw_"+uuid.NewString(), "macc_"+uuid.NewString()
	opened, err := p.OpenAccount(ctx, partners.AccountRequest{ID: id, Holder: holder, Currency: currency})
	if err != nil {
		return TreasurySweep{}, fmt.Errorf("%w: %v", ErrAccountNotOpened, err)
	}
	if opened.Status != partners.StatusCompleted {
		return TreasurySweep{}, fmt.Errorf("%w: it answered %s: %s", ErrAccountNotOpened, opened.Status, opened.Detail)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TreasurySweep{}, fmt.Errorf("economy: set treasury sweep: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO money_accounts (id, workspace_id, currency, purpose, name) VALUES ($1, $2, $3, $4, $5)`,
		fund, workspaceID, currency, MoneyPot, "Money-market fund at "+p.Name()); err != nil {
		return TreasurySweep{}, fmt.Errorf("economy: set treasury sweep: %w", err)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO treasury_sweeps (id, workspace_id, account_id, funding, target_minor, partner, partner_account_ref, fund_account_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (account_id) DO NOTHING`,
		id, workspaceID, accountID, funding, targetMinor, p.Name(), opened.Ref, fund)
	if err != nil {
		return TreasurySweep{}, fmt.Errorf("economy: set treasury sweep: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// One set at the same time is the account's sweep; this one is rolled back and that one retargeted.
		_ = tx.Rollback(ctx)
		return s.SetTreasurySweep(ctx, workspaceID, accountID, targetMinor, funding)
	}
	if err := tx.Commit(ctx); err != nil {
		return TreasurySweep{}, fmt.Errorf("economy: set treasury sweep: %w", err)
	}
	return s.TreasurySweep(ctx, workspaceID, accountID)
}

// retargetTreasurySweep changes an existing sweep's target. Its funding stays what the fund was opened for.
func (s *DualTokenStore) retargetTreasurySweep(ctx context.Context, workspaceID string, sw TreasurySweep, targetMinor int64, funding string) (TreasurySweep, error) {
	if funding != sw.Funding {
		return TreasurySweep{}, fmt.Errorf("%w: this account's sweep is for %s money", ErrTreasurySweep, sw.Funding)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE treasury_sweeps SET target_minor = $3, updated_at = now() WHERE id = $1 AND workspace_id = $2`,
		sw.ID, workspaceID, targetMinor); err != nil {
		return TreasurySweep{}, fmt.Errorf("economy: set treasury sweep: %w", err)
	}
	return s.TreasurySweep(ctx, workspaceID, sw.AccountID)
}

// TreasurySweep is accountID's sweep in workspaceID and what its fund holds.
func (s *DualTokenStore) TreasurySweep(ctx context.Context, workspaceID, accountID string) (TreasurySweep, error) {
	var sw TreasurySweep
	var sweptOn *time.Time
	err := s.pool.QueryRow(ctx, `SELECT t.id, t.account_id, a.currency, t.funding, t.target_minor, t.partner, t.partner_account_ref,
		t.fund_account_id, t.swept_on, t.created_at,
		COALESCE((SELECT sum(b.balance_minor) FROM money_account_balances b WHERE b.account_id = t.fund_account_id AND b.funding = t.funding), 0)::bigint
		FROM treasury_sweeps t JOIN money_accounts a ON a.id = t.account_id
		WHERE t.workspace_id = $1 AND t.account_id = $2`, workspaceID, accountID).Scan(
		&sw.ID, &sw.AccountID, &sw.Currency, &sw.Funding, &sw.TargetMinor, &sw.Partner, &sw.PartnerAccountRef,
		&sw.FundAccountID, &sweptOn, &sw.CreatedAt, &sw.FundMinor)
	if errors.Is(err, pgx.ErrNoRows) {
		return TreasurySweep{}, ErrTreasurySweepNotFound
	}
	if err != nil {
		return TreasurySweep{}, fmt.Errorf("economy: treasury sweep: %w", err)
	}
	if sweptOn != nil {
		sw.SweptOn = sweptOn.Format(time.DateOnly)
	}
	if sw.Partner == "test" {
		// A day's share of the yearly rate, rounded down, without overflowing on a large fund.
		const perDay = 10_000 * 365
		sw.RateBPS, sw.Return = SimTreasuryRateBPS, "simulated"
		sw.SimulatedDailyReturnMinor = sw.FundMinor/perDay*sw.RateBPS + sw.FundMinor%perDay*sw.RateBPS/perDay
	}
	return sw, nil
}

// SweepTreasuries runs each sweep that has not run on now's day (UTC): what an account holds above its target moves
// into its fund. It answers how many moved money; a sweep that fails is left for the next run and does not stop the rest.
func (s *DualTokenStore) SweepTreasuries(ctx context.Context, now time.Time) (int, error) {
	day := now.UTC().Format(time.DateOnly)
	rows, err := s.pool.Query(ctx, `SELECT id FROM treasury_sweeps WHERE swept_on IS NULL OR swept_on < $1::date ORDER BY id`, day)
	if err != nil {
		return 0, fmt.Errorf("economy: sweep treasuries: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("economy: sweep treasuries: %w", err)
	}
	moved, errs := 0, []error{}
	for _, id := range ids {
		ok, err := s.sweepTreasury(ctx, id, day)
		if err != nil {
			errs = append(errs, fmt.Errorf("treasury sweep %s: %w", id, err))
		}
		if ok {
			moved++
		}
	}
	return moved, errors.Join(errs...)
}

// sweepTreasury runs sweep id for day once, under locks on the sweep and its account so no payment from the account
// lands between reading its balance and moving the excess.
func (s *DualTokenStore) sweepTreasury(ctx context.Context, id, day string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var ws, account, fund, funding, partner string
	var target int64
	var done bool
	if err := tx.QueryRow(ctx, `SELECT workspace_id, account_id, fund_account_id, funding, target_minor, partner,
		COALESCE(swept_on >= $2::date, false) FROM treasury_sweeps WHERE id = $1 FOR UPDATE`, id, day).Scan(
		&ws, &account, &fund, &funding, &target, &partner, &done); err != nil {
		return false, err
	}
	if done {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM money_accounts WHERE id = $1 FOR UPDATE`, account); err != nil {
		return false, err
	}
	var held int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1 AND funding = $2`,
		account, funding).Scan(&held); err != nil {
		return false, err
	}
	excess := held - target
	if excess > 0 {
		if _, err := postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: ws, Capability: CapabilityTreasurySweep, Kind: "treasury_sweep",
			IdempotencyKey: "treasury_sweep:" + id + ":" + day, Funding: funding, Memo: "To the money-market fund at " + partner,
			Postings: []MoneyPosting{{AccountID: account, AmountMinor: -excess}, {AccountID: fund, AmountMinor: excess}}}, s.screener); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE treasury_sweeps SET swept_on = $2::date WHERE id = $1`, id, day); err != nil {
		return false, err
	}
	return excess > 0, tx.Commit(ctx)
}

// WithdrawTreasury moves amountMinor from accountID's fund back into the account at once, as one entry under key. The
// same key again answers the entry it first wrote.
func (s *DualTokenStore) WithdrawTreasury(ctx context.Context, workspaceID, accountID string, amountMinor int64, key string) (MoneyEntry, error) {
	if amountMinor <= 0 || strings.TrimSpace(key) == "" {
		return MoneyEntry{}, fmt.Errorf("%w: a withdrawal needs a positive amount_minor and an idempotency_key", ErrTreasurySweep)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: withdraw from the fund: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id, fund, funding, partner string
	err = tx.QueryRow(ctx, `SELECT id, fund_account_id, funding, partner FROM treasury_sweeps WHERE workspace_id = $1 AND account_id = $2 FOR UPDATE`,
		workspaceID, accountID).Scan(&id, &fund, &funding, &partner)
	if errors.Is(err, pgx.ErrNoRows) {
		return MoneyEntry{}, ErrTreasurySweepNotFound
	}
	if err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: withdraw from the fund: %w", err)
	}
	e, err := postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: workspaceID, Capability: CapabilityTreasurySweep, Kind: "treasury_withdrawal",
		IdempotencyKey: "treasury_withdrawal:" + id + ":" + key, Funding: funding, Memo: "From the money-market fund at " + partner,
		Postings: []MoneyPosting{{AccountID: fund, AmountMinor: -amountMinor}, {AccountID: accountID, AmountMinor: amountMinor}}}, s.screener)
	if err != nil {
		return MoneyEntry{}, err
	}
	// Read after posting, under the sweep's lock: a replay posted nothing, and a new entry that overdraws is rolled back.
	var left int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1 AND funding = $2`,
		fund, funding).Scan(&left); err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: withdraw from the fund: %w", err)
	}
	if left < 0 {
		return MoneyEntry{}, fmt.Errorf("%w: it holds %s, and this is %s", ErrTreasuryFunds, minorString(left+amountMinor, e.Postings[0].Currency),
			minorString(amountMinor, e.Postings[0].Currency))
	}
	if err := tx.Commit(ctx); err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: withdraw from the fund: %w", err)
	}
	return e, nil
}
