// Package economy implements the LENS token marketplace and
// staking surface. Sits on top of internal/mining's LedgerStore
// — every LENS movement still flows through the canonical
// ledger; this package just adds the marketplace state machine
// and the staking yield calculator.
package economy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
)

// ─── constants ───────────────────────────────────

const (
	// TalyvorFeeRate is the platform fee on marketplace trades —
	// 5% as the spec mandates.
	TalyvorFeeRate = 0.05

	// TalyvorWorkspace is the synthetic workspace ID that
	// accumulates marketplace fees. Stays inside the ledger
	// like any other workspace so the supply accounting works.
	TalyvorWorkspace = "talyvor"

	// TalyvorSeedWorkspace is the dedicated platform owner for L·seed warm-start
	// cache entries. Single-purpose and DELIBERATELY never earn_verified and never
	// the subject of an lxc_purchase, so earnverify.MayEarn always returns false for
	// it → both royalty mint paths (cache + distill) credit it ZERO at the held-ledger
	// verifyEarn chokepoint. Kept distinct from TalyvorWorkspace (which earns
	// marketplace fees) so the never-verified invariant is isolated + auditable.
	// Seeded entries owned by this id provably mint nothing. NOT earn_verified, NO
	// lxc_purchase — ever.
	TalyvorSeedWorkspace = "talyvor-seed"

	// MinListingAmount is the floor for marketplace listings, in µLENS (SEC-2).
	// Below this we discourage dust orders. 1_000_000 µLENS = 1 LENS.
	MinListingAmount int64 = 1_000_000

	// LENSPerUSD is the published peg: 1 LENS = $0.10. Caller
	// code uses this to convert between USD intent and LENS
	// amount where needed (e.g. the LENS-discount burn path).
	LENSPerUSD = 10.0
)

// Lock period → APY mapping. Hardcoded so a misconfigured
// deployment can't accidentally pay 100% APY on a 30-day lock.
const (
	APY30  = 0.05
	APY90  = 0.12
	APY180 = 0.20
)

// allowedLockDays gates Stake — anything not in this set is
// rejected at the API boundary.
var apyByLock = map[int]float64{
	30:  APY30,
	90:  APY90,
	180: APY180,
}

// ─── status constants ────────────────────────────

const (
	ListingActive    = "active"
	ListingFilled    = "filled"
	ListingCancelled = "cancelled"
)

// ─── errors ──────────────────────────────────────

var (
	ErrInsufficientBalance = errors.New("economy: insufficient balance")
	ErrListingNotFound     = errors.New("economy: listing not found")
	ErrListingNotActive    = errors.New("economy: listing not active")
	ErrNotSeller           = errors.New("economy: only the seller can cancel this listing")
	ErrInvalidLockDays     = errors.New("economy: lock_days must be 30, 90, or 180")
	ErrStakeLocked         = errors.New("economy: stake is still in lock period")
	ErrPositionNotFound    = errors.New("economy: stake position not found")
)

// ─── types ───────────────────────────────────────

