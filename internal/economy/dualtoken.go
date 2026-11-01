package economy

// dualtoken.go — the two-token split (Master Plan Upgrade 1).
//
// LENS is the volatile, tradeable, mined token (the existing
// one — role-clarified). LXC is a USD-pegged, one-way compute
// credit: 1 LXC = $0.10 of compute, minted by converting LENS
// at the admin-approved rate, spent on AI calls, and NEVER
// convertible back to LENS or fiat.
//
// Splitting the roles kills the arbitrage of a single token
// trying to be both a stablecoin and a speculative asset: the
// peg lives entirely in LXC (a fixed constant, never computed),
// while price discovery lives entirely in LENS.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/ap2"
	"github.com/talyvor/lens/internal/dbjson"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/passkey"
)

// ─── constants ───────────────────────────────────

const (
	// LXCUSDValue is the FIXED peg: 1 LXC always equals $0.10 of
	// compute credit. This is a constant — it is never computed,
	// derived, or adjusted. The peg is what makes LXC a stable
	// unit of account.
	LXCUSDValue = 0.10

	// ConversionSpread is the company margin / stability buffer
	// applied on top of the fair rate when minting LXC. 5%.
	ConversionSpread = 0.05

	// MaxRateChangePct caps how far an approved rate can move from
	// the previous approved rate in a single approval (±10%). Stops
	// a sudden backing-value swing from whipsawing the conversion
	// rate.
	MaxRateChangePct = 0.10

	// Phase1FloorRate is the conservative fixed floor for the
	// LENS-per-LXC rate until a live marketplace price exists.
	// TODO(phase2): replace this fixed floor with a floor derived
	// from the live marketplace mid-price once there is real
	// LENS price discovery on the order book.
	Phase1FloorRate = 1.0

	// MinConversionLXC is the smallest LXC amount a single conversion can mint,
	// in µLXC (SEC-2) — blocks dust conversions. 100_000 µLXC = 0.10 LXC.
	MinConversionLXC int64 = 100_000

	// Ledger row types for the LXC + conversion paths.
	LXCTypeConvertFromLENS = "convert_from_lens"
	LXCTypeSpend           = "spend"
	LXCTypePurchase        = "purchase"
	// LXCTypeGrant marks a COMPED admin grant (economy.GrantLXC) — LXC that entered
	// a workspace's balance without a fiat purchase. Deliberately DISTINCT from
	// LXCTypePurchase so an auditor summing revenue (type='purchase') never counts a
	// comp as a sale, and a comp is always self-identifying in the ledger. Not a LENS
	// mint type (mint_gate.mintTypeList is the lens_token_ledger; this is lxc_ledger).
	LXCTypeGrant = "admin_grant"
	// LENSTypeConvertToLXC is the writer-side name for the conversion debit. The canonical
	// constant lives in internal/mining beside the supply predicates that classify it as a
	// BURN (burnedSupplyTypeList) — aliasing rather than re-declaring keeps the string that
	// the ledger writes and the string that supply subtracts provably identical.
	LENSTypeConvertToLXC = mining.TypeConvertToLXC
)

// ChargeULXC prices a USD amount as a charge: µLXC at the peg, rounded UP. B17.224 — the quotient is first
// rounded to a millionth of a µLXC, so float noise (1 in / 3 out tokens of GPT-4.1 nano comes to
// 13.000000000000002) is never billed as one µLXC more than the catalog price.
func ChargeULXC(usd float64) int64 {
	return int64(math.Ceil(math.Round(usd/LXCUSDValue*1e12) / 1e6))
}

// ─── errors ──────────────────────────────────────

var (
	ErrInsufficientLXC     = errors.New("economy: insufficient LXC balance")
	ErrConversionTooSmall  = fmt.Errorf("economy: conversion below minimum of %d µLXC", MinConversionLXC)
	ErrInsufficientLENSFor = errors.New("economy: insufficient LENS balance for conversion")
)

// ─── types ───────────────────────────────────────

// RateComputation captures every intermediate value the rate
// engine used so an approved rate is fully reconstructable and
// the dashboard can show the math.
type RateComputation struct {
	FairRate     float64 `json:"fair_rate"`     // R_fair (before spread)
	AdminRate    float64 `json:"admin_rate"`    // R_admin (final, after spread + guards)
	BackingValue float64 `json:"backing_value"` // USD value backing 1 LENS
	Circulating  float64 `json:"circulating"`   // circulating LENS supply
	Spread       float64 `json:"spread"`        // policy spread applied
	PreviousRate float64 `json:"previous_rate"` // last approved rate
	Clamped      bool    `json:"clamped"`       // true if the ±band limited it
	Floored      bool    `json:"floored"`       // true if the Phase1 floor applied
}

// ConvertResult is what ConvertLENStoLXC returns. LXC amounts are integer µLXC,
// LENS amounts integer µLENS (SEC-2). Rate is a Tier-2 float (LENS per LXC).
type ConvertResult struct {
	LXCMinted      int64   `json:"lxc_minted_ulxc"`
	LENSSpent      int64   `json:"lens_spent_ulens"`
	Rate           float64 `json:"rate"`
	NewLXCBalance  int64   `json:"new_lxc_balance_ulxc"`
	NewLENSBalance int64   `json:"new_lens_balance_ulens"`
}

// LXCSnapshot mirrors a row of lxc_balances. Balances are integer µLXC (SEC-2);
// USDValue is the balance's worth in integer µUSD (1e-6 USD) at the fixed peg.
type LXCSnapshot struct {
	WorkspaceID    string `json:"workspace_id"`
	Balance        int64  `json:"balance_ulxc"`
	LifetimeMinted int64  `json:"lifetime_minted_ulxc"`
	LifetimeSpent  int64  `json:"lifetime_spent_ulxc"`
	USDValue       int64  `json:"usd_value_uusd"`
}

// ─── RateEngine ──────────────────────────────────

// RateEngine derives the LENS->LXC conversion rate from the
// backing value of mined work + the circulating LENS supply. The
// admin can only ever APPROVE the engine's output (clamped to a
// ±band and a floor) — there is no path to set an arbitrary rate.
type RateEngine struct {
	ledger *mining.LedgerStore
	pool   pgxDB
}

// NewRateEngine wraps a real pool.
func NewRateEngine(ledger *mining.LedgerStore, pool *pgxpool.Pool) *RateEngine {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newRateEngine(ledger, db)
}

func newRateEngine(ledger *mining.LedgerStore, db pgxDB) *RateEngine {
	return &RateEngine{ledger: ledger, pool: db}
}

// ComputeFairRate derives the rate from current supply + backing.
//
//	backing_value_per_LENS = total_USD_value_of_verified_work
//	                         / circulating_LENS_supply
//	R_fair  = LXCUSDValue / backing_value_per_LENS
//	R_admin = R_fair * (1 + ConversionSpread)
//	         then clamped to ±MaxRateChangePct of the previous
//	         approved rate, then floored at Phase1FloorRate.
//
// The total USD value of verified work is the all-time minted
// LENS (sum of mining-track credits) valued at the peg — each
// LENS was minted as the reward for compute work worth, at mint
// time, the peg's value. Circulating supply is minted minus
// burned, so as LENS is spent/burned the remaining supply is
// backed by proportionally more work and the fair rate falls.
func (e *RateEngine) ComputeFairRate(ctx context.Context) (RateComputation, error) {
	totalMinted, err := e.ledger.GetTotalSupply(ctx)
	if err != nil {
		return RateComputation{}, fmt.Errorf("economy: total supply: %w", err)
	}
	circulating, err := e.ledger.GetCirculatingSupply(ctx)
	if err != nil {
		return RateComputation{}, fmt.Errorf("economy: circulating supply: %w", err)
	}
	prev, err := e.CurrentRate(ctx)
	if err != nil {
		return RateComputation{}, err
	}

	// SEC-2: supply is integer µLENS; the rate math (Tier-2) works in whole LENS,
	// so convert to float LENS here. RateComputation.Circulating is a Tier-2
	// display/audit value (conversion_rate_history.circulating stays DOUBLE).
	totalMintedLENS := mining.MicroToFloat(totalMinted)
	circulatingLENS := mining.MicroToFloat(circulating)

	comp := RateComputation{
		Spread:       ConversionSpread,
		Circulating:  circulatingLENS,
		PreviousRate: prev,
	}

	// Backing + fair rate. Guard against an empty economy (no LENS
	// minted yet or everything burned) — fall through to the floor.
	if totalMinted > 0 && circulating > 0 {
		totalWorkUSD := totalMintedLENS * LXCUSDValue
		comp.BackingValue = totalWorkUSD / circulatingLENS
		comp.FairRate = LXCUSDValue / comp.BackingValue
	} else {
		comp.BackingValue = 0
		comp.FairRate = Phase1FloorRate
	}

	rAdmin := comp.FairRate * (1 + ConversionSpread)

	// Guard 1: clamp to ±MaxRateChangePct of the previous rate.
	if prev > 0 {
		hi := prev * (1 + MaxRateChangePct)
		lo := prev * (1 - MaxRateChangePct)
		if rAdmin > hi {
			rAdmin = hi
			comp.Clamped = true
		} else if rAdmin < lo {
			rAdmin = lo
			comp.Clamped = true
		}
	}

	// Guard 2: never below the Phase1 floor.
	if rAdmin < Phase1FloorRate {
		rAdmin = Phase1FloorRate
		comp.Floored = true
	}

	// SEC-2: AdminRate is a Tier-2 rate (LENS per LXC), not a conserved amount, so
	// it stays float64 and the roundTo(_,6) band-aid is removed. Full Tier-2 rate
	// treatment is deferred; the conversion path that consumes this rate rounds
	// its conserved µLENS RESULT house-favoring (see ConvertLENStoLXC).
	comp.AdminRate = rAdmin
	return comp, nil
}