// MarketplaceListing mirrors one row of marketplace_listings. Amount is integer
// µLENS (SEC-2). PriceUSD/MinBuyUSD are Tier-2/3 USD values (stay float64).
type MarketplaceListing struct {
	ID        string     `json:"id"`
	SellerID  string     `json:"seller_id"`
	Amount    int64      `json:"amount_ulens"`
	PriceUSD  float64    `json:"price_usd"`
	MinBuyUSD float64    `json:"min_buy_usd"`
	Status    string     `json:"status"`
	FilledAt  *time.Time `json:"filled_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// MarketplaceTrade mirrors one row of marketplace_trades. Amount/TalyvorFee are
// integer µLENS (SEC-2). PriceUSD is a Tier-3 USD value (stays float64).
type MarketplaceTrade struct {
	ID         string    `json:"id"`
	ListingID  string    `json:"listing_id"`
	BuyerID    string    `json:"buyer_id"`
	SellerID   string    `json:"seller_id"`
	Amount     int64     `json:"amount_ulens"`
	PriceUSD   float64   `json:"price_usd"`
	TalyvorFee int64     `json:"talyvor_fee_ulens"`
	CreatedAt  time.Time `json:"created_at"`
}

// StakePosition mirrors one row of stake_positions, augmented at read time with
// the live AccruedYield. Amount/AccruedYield are integer µLENS (SEC-2); APY is a
// Tier-2 rate (stays float64).
type StakePosition struct {
	ID           string    `json:"id"`
	WorkspaceID  string    `json:"workspace_id"`
	Amount       int64     `json:"amount_ulens"`
	LockDays     int       `json:"lock_days"`
	APY          float64   `json:"apy"`
	StartedAt    time.Time `json:"started_at"`
	UnlocksAt    time.Time `json:"unlocks_at"`
	AccruedYield int64     `json:"accrued_yield_ulens"`
}

// EconomyStats backs /v1/economy/stats. Supply/burned/staked are integer µLENS
// (SEC-2); AvgPriceUSD is a Tier-3 USD value (stays float64).
type EconomyStats struct {
	TotalSupply       int64   `json:"total_supply_ulens"`
	CirculatingSupply int64   `json:"circulating_supply_ulens"`
	TotalBurned       int64   `json:"total_burned_ulens"`
	TotalStaked       int64   `json:"total_staked_ulens"`
	MarketListings    int     `json:"market_listings"`
	AvgPriceUSD       float64 `json:"avg_price_usd"`
}

// ─── pgxDB shim ──────────────────────────────────

// pgxDB mirrors the subset of pgxpool.Pool we need. Tests use
// pgxmock through this interface.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ─── MarketplaceStore ────────────────────────────

// MarketplaceStore owns marketplace_listings + marketplace_trades
// + stake_positions. Holds a reference to mining.LedgerStore so
// every LENS movement still flows through the canonical ledger.
type MarketplaceStore struct {
	ledger *mining.LedgerStore
	pool   pgxDB
}

// NewMarketplaceStore wraps a real pool. Pass `nil` ledger to
// disable LENS movements (useful for read-only tests).
func NewMarketplaceStore(ledger *mining.LedgerStore, pool *pgxpool.Pool) *MarketplaceStore {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newMarketplaceStore(ledger, db)
}

func newMarketplaceStore(ledger *mining.LedgerStore, db pgxDB) *MarketplaceStore {
	return &MarketplaceStore{ledger: ledger, pool: db}
}

// ─── listing lifecycle ───────────────────────────

// CreateListing reserves the LENS by debiting the seller (the
// LENS sits "in the listing" until ExecuteTrade or CancelListing
// resolves it). The ledger entry uses TypeTransferOut with the
// listing ID in metadata for auditability.
func (s *MarketplaceStore) CreateListing(ctx context.Context, l MarketplaceListing) (*MarketplaceListing, error) {
	if l.Amount < MinListingAmount {
		return nil, fmt.Errorf("economy: listing amount must be ≥ %d µLENS", MinListingAmount)
	}
	if l.PriceUSD <= 0 {
		return nil, errors.New("economy: price_usd must be positive")
	}
	if l.SellerID == "" {
		return nil, errors.New("economy: seller_id required")
	}

	if s.pool == nil {
		// No-DB path (in-memory test mode): use standalone Debit for
		// the balance check, then return a synthetic listing ID.
		if err := s.ledger.Debit(ctx, l.SellerID, l.Amount,
			"marketplace_listing", "marketplace listing reservation", nil); err != nil {
			if errors.Is(err, mining.ErrInsufficientBalance) {
				return nil, ErrInsufficientBalance
			}
			return nil, fmt.Errorf("economy: reserve listing: %w", err)
		}
		l.ID = fmt.Sprintf("list_%d", time.Now().UnixNano())
		l.Status = ListingActive
		l.CreatedAt = time.Now().UTC()
		return &l, nil
	}

	// Full DB path: Debit + INSERT listing run inside one transaction so
	// a failure at either step leaves the seller balance fully consistent.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("economy: begin listing tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.ledger.DebitTx(ctx, tx, l.SellerID, l.Amount,
		"marketplace_listing", "marketplace listing reservation", nil); err != nil {
		if errors.Is(err, mining.ErrInsufficientBalance) {
			return nil, ErrInsufficientBalance
		}
		return nil, fmt.Errorf("economy: reserve listing: %w", err)
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO marketplace_listings (seller_id, amount, price_usd, min_buy_usd)
		VALUES ($1, $2, $3, $4)
		RETURNING id, status, created_at`,
		l.SellerID, l.Amount, l.PriceUSD, l.MinBuyUSD)
	if err := row.Scan(&l.ID, &l.Status, &l.CreatedAt); err != nil {
		return nil, fmt.Errorf("economy: insert listing: %w", err)
	}
	return &l, tx.Commit(ctx)
}

// GetListings returns active listings cheapest-first.
func (s *MarketplaceStore) GetListings(ctx context.Context, limit int) ([]MarketplaceListing, error) {
	if s.pool == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, seller_id, amount, price_usd, min_buy_usd, status, filled_at, created_at
		FROM marketplace_listings
		WHERE status = $1
		ORDER BY price_usd ASC, created_at ASC
		LIMIT $2`, ListingActive, limit)
	if err != nil {
		return nil, fmt.Errorf("economy: list listings: %w", err)
	}
	defer rows.Close()
	return scanListings(rows)
}

func scanListings(rows pgx.Rows) ([]MarketplaceListing, error) {
	var out []MarketplaceListing
	for rows.Next() {
		var l MarketplaceListing
		if err := rows.Scan(&l.ID, &l.SellerID, &l.Amount, &l.PriceUSD,
			&l.MinBuyUSD, &l.Status, &l.FilledAt, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("economy: scan listing: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// getListingForUpdate reads a listing row inside an existing transaction
// and acquires a row-level lock (SELECT ... FOR UPDATE). Callers must
// be inside a transaction. Returns ErrListingNotFound when the row is missing.
func getListingForUpdate(ctx context.Context, tx pgx.Tx, id string) (*MarketplaceListing, error) {
	row := tx.QueryRow(ctx, `
		SELECT id, seller_id, amount, price_usd, min_buy_usd, status, filled_at, created_at
		FROM marketplace_listings WHERE id = $1 FOR UPDATE`, id)
	var l MarketplaceListing
	if err := row.Scan(&l.ID, &l.SellerID, &l.Amount, &l.PriceUSD,
		&l.MinBuyUSD, &l.Status, &l.FilledAt, &l.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrListingNotFound
		}
		return nil, fmt.Errorf("economy: get listing: %w", err)
	}
	return &l, nil
}

// ExecuteTrade is the buy path. Validates the listing is active
// + amount fits, then credits the buyer (95%) + platform (5%
// fee) + marks the listing filled + records the trade. The
// seller was already debited at listing time so we don't touch
// their balance here.
//
// The trade is "all-or-nothing" — the spec doesn't define
// partial fills, so a buyer who under-pays the listing amount
// just buys less LENS and the listing closes filled.
//
// TOCTOU safety: the listing row is read inside the transaction
// with SELECT ... FOR UPDATE so concurrent buyers serialize at the
// DB level. The UPDATE also carries AND status='active' as a
// defence-in-depth guard: if RowsAffected==0, the race was lost
// and the transaction rolls back without issuing any credits.
func (s *MarketplaceStore) ExecuteTrade(ctx context.Context, listingID, buyerID string, amountUSD float64) (*MarketplaceTrade, error) {
	if buyerID == "" {
		return nil, errors.New("economy: buyer_id required")
	}
	if !(amountUSD > 0) || math.IsInf(amountUSD, 0) { // B50.1: NaN and Inf are not positive amounts
		return nil, errors.New("economy: amount_usd must be positive")
	}
	if s.pool == nil {
		return nil, errors.New("economy: marketplace requires DB")
	}

	// All validation + mutations run inside one transaction. The listing row
	// is locked with SELECT ... FOR UPDATE so that two concurrent buyers
	// block on the same row: the loser sees status='filled' and returns
	// ErrListingNotActive before any credits are issued.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("economy: begin trade tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	listing, err := getListingForUpdate(ctx, tx, listingID)
	if err != nil {
		return nil, err
	}
	if listing.Status != ListingActive {
		return nil, ErrListingNotActive
	}
	if listing.MinBuyUSD > 0 && amountUSD < listing.MinBuyUSD {
		return nil, fmt.Errorf("economy: amount_usd below min_buy_usd (%v)", listing.MinBuyUSD)
	}
	if listing.SellerID == buyerID {
		return nil, errors.New("economy: cannot buy own listing")
	}

	// SEC-2 site #2 (marketplace trade) — see tradeSplit for the conservation
	// argument. lensAmount = netToBuyer + fee is the LENS the buyer bought.
	lensAmount, fee, netToBuyer, unsold := tradeSplit(listing.Amount, listing.PriceUSD, amountUSD)
	priceActual := listing.PriceUSD

	meta := map[string]interface{}{
		"listing_id": listing.ID,
		"seller_id":  listing.SellerID,
		"price_usd":  priceActual,
	}

	// Credit buyer (net).
	if err := s.ledger.CreditTx(ctx, tx, buyerID, netToBuyer, "marketplace_buy",
		"marketplace purchase", meta); err != nil {
		return nil, fmt.Errorf("economy: credit buyer: %w", err)
	}

	// Credit platform fee.
	if fee > 0 {
		feeMeta := map[string]interface{}{
			"listing_id": listing.ID,
			"buyer_id":   buyerID,
		}
		if err := s.ledger.CreditTx(ctx, tx, TalyvorWorkspace, fee, "marketplace_fee",
			"marketplace 5% fee", feeMeta); err != nil {
			return nil, fmt.Errorf("economy: credit fee: %w", err)
		}
	}

	// Refund unsold portion to seller (unsold from tradeSplit).
	if unsold > 0 {
		if err := s.ledger.CreditTx(ctx, tx, listing.SellerID, unsold, "marketplace_unsold_refund",
			"unsold portion of listing refunded", meta); err != nil {
			return nil, fmt.Errorf("economy: refund unsold: %w", err)
		}
	}

	// Mark listing filled. AND status='active' is defence-in-depth: with FOR
	// UPDATE the status cannot have changed, but the guard catches any edge
	// case where isolation semantics don't fully serialize.
	tag, err := tx.Exec(ctx,
		`UPDATE marketplace_listings SET status = $1, filled_at = NOW()
		 WHERE id = $2 AND status = 'active'`,
		ListingFilled, listing.ID)
	if err != nil {
		return nil, fmt.Errorf("economy: mark filled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrListingNotActive // lost the race
	}

	trade := MarketplaceTrade{
		ListingID:  listing.ID,
		BuyerID:    buyerID,
		SellerID:   listing.SellerID,
		Amount:     lensAmount,
		PriceUSD:   priceActual,
		TalyvorFee: fee,
	}
	tradeRow := tx.QueryRow(ctx, `
		INSERT INTO marketplace_trades (listing_id, buyer_id, seller_id, amount, price_usd, talyvor_fee)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at`,
		trade.ListingID, trade.BuyerID, trade.SellerID,
		trade.Amount, trade.PriceUSD, trade.TalyvorFee)
	if err := tradeRow.Scan(&trade.ID, &trade.CreatedAt); err != nil {
		return nil, fmt.Errorf("economy: insert trade: %w", err)
	}
	return &trade, tx.Commit(ctx)
}

// CancelListing releases the seller's reservation. Only the
// original seller can cancel — anyone else gets ErrNotSeller.
//
// TOCTOU safety: the listing row is read inside the transaction with
// SELECT ... FOR UPDATE. If a concurrent ExecuteTrade fills the listing
// between the read and this cancel's commit, the loser (whichever
// serializes second) sees the updated status and returns ErrListingNotActive
// without issuing a refund — preventing the double-refund fund-leak.
func (s *MarketplaceStore) CancelListing(ctx context.Context, listingID, workspaceID string) error {
	if s.pool == nil {
		return nil
	}

	// UPDATE listing + Credit refund run inside one transaction. The listing
	// row is locked with SELECT ... FOR UPDATE to serialize concurrent
	// ExecuteTrade/CancelListing calls and eliminate the double-refund race.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: begin cancel tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	listing, err := getListingForUpdate(ctx, tx, listingID)
	if err != nil {
		return err
	}
	if listing.SellerID != workspaceID {
		return ErrNotSeller
	}
	if listing.Status != ListingActive {
		return ErrListingNotActive
	}

	// AND status='active' guard: defence-in-depth (see ExecuteTrade comment).
	tag, err := tx.Exec(ctx,
		`UPDATE marketplace_listings SET status = $1 WHERE id = $2 AND status = 'active'`,
		ListingCancelled, listingID)
	if err != nil {
		return fmt.Errorf("economy: cancel listing: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrListingNotActive // lost the race
	}
	if err := s.ledger.CreditTx(ctx, tx, listing.SellerID, listing.Amount,
		"marketplace_refund", "listing cancelled", map[string]interface{}{
			"listing_id": listingID,
		}); err != nil {
		return fmt.Errorf("economy: refund cancel: %w", err)
	}
	return tx.Commit(ctx)
}

// GetTrades returns every trade where `workspaceID` was either
// buyer or seller. Newest first.
func (s *MarketplaceStore) GetTrades(ctx context.Context, workspaceID string, limit int) ([]MarketplaceTrade, error) {
	if s.pool == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, listing_id, buyer_id, seller_id, amount, price_usd, talyvor_fee, created_at
		FROM marketplace_trades
		WHERE buyer_id = $1 OR seller_id = $1
		ORDER BY created_at DESC LIMIT $2`, workspaceID, limit)
	if err != nil {
		return nil, fmt.Errorf("economy: list trades: %w", err)
	}
	defer rows.Close()
	var out []MarketplaceTrade
	for rows.Next() {
		var t MarketplaceTrade
		if err := rows.Scan(&t.ID, &t.ListingID, &t.BuyerID, &t.SellerID,
			&t.Amount, &t.PriceUSD, &t.TalyvorFee, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("economy: scan trade: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ─── staking ─────────────────────────────────────

// Stake locks `amount` LENS for `lockDays`. Debits the workspace
// balance + INSERTs a stake_position. Returns the position with
// AccruedYield=0 (it'll grow as time passes).
func (s *MarketplaceStore) Stake(ctx context.Context, workspaceID string, amount int64, lockDays int) (*StakePosition, error) {
	apy, ok := apyByLock[lockDays]
	if !ok {
		return nil, ErrInvalidLockDays
	}
	if amount <= 0 {
		return nil, errors.New("economy: stake amount must be positive")
	}
	pos := &StakePosition{
		WorkspaceID: workspaceID,
		Amount:      amount,
		LockDays:    lockDays,
		APY:         apy,
		StartedAt:   time.Now().UTC(),
		UnlocksAt:   time.Now().Add(time.Duration(lockDays) * 24 * time.Hour),
	}

	if s.pool == nil {
		// No-DB path: use standalone Debit for the balance check.
		if err := s.ledger.Debit(ctx, workspaceID, amount, "stake",
			"LENS staked for yield", map[string]interface{}{"lock_days": lockDays}); err != nil {
			if errors.Is(err, mining.ErrInsufficientBalance) {
				return nil, ErrInsufficientBalance
			}
			return nil, fmt.Errorf("economy: debit stake: %w", err)
		}
		pos.ID = fmt.Sprintf("stake_%d", time.Now().UnixNano())
		return pos, nil
	}

	// Full DB path: Debit + INSERT stake_position run inside one transaction
	// so a failed INSERT never leaves the balance silently drained.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("economy: begin stake tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.ledger.DebitTx(ctx, tx, workspaceID, amount, "stake",
		"LENS staked for yield", map[string]interface{}{"lock_days": lockDays}); err != nil {
		if errors.Is(err, mining.ErrInsufficientBalance) {
			return nil, ErrInsufficientBalance
		}
		return nil, fmt.Errorf("economy: debit stake: %w", err)
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO stake_positions (workspace_id, amount, lock_days, apy, unlocks_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, started_at`,
		workspaceID, amount, lockDays, apy, pos.UnlocksAt)
	if err := row.Scan(&pos.ID, &pos.StartedAt); err != nil {
		return nil, fmt.Errorf("economy: insert stake: %w", err)
	}
	return pos, tx.Commit(ctx)
}

// Unstake returns the principal + accrued yield to the
// workspace once the lock period has elapsed.
func (s *MarketplaceStore) Unstake(ctx context.Context, positionID, workspaceID string) error {
	if s.pool == nil {
		return nil
	}
	// One transaction start-to-finish. The position is read UNDER `FOR UPDATE`
	// (inside the tx, not on the pool) so concurrent Unstake calls on the same
	// row serialize: the second waits for the first to commit, then its locked
	// read finds the row already deleted and returns ErrPositionNotFound — never
	// a second credit. The DELETE's RowsAffected is checked and the payout is
	// credited ONLY when THIS tx removed the row, so delete+credit commit
	// atomically and exactly once. Loss-of-funds protection is preserved: a
	// Credit failure rolls the DELETE back (the position is never removed
	// without the LENS being returned).
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: begin unstake tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, `
		SELECT id, workspace_id, amount, lock_days, apy, started_at, unlocks_at
		FROM stake_positions WHERE id = $1 FOR UPDATE`, positionID)
	var pos StakePosition
	if err := row.Scan(&pos.ID, &pos.WorkspaceID, &pos.Amount,
		&pos.LockDays, &pos.APY, &pos.StartedAt, &pos.UnlocksAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPositionNotFound
		}
		return fmt.Errorf("economy: read stake: %w", err)
	}
	if pos.WorkspaceID != workspaceID {
		return ErrPositionNotFound
	}
	if time.Now().Before(pos.UnlocksAt) {
		return ErrStakeLocked
	}
	yield := computeYield(pos.Amount, pos.APY, time.Since(pos.StartedAt))
	payout := pos.Amount + yield

	ct, err := tx.Exec(ctx, `DELETE FROM stake_positions WHERE id = $1`, positionID)
	if err != nil {
		return fmt.Errorf("economy: delete stake: %w", err)
	}
	if ct.RowsAffected() != 1 {
		// The row was already unstaked by a concurrent/prior committed call —
		// do NOT credit. Clean already-unstaked signal (handler maps it to 404).
		return ErrPositionNotFound
	}
	// ⚠ TWO ROWS, BECAUSE THIS IS TWO KINDS OF MONEY. It was one CreditTx of principal+yield under
	// type "unstake", with the split visible only inside the metadata JSON — and "unstake" is in
	// no counted supply list, so the YIELD was real LENS in a wallet that GetTotalSupply never saw
	// (found by #400's sweep, reported, unfixed then).
	//
	// The fix is NOT to count "unstake". The PRINCIPAL was already in supply when it was staked —
	// staking debits the wallet and locks it, it does not burn it — so counting the return as a
	// mint would inflate supply on every unstake, a worse error than the one being fixed. Only the
	// yield is new LENS, so only the yield is written under a counted type.
	//
	// Both rows are inside the SAME transaction as the position delete, so a wallet can never be
	// credited a principal without its yield or vice versa.
	meta := map[string]interface{}{
		"position_id": positionID,
		"principal":   pos.Amount,
		"accrued":     yield,
		"lock_days":   pos.LockDays,
	}
	if err := s.ledger.CreditTx(ctx, tx, workspaceID, pos.Amount, "unstake",
		"stake principal returned", meta); err != nil {
		return fmt.Errorf("economy: credit unstake principal: %w", err)
	}
	// A zero-yield unstake (unlocked immediately, or a 0% APY position) writes NO yield row:
	// CreditTx refuses a non-positive amount, and a 0-µLENS mint row would be noise in the
	// ledger a customer reads. payout is retained above purely as the arithmetic check that the
	// two rows sum to what the position is worth.
	if yield > 0 {
		if err := s.ledger.CreditTx(ctx, tx, workspaceID, yield, mining.TypeStakeYield,
			"stake yield accrued", meta); err != nil {
			return fmt.Errorf("economy: credit stake yield: %w", err)
		}
	}
	_ = payout // the two rows above sum to it; kept named so the invariant is readable here
	return tx.Commit(ctx)
}

// GetStakePositions returns the workspace's open positions
// with live AccruedYield computed against `now`.
func (s *MarketplaceStore) GetStakePositions(ctx context.Context, workspaceID string) ([]StakePosition, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, amount, lock_days, apy, started_at, unlocks_at
		FROM stake_positions WHERE workspace_id = $1
		ORDER BY started_at DESC`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: list stakes: %w", err)
	}
	defer rows.Close()
	var out []StakePosition
	for rows.Next() {
		var p StakePosition
		if err := rows.Scan(&p.ID, &p.WorkspaceID, &p.Amount,
			&p.LockDays, &p.APY, &p.StartedAt, &p.UnlocksAt); err != nil {
			return nil, fmt.Errorf("economy: scan stake: %w", err)
		}
		p.AccruedYield = computeYield(p.Amount, p.APY, time.Since(p.StartedAt))
		out = append(out, p)
	}
	return out, rows.Err()
}

// tradeSplit computes the conserved µLENS split of a marketplace trade (SEC-2
// site #2). Given the listing amount (µLENS), the listing price (Tier-2 USD per
// LENS) and the buyer's spend (Tier-3 USD), it returns:
//
//	lensAmount  — LENS the buyer bought (= netToBuyer + fee), capped at the listing
//	fee         — the 5% platform cut
//	netToBuyer  — LENS credited to the buyer
//	unsold      — LENS refunded to the seller
//
// The USD→LENS division rounds DOWN (FloatToMicroFloor): a payout never
// over-issues a sub-µLENS. The fee is a CHARGE out of lensAmount, so it rounds UP
// (MulCeil): the house never under-collects a sub-µLENS, and netToBuyer takes the
// exact integer remainder. By construction netToBuyer + fee == lensAmount and
// (netToBuyer + fee) + unsold == listingAmount EXACTLY (integer subtraction) — so
// a trade conserves value with ZERO drift. This is the SEC-2 conservation proof
// (see internal/economy/sec2_float_drift_test.go).
func tradeSplit(listingAmount int64, priceUSD, amountUSD float64) (lensAmount, fee, netToBuyer, unsold int64) {
	lensAmount = mining.FloatToMicroFloor(amountUSD / priceUSD)
	if lensAmount > listingAmount {
		lensAmount = listingAmount
	}
	fee = mining.MulCeil(lensAmount, TalyvorFeeRate)
	netToBuyer = lensAmount - fee
	unsold = listingAmount - lensAmount
	return
}

// computeYield = principal × APY × (elapsedDays / 365), in µLENS (SEC-2 site #3,
// staking yield). principal is conserved µLENS; APY (Tier-2 rate) and days/365
// are floats. Yield is newly-minted LENS paid to the staker, so it rounds DOWN
// (MulFloor) — the dropped sub-µLENS is simply not minted (retained by the
// protocol), so a rounding can never mint value from nothing.
func computeYield(principal int64, apy float64, elapsed time.Duration) int64 {
	days := elapsed.Hours() / 24
	if days < 0 {
		days = 0
	}
	return mining.MulFloor(principal, apy*(days/365.0))
}

// ─── EconomyStats ────────────────────────────────

// GetEconomyStats returns the public-facing rollup for
// /v1/economy/stats. Reads from both the mining ledger and the
// marketplace tables.
func (s *MarketplaceStore) GetEconomyStats(ctx context.Context) (*EconomyStats, error) {
	stats := &EconomyStats{}
	total, err := s.ledger.GetTotalSupply(ctx)
	if err != nil {
		return nil, err
	}
	stats.TotalSupply = total
	burned, err := s.ledger.GetTotalBurned(ctx)
	if err != nil {
		return nil, err
	}
	stats.TotalBurned = burned
	stats.CirculatingSupply = total - burned

	if s.pool == nil {
		return stats, nil
	}
	row := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount), 0) FROM stake_positions`)
	if err := row.Scan(&stats.TotalStaked); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("economy: total staked: %w", err)
	}

	row = s.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(AVG(price_usd), 0)
		FROM marketplace_listings WHERE status = $1`, ListingActive)
	if err := row.Scan(&stats.MarketListings, &stats.AvgPriceUSD); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("economy: market listings: %w", err)
	}
	return stats, nil
}

// Metadata destined for a jsonb column goes through dbjson.Marshal ->
// dbjson.JSONB (see internal/dbjson), which encodes correctly under both pgx
// protocols. The former local jsonMeta helper (a dead []byte escape hatch)
// was removed with #133.