// CurrentRate returns the most recent approved rate from
// conversion_rate_history, or Phase1FloorRate when no rate has
// been approved yet.
func (e *RateEngine) CurrentRate(ctx context.Context) (float64, error) {
	if e.pool == nil {
		return Phase1FloorRate, nil
	}
	row := e.pool.QueryRow(ctx,
		`SELECT rate FROM conversion_rate_history ORDER BY created_at DESC LIMIT 1`)
	var rate float64
	if err := row.Scan(&rate); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Phase1FloorRate, nil
		}
		return 0, fmt.Errorf("economy: current rate: %w", err)
	}
	return rate, nil
}

// ApproveRate computes the fair rate, writes it to
// conversion_rate_history with all intermediate inputs, and
// returns the computation. This is the only way a rate changes —
// the admin approves the algorithm's output, never an arbitrary
// number.
func (e *RateEngine) ApproveRate(ctx context.Context, approvedBy string) (RateComputation, error) {
	comp, err := e.ComputeFairRate(ctx)
	if err != nil {
		return RateComputation{}, err
	}
	if e.pool == nil {
		return comp, nil
	}
	_, err = e.pool.Exec(ctx, `
		INSERT INTO conversion_rate_history
			(rate, fair_rate, backing_value, circulating, spread, previous_rate, approved_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		comp.AdminRate, comp.FairRate, comp.BackingValue, comp.Circulating,
		comp.Spread, comp.PreviousRate, approvedBy)
	if err != nil {
		return RateComputation{}, fmt.Errorf("economy: write rate history: %w", err)
	}
	return comp, nil
}

// RateHistoryEntry is one row of conversion_rate_history for the
// public history endpoint.
type RateHistoryEntry struct {
	Rate         float64   `json:"rate"`
	FairRate     float64   `json:"fair_rate"`
	BackingValue float64   `json:"backing_value"`
	Circulating  float64   `json:"circulating"`
	Spread       float64   `json:"spread"`
	PreviousRate float64   `json:"previous_rate"`
	ApprovedBy   string    `json:"approved_by"`
	CreatedAt    time.Time `json:"created_at"`
}

// RateHistory returns recent rate approvals, newest first.
func (e *RateEngine) RateHistory(ctx context.Context, limit int) ([]RateHistoryEntry, error) {
	if e.pool == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := e.pool.Query(ctx, `
		SELECT rate, fair_rate, backing_value, circulating, spread, previous_rate, approved_by, created_at
		FROM conversion_rate_history ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("economy: rate history: %w", err)
	}
	defer rows.Close()
	var out []RateHistoryEntry
	for rows.Next() {
		var h RateHistoryEntry
		if err := rows.Scan(&h.Rate, &h.FairRate, &h.BackingValue, &h.Circulating,
			&h.Spread, &h.PreviousRate, &h.ApprovedBy, &h.CreatedAt); err != nil {
			return nil, fmt.Errorf("economy: scan rate history: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ─── DualTokenStore ──────────────────────────────

// DualTokenStore owns the LXC side + the LENS->LXC conversion.
// It holds a reference to the LENS LedgerStore for reads, but the
// conversion itself runs as a single inline transaction touching
// both tokens' tables so the debit + mint can never partially
// apply.
type DualTokenStore struct {
	lens   *mining.LedgerStore
	pool   pgxDB
	engine *RateEngine
	// B19.16: the relying party approvals' passkeys are checked for, and the push sent for each approval filed.
	approvalRP     *passkey.RelyingParty
	approvalPusher ApprovalPusher
	approvalTTL    time.Duration // B50.18: how long an approval lasts; 0 is defaultApprovalTTL
	// B40.4: how the phone app's device keys are checked when they are registered.
	deviceVerifier devicekey.Verifier
	// B40.15: the native push sent to each phone with a push token for each approval filed or request prepared.
	nativePusher NativePusher
	// B45.28: the key the owner's AP2 mandates and agents' approval evidence are signed with.
	mandateSigner *ap2.Signer
	// B19.11: what the verified-agent badge follows.
	ownerVerifier OwnerVerifier
	// cashOutPartner pays cash-outs out as money (B22.9); nil, cash-out is unavailable.
	cashOutPartner CashOutPartner
	// screener screens who money in or out through a partner is from or to before it moves (B30.6); nil, no such
	// money moves.
	screener PaymentScreener
	// monitor runs transaction monitoring on each payment in or out through a partner once it has moved (B30.7);
	// nil, the nightly run is the only one.
	monitor MoneyMonitor
	// accountPartners is where a company's currency account is opened (B30.13); nil, none opens.
	accountPartners AccountPartners
	// moneyLayer opens and reads every agent's wallet (B46.4); nil, no agent has one.
	moneyLayer *money.Registry
	// smartAccounts makes each crypto agent's wallet its own smart account (B48.2); nil, none is made.
	smartAccounts *smartAccounts
	// invoicePayBase is the web app's origin an invoice's pay link points at (B30.20).
	invoicePayBase string
	// payeeScreener screens an outside payee when it is saved and before it is paid (B30.16); nil, none is.
	payeeScreener PayeeScreener
	// unusual is when a payment request item's amount is far above its payee's usual (B41.103); zero, never.
	unusual unusualAmount
	// B19.17: the marketplace a schedule pays a listing through.
	listings ListingCharger
	// B19.15: the marketplace a payment to another company's agent goes through.
	companyPayments CompanyPayments
	// B22.1: whether Lens's Stripe key is live, which makes a Stripe bill real money.
	liveStripe bool
	// B32.11: the platform fee's rate on a workspace's AI spend; nil, no fee.
	platformFee PlatformFeeResolver
	// B32.32: what judges a room wallet's monthly limit against the room owner's plan; nil, a room wallet's rules
	// cannot be saved.
	roomBudgets RoomBudgets
}

// NewDualTokenStore wraps a real pool.
func NewDualTokenStore(lens *mining.LedgerStore, pool *pgxpool.Pool, engine *RateEngine) *DualTokenStore {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newDualTokenStore(lens, db, engine)
}

func newDualTokenStore(lens *mining.LedgerStore, db pgxDB, engine *RateEngine) *DualTokenStore {
	return &DualTokenStore{lens: lens, pool: db, engine: engine}
}

// ConvertLENStoLXC mints LXC by spending LENS at the current
// approved rate. ONE-WAY: there is deliberately no inverse method
// — LXC can never be converted back to LENS or to fiat.
//
// The whole operation runs in a single transaction: read+debit
// the LENS balance, read+credit the LXC balance, and append a row
// to each token's ledger. Any failure rolls the lot back.
func (s *DualTokenStore) ConvertLENStoLXC(ctx context.Context, workspaceID string, lxcAmount int64) (ConvertResult, error) {
	if lxcAmount < MinConversionLXC {
		return ConvertResult{}, ErrConversionTooSmall
	}
	rate, err := s.engine.CurrentRate(ctx)
	if err != nil {
		return ConvertResult{}, err
	}
	// SEC-2 site #1 (LXC→LENS conversion). lxcAmount is conserved µLXC; rate is a
	// Tier-2 float (LENS per LXC). µLXC × (LENS/LXC) = µLENS (same 1e6 scale). The
	// LENS cost is a CHARGE the buyer pays to mint LXC, so it rounds UP (MulCeil)
	// — the buyer is never under-charged a sub-µLENS; the extra sub-unit is a
	// larger debit retained by the protocol. The LXC minted is exactly lxcAmount.
	lensCost := mining.MulCeil(lxcAmount, rate)

	if s.pool == nil {
		// No-DB path (unit tests of the pure arithmetic).
		return ConvertResult{
			LXCMinted: lxcAmount,
			LENSSpent: lensCost,
			Rate:      rate,
		}, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ConvertResult{}, fmt.Errorf("economy: begin conversion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// ── debit LENS ──
	lensBal, lensEarned, lensSpent, err := readLENSBalance(ctx, tx, workspaceID)
	if err != nil {
		return ConvertResult{}, err
	}
	if lensBal < lensCost {
		return ConvertResult{}, ErrInsufficientLENSFor
	}
	newLENS := lensBal - lensCost // exact integer µLENS
	meta := map[string]interface{}{"lxc_minted": lxcAmount, "rate": rate}
	if err := insertLENSLedger(ctx, tx, workspaceID, -lensCost, newLENS,
		LENSTypeConvertToLXC, "converted to LXC", meta); err != nil {
		return ConvertResult{}, err
	}
	if err := writeLENSBalance(ctx, tx, workspaceID, newLENS, lensEarned, lensSpent+lensCost); err != nil {
		return ConvertResult{}, err
	}

	// ── credit LXC ──
	lxcBal, lxcMinted, lxcSpentTotal, err := readLXCBalance(ctx, tx, workspaceID)
	if err != nil {
		return ConvertResult{}, err
	}
	newLXC := lxcBal + lxcAmount // exact integer µLXC
	lxcMeta := map[string]interface{}{"lens_spent": lensCost, "rate": rate, "funding": FundingSynthetic}
	if err := insertLXCLedger(ctx, tx, workspaceID, lxcAmount, newLXC,
		LXCTypeConvertFromLENS, "converted from LENS", lxcMeta); err != nil {
		return ConvertResult{}, err
	}
	if err := writeLXCBalance(ctx, tx, workspaceID, newLXC, lxcMinted+lxcAmount, lxcSpentTotal); err != nil {
		return ConvertResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return ConvertResult{}, fmt.Errorf("economy: commit conversion: %w", err)
	}
	metrics.ConvertedLXC(mining.MicroToFloat(lxcAmount)) // LXC minted via LENS→LXC conversion
	return ConvertResult{
		LXCMinted:      lxcAmount,
		LENSSpent:      lensCost,
		Rate:           rate,
		NewLXCBalance:  newLXC,
		NewLENSBalance: newLENS,
	}, nil
}

// SpendLXC debits the workspace's LXC balance — this is what the
// proxy bills against on each AI call. Fails with ErrInsufficientLXC
// when the balance can't cover the spend.
func (s *DualTokenStore) SpendLXC(ctx context.Context, workspaceID string, lxcAmount int64, description string) error {
	_, err := s.SpendLXCMeta(ctx, workspaceID, lxcAmount, description, nil)
	return err
}

// SpendLXCMeta is SpendLXC with metadata on the ledger row, returning the CASH-BACKED part of the spend —
// the only part that may fund a royalty (B9.3: a browser-chat pooled serve, like an agent's settle).
func (s *DualTokenStore) SpendLXCMeta(ctx context.Context, workspaceID string, lxcAmount int64, description string, metadata map[string]interface{}) (cashBacked int64, err error) {
	if lxcAmount <= 0 {
		return 0, errors.New("economy: spend amount must be positive")
	}
	if s.pool == nil {
		return 0, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("economy: begin spend: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	bal, minted, spent, err := readLXCBalance(ctx, tx, workspaceID)
	if err != nil {
		return 0, err
	}
	// B32.11: every caller of this is a model call, so it carries the plan's platform fee, in this transaction.
	bps, err := s.platformFeeBPS(ctx, tx, workspaceID)
	if err != nil {
		return 0, err
	}
	fee := PlatformFee(lxcAmount, bps)
	if bal < lxcAmount+fee {
		return 0, ErrInsufficientLXC
	}
	// B19.13: every caller of this is the workspace's own spending, so it cannot use what its agents hold.
	if err := requireUnallocated(ctx, tx, workspaceID, bal, lxcAmount+fee); err != nil {
		return 0, err
	}
	newBal := bal - lxcAmount // exact integer µLXC
	if err := insertLXCLedger(ctx, tx, workspaceID, -lxcAmount, newBal,
		LXCTypeSpend, description, metadata); err != nil {
		return 0, err
	}
	if err := insertPlatformFee(ctx, tx, workspaceID, fee, newBal-fee, bps, lxcAmount, chargeRequestFrom(ctx)); err != nil {
		return 0, err
	}
	if err := writeLXCBalance(ctx, tx, workspaceID, newBal-fee, minted, spent+lxcAmount+fee); err != nil {
		return 0, err
	}
	// A direct spend consumes backing exactly as a settled reservation does. Without this the
	// invariant cash_backed <= balance breaks and a LATER settle mints against backing this spend
	// already used. `bal` is the pre-spend balance and carries no hold on this path.
	fromCash, err := consumeCashBacked(ctx, tx, workspaceID, bal, lxcAmount)
	if err != nil {
		return 0, err
	}
	// The fee consumes backing too, after the spend; only the spend's part may fund a royalty.
	if _, err := consumeCashBacked(ctx, tx, workspaceID, newBal, fee); err != nil {
		return 0, err
	}
	return fromCash, tx.Commit(ctx)
}

// CreditLXCTx credits LXC WITHOUT spending LENS — the fiat-purchase path (U18b):
// a PAID Stripe top-up mints LXC directly. It is the LXC-credit half of
// ConvertLENStoLXC with NO LENS debit, recorded as LXCTypePurchase. It runs
// inside the CALLER's transaction so a billing idempotency claim (the
// lxc_purchases INSERT ... ON CONFLICT) and this credit commit ATOMICALLY — the
// money guarantee. Returns the new LXC balance. The caller owns Begin/Commit/
// Rollback (and must never let billing write lxc_ledger/lxc_balances directly).
func (s *DualTokenStore) CreditLXCTx(ctx context.Context, tx pgx.Tx, workspaceID string, lxcAmount int64, reason string, metadata map[string]interface{}) (int64, error) {
	return s.creditLXCTxTyped(ctx, tx, workspaceID, lxcAmount, LXCTypePurchase, reason, metadata)
}

// GrantLXCTx credits COMPED LXC (an admin grant, NOT a paid purchase) on the caller's tx — the exact
// same atomic ledger-row + balance move as CreditLXCTx, recorded under LXCTypeGrant so a comp is
// always distinguishable from a sale. Callers must never write lxc_ledger/lxc_balances directly.
func (s *DualTokenStore) GrantLXCTx(ctx context.Context, tx pgx.Tx, workspaceID string, lxcAmount int64, reason string, metadata map[string]interface{}) (int64, error) {
	return s.creditLXCTxTyped(ctx, tx, workspaceID, lxcAmount, LXCTypeGrant, reason, metadata)
}

// creditLXCTxTyped is the shared atomic credit kernel: read balance → insert one lxc_ledger row of
// `ledgerType` → write the new balance, all on the caller's tx (ledger + balance move together or not
// at all). Integer µLXC throughout — no float. CreditLXCTx (purchase) and GrantLXCTx (admin grant)
// differ ONLY in the self-describing ledger type.
func (s *DualTokenStore) creditLXCTxTyped(ctx context.Context, tx pgx.Tx, workspaceID string, lxcAmount int64, ledgerType, reason string, metadata map[string]interface{}) (int64, error) {
	if lxcAmount <= 0 {
		return 0, errors.New("economy: credit amount must be positive")
	}
	bal, minted, spent, err := readLXCBalance(ctx, tx, workspaceID)
	if err != nil {
		return 0, err
	}
	newBal := bal + lxcAmount // exact integer µLXC
	metadata, testFunded := lotFunding(ledgerType, lxcAmount, metadata)
	if err := insertLXCLedger(ctx, tx, workspaceID, lxcAmount, newBal,
		ledgerType, reason, metadata); err != nil {
		return 0, err
	}
	if err := writeLXCBalance(ctx, tx, workspaceID, newBal, minted+lxcAmount, spent); err != nil {
		return 0, err
	}
	if err := addTestFunded(ctx, tx, workspaceID, testFunded); err != nil {
		return 0, err
	}
	// ⚠ ONLY A REAL PURCHASE ADDS BACKING. creditLXCTxTyped serves BOTH purchase and admin_grant —
	// they differ only in the ledger type — so the type is the ONE thing that decides here. A grant
	// credits spendable LXC and adds no cash to the system — nor does the test-funded part of a purchase (B30.25).
	if ledgerType == LXCTypePurchase {
		if err := addCashBacked(ctx, tx, workspaceID, lxcAmount-testFunded); err != nil {
			return 0, err
		}
	}
	return newBal, nil
}

// CreditLXC is the standalone (own-transaction) form of CreditLXCTx, for callers
// that are not already inside a tx. Mirrors the convert path's tx envelope.
func (s *DualTokenStore) CreditLXC(ctx context.Context, workspaceID string, lxcAmount int64, reason string, metadata map[string]interface{}) (int64, error) {
	if lxcAmount <= 0 {
		return 0, errors.New("economy: credit amount must be positive")
	}
	if s.pool == nil {
		return 0, nil // no-DB path (pure-arithmetic unit tests)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("economy: begin credit: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	newBal, err := s.CreditLXCTx(ctx, tx, workspaceID, lxcAmount, reason, metadata)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("economy: commit credit: %w", err)
	}
	return newBal, nil
}

// GrantLXC credits a COMPED admin grant to a workspace in its own transaction — the ledger row and the
// balance update commit atomically and roll back together on any error. Same shape as CreditLXC, but
// the row is recorded under LXCTypeGrant ("admin_grant"), never LXCTypePurchase. Positive-amount is
// enforced in the shared kernel. Never write lxc_ledger/lxc_balances directly.
func (s *DualTokenStore) GrantLXC(ctx context.Context, workspaceID string, lxcAmount int64, reason string, metadata map[string]interface{}) (int64, error) {
	if lxcAmount <= 0 {
		return 0, errors.New("economy: credit amount must be positive")
	}
	if s.pool == nil {
		return 0, nil // no-DB path (pure-arithmetic unit tests)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("economy: begin grant: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	newBal, err := s.GrantLXCTx(ctx, tx, workspaceID, lxcAmount, reason, metadata)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("economy: commit grant: %w", err)
	}
	return newBal, nil
}

// GrantLXCUpTo grants each of workspaceIDs whose balance not held by its agents and pots is below target
// the difference (B17.268: counting what the agents hold, a workspace whose agents had kept 1,000 LXC
// across nights was "restored" to nothing it could fund an agent with), in ONE
// transaction of set-based statements however many workspaces there are (B26.1: a synthetic reset of
// thousands). Each grant is what GrantLXC writes — one admin_grant lxc_ledger row (funding "grant") and
// the balance, lifetime minted and test-funded part moved with it — so the ledger cannot tell them apart.
// It returns how many workspaces were granted.
func (s *DualTokenStore) GrantLXCUpTo(ctx context.Context, workspaceIDs []string, target int64, reason string, metadata map[string]interface{}) (int, error) {
	if target <= 0 {
		return 0, errors.New("economy: grant target must be positive")
	}
	if s.pool == nil || len(workspaceIDs) == 0 {
		return 0, nil
	}
	meta, _ := lotFunding(LXCTypeGrant, target, metadata)
	metaJSON, err := dbjson.Marshal(meta)
	if err != nil {
		return 0, fmt.Errorf("economy: marshal lxc ledger metadata: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("economy: begin grant: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO lxc_balances (workspace_id, balance, lifetime_minted, lifetime_spent)
		SELECT id, 0, 0, 0 FROM unnest($1::text[]) AS id ON CONFLICT (workspace_id) DO NOTHING`, workspaceIDs); err != nil {
		return 0, fmt.Errorf("economy: ensure lxc balance rows: %w", err)
	}
	// The balance row is locked, the ledger row written and the balance moved in one statement; the
	// test-funded clamp is writeLXCBalance's.
	tag, err := tx.Exec(ctx, `
		WITH short AS (
		    SELECT b.workspace_id, $2::bigint + h.held AS goal, $2::bigint + h.held - b.balance AS delta
		    FROM lxc_balances b
		    CROSS JOIN LATERAL (SELECT COALESCE(sum(a.balance_ulxc), 0)::bigint AS held FROM agent_account_balances a
		        WHERE a.workspace_id = b.workspace_id AND (a.account LIKE 'agent:%' OR a.account LIKE 'pot:%')) h
		    WHERE b.workspace_id = ANY($1::text[]) AND b.balance < $2::bigint + h.held
		    FOR UPDATE OF b
		), ledger AS (
		    INSERT INTO lxc_ledger (workspace_id, amount, balance_after, type, description, metadata)
		    SELECT workspace_id, delta, goal, $3, $4, $5::jsonb FROM short
		)
		UPDATE lxc_balances b
		SET balance = s.goal, lifetime_minted = b.lifetime_minted + s.delta, updated_at = NOW(),
		    test_funded_ulxc = CASE WHEN b.test_funded_ulxc <= s.goal THEN b.test_funded_ulxc ELSE LEAST(b.test_funded_ulxc, GREATEST(
		        s.goal + (SELECT COALESCE(sum(held_ulxc), 0) FROM lxc_reservations r WHERE r.workspace_id = b.workspace_id AND r.status = 'held'), 0)) END
		FROM short s WHERE b.workspace_id = s.workspace_id`,
		workspaceIDs, target, LXCTypeGrant, reason, metaJSON)
	if err != nil {
		return 0, fmt.Errorf("economy: grant up to %d: %w", target, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("economy: commit grant: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// GetLXCBalance returns the current LXC balance (0 for a fresh
// workspace — not an error).
func (s *DualTokenStore) GetLXCBalance(ctx context.Context, workspaceID string) (int64, error) {
	if s.pool == nil {
		return 0, nil
	}
	row := s.pool.QueryRow(ctx, `SELECT balance FROM lxc_balances WHERE workspace_id = $1`, workspaceID)
	var b int64
	if err := row.Scan(&b); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("economy: lxc balance: %w", err)
	}
	return b, nil
}

// GetLXCSnapshot returns the full LXC balance row + the USD value
// of the balance at the fixed peg.
func (s *DualTokenStore) GetLXCSnapshot(ctx context.Context, workspaceID string) (LXCSnapshot, error) {
	snap := LXCSnapshot{WorkspaceID: workspaceID}
	if s.pool == nil {
		return snap, nil
	}
	row := s.pool.QueryRow(ctx, `
		SELECT balance, lifetime_minted, lifetime_spent
		FROM lxc_balances WHERE workspace_id = $1`, workspaceID)
	if err := row.Scan(&snap.Balance, &snap.LifetimeMinted, &snap.LifetimeSpent); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return LXCSnapshot{}, fmt.Errorf("economy: lxc snapshot: %w", err)
		}
	}
	// SEC-2 site #6 (USD-value display). Balance is conserved µLXC; LXCUSDValue
	// ($0.10, Tier-2 peg) is a float. µLXC × (USD/LXC) = µUSD (1e-6 USD, same 1e6
	// scale). This is a DISPLAY of value the workspace holds, so it rounds DOWN
	// (MulFloor) — never overstate the balance's worth.
	snap.USDValue = mining.MulFloor(snap.Balance, LXCUSDValue)
	return snap, nil
}

// LXCLedgerEntry is one row of the fiat LXC ledger — µLXC, integer. Mirrors mining.LedgerEntry with _ulxc
// units. amount is +credit / −spend; balance_after is the running balance after the row.
type LXCLedgerEntry struct {
	ID           string                 `json:"id"`
	WorkspaceID  string                 `json:"workspace_id"`
	Amount       int64                  `json:"amount_ulxc"`
	BalanceAfter int64                  `json:"balance_after_ulxc"`
	Type         string                 `json:"type"`
	Description  string                 `json:"description"`
	Metadata     map[string]interface{} `json:"metadata"`
	CreatedAt    time.Time              `json:"created_at"`
}

// GetLXCHistory returns the workspace's LXC ledger rows, newest-first, paginated (limit+offset). It mirrors
// the LENS tokens/history read (mining.LedgerStore.GetHistory) for the fiat ledger, and is the FIRST reader
// of lxc_ledger (all prior access was INSERT-only). Scope is intra-tenant (WHERE workspace_id = $1). Same
// clamp policy as the LENS ledger: default 20, max 200, offset floored at 0. There is no counterparty-masking
// step (the LXC ledger types convert_from_lens|spend|purchase|admin_grant stamp no counterparty).
func (s *DualTokenStore) GetLXCHistory(ctx context.Context, workspaceID string, limit, offset int) ([]LXCLedgerEntry, error) {
	if s.pool == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, amount, balance_after, type, description, metadata, created_at
		FROM lxc_ledger
		WHERE workspace_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3`, workspaceID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("economy: query lxc history: %w", err)
	}
	defer rows.Close()
	var out []LXCLedgerEntry
	for rows.Next() {
		var e LXCLedgerEntry
		var meta []byte
		if err := rows.Scan(&e.ID, &e.WorkspaceID, &e.Amount, &e.BalanceAfter,
			&e.Type, &e.Description, &meta, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("economy: scan lxc history: %w", err)
		}
		if len(meta) > 0 {
			_ = json.Unmarshal(meta, &e.Metadata)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ─── tx-scoped SQL helpers ───────────────────────
// These mirror the LENS-side helpers in internal/mining but live
// here because the conversion tx spans both token schemas and
// the mining helpers are package-private.

func readLXCBalance(ctx context.Context, tx pgx.Tx, workspaceID string) (bal, minted, spent int64, err error) {
	if _, err = tx.Exec(ctx, `
		INSERT INTO lxc_balances (workspace_id, balance, lifetime_minted, lifetime_spent)
		VALUES ($1, 0, 0, 0) ON CONFLICT (workspace_id) DO NOTHING`, workspaceID); err != nil {
		return 0, 0, 0, fmt.Errorf("economy: ensure lxc balance row: %w", err)
	}
	row := tx.QueryRow(ctx, `
		SELECT balance, lifetime_minted, lifetime_spent
		FROM lxc_balances WHERE workspace_id = $1 FOR UPDATE`, workspaceID)
	if err = row.Scan(&bal, &minted, &spent); err != nil {
		return 0, 0, 0, fmt.Errorf("economy: read lxc balance: %w", err)
	}
	return
}

// writeLXCBalance also keeps the test-funded part (B22.1) within the balance plus the workspace's open holds:
// a spend takes the credits that are not test-funded first, and a hold, which is not a spend, takes none. The
// caller has resolved its own reservation (status no longer 'held') before writing a settle or a release.
func writeLXCBalance(ctx context.Context, tx pgx.Tx, workspaceID string, bal, minted, spent int64) error {
	_, err := tx.Exec(ctx, `
		UPDATE lxc_balances
		SET balance = $2, lifetime_minted = $3, lifetime_spent = $4, updated_at = NOW(),
		    test_funded_ulxc = CASE WHEN test_funded_ulxc <= $2::bigint THEN test_funded_ulxc ELSE LEAST(test_funded_ulxc, GREATEST(
		        $2::bigint + (SELECT COALESCE(sum(held_ulxc), 0) FROM lxc_reservations WHERE workspace_id = $1 AND status = 'held'), 0)) END
		WHERE workspace_id = $1`, workspaceID, bal, minted, spent)
	if err != nil {
		return fmt.Errorf("economy: update lxc balance: %w", err)
	}
	return nil
}

func insertLXCLedger(ctx context.Context, tx pgx.Tx, workspaceID string, delta, balanceAfter int64,
	txType, description string, metadata map[string]interface{}) error {
	meta, err := dbjson.Marshal(metadata) // JSON text on both protocols (#133)
	if err != nil {
		return fmt.Errorf("economy: marshal lxc ledger metadata: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO lxc_ledger (workspace_id, amount, balance_after, type, description, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		workspaceID, delta, balanceAfter, txType, description, meta); err != nil {
		return fmt.Errorf("economy: insert lxc ledger: %w", err)
	}
	return nil
}

// LENS-side tx helpers — the conversion debits LENS inside the
// same tx that mints LXC, so we re-implement the minimal SQL
// against lens_token_* here.

func readLENSBalance(ctx context.Context, tx pgx.Tx, workspaceID string) (bal, earned, spent int64, err error) {
	if _, err = tx.Exec(ctx, `
		INSERT INTO lens_token_balances (workspace_id, balance, lifetime_earned, lifetime_spent)
		VALUES ($1, 0, 0, 0) ON CONFLICT (workspace_id) DO NOTHING`, workspaceID); err != nil {
		return 0, 0, 0, fmt.Errorf("economy: ensure lens balance row: %w", err)
	}
	row := tx.QueryRow(ctx, `
		SELECT balance, lifetime_earned, lifetime_spent
		FROM lens_token_balances WHERE workspace_id = $1 FOR UPDATE`, workspaceID)
	if err = row.Scan(&bal, &earned, &spent); err != nil {
		return 0, 0, 0, fmt.Errorf("economy: read lens balance: %w", err)
	}
	return
}

func writeLENSBalance(ctx context.Context, tx pgx.Tx, workspaceID string, bal, earned, spent int64) error {
	_, err := tx.Exec(ctx, `
		UPDATE lens_token_balances
		SET balance = $2, lifetime_earned = $3, lifetime_spent = $4, updated_at = NOW()
		WHERE workspace_id = $1`, workspaceID, bal, earned, spent)
	if err != nil {
		return fmt.Errorf("economy: update lens balance: %w", err)
	}
	return nil
}

func insertLENSLedger(ctx context.Context, tx pgx.Tx, workspaceID string, delta, balanceAfter int64,
	txType, description string, metadata map[string]interface{}) error {
	meta, err := dbjson.Marshal(metadata) // JSON text on both protocols (#133)
	if err != nil {
		return fmt.Errorf("economy: marshal lens ledger metadata: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO lens_token_ledger (workspace_id, amount, balance_after, type, description, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		workspaceID, delta, balanceAfter, txType, description, meta); err != nil {
		return fmt.Errorf("economy: insert lens ledger: %w", err)
	}
	return nil
}
