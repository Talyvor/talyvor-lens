package economy

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
)

// TestRewardLoopSeam_MintFinalizeRedeem_Integration proves the contributor
// reward loop CLOSES end to end, across the two packages, using the REAL
// functions (no reimplementation):
//
//	mint (held)  → mining.CreditHeldTx     (held_balance += amount; balance unchanged)
//	finalize     → mining.FinalizeHeldTx   (held_balance -= amount; balance += amount)
//	redeem       → economy.ConvertLENStoLXC(debit balance; credit lxc_balances)
//
// It is the one test spanning the seam the column-to-column verification found
// correct-by-construction: finalize credits `balance`, convert debits `balance`,
// and convert can reach NEITHER held_balance (unfinalized income) NOR
// locked_balance (staking collateral). The money-safety property — still-held
// LENS is NOT redeemable — is asserted twice (before finalize, and with
// leftover held LENS present after a partial finalize).
//
// Rate: no conversion_rate_history row is seeded, so CurrentRate falls through
// to Phase1FloorRate (1.0) — the real Phase-1 path — making lensCost = lxc × 1.0.
func TestRewardLoopSeam_MintFinalizeRedeem_Integration(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG reward-loop seam test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	rewardLoopSchema(t, pool, ctx)

	const ws = "wsContrib"
	ledger := mining.NewLedgerStore(pool)
	engine := NewRateEngine(ledger, pool)
	dt := NewDualTokenStore(ledger, pool, engine)

	// Seed a locked (staking collateral) balance so we can prove convert never
	// touches it. (Direct write — collateral isn't part of this seam.)
	if _, err := pool.Exec(ctx, `INSERT INTO lens_token_balances (workspace_id, balance, held_balance, locked_balance, lifetime_earned, lifetime_spent)
		VALUES ($1, 0, 0, 2000000, 0, 0)`, ws); err != nil {
		t.Fatal(err)
	}

	// ── STEP 1: mint a pooled cross-tenant hit as HELD (the real held credit) ──
	mintHeld := func(amount int64) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := ledger.CreditHeldTx(ctx, tx, ws, amount, mining.TypePoolRoyaltyHeld, "pooled cross-tenant hit", nil); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("CreditHeldTx: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	mintHeld(8 * uLENS)

	bal, held, _, earned := readBalances(t, pool, ctx, ws)
	if bal != 0 || held != 8*uLENS || earned != 0 {
		t.Fatalf("after mint: balance=%v held=%v earned=%v, want 0/8.0/0 (held, not spendable)", bal, held, earned)
	}

	// ── STEP 2: HELD IS NOT REDEEMABLE (the key money-safety assertion) ──
	// balance is 0; converting 3 LXC needs 3 LENS spendable → must fail, because
	// the 8 LENS sits in held_balance, which convert cannot reach.
	if _, err := dt.ConvertLENStoLXC(ctx, ws, 3*uLXC); !errors.Is(err, ErrInsufficientLENSFor) {
		t.Fatalf("held LENS must NOT be redeemable: ConvertLENStoLXC = %v, want ErrInsufficientLENSFor", err)
	}
	bal, held, _, _ = readBalances(t, pool, ctx, ws)
	if bal != 0 || held != 8*uLENS {
		t.Fatalf("a failed conversion must move nothing: balance=%v held=%v, want 0/8.0", bal, held)
	}

	// ── STEP 3: finalize PART of it (5 of 8) — leaves 3 still held ──
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.FinalizeHeldTx(ctx, tx, ws, 5*uLENS, "holdback window elapsed", nil); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("FinalizeHeldTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	bal, held, _, earned = readBalances(t, pool, ctx, ws)
	if bal != 5*uLENS || held != 3*uLENS || earned != 5*uLENS {
		t.Fatalf("after finalize(5): balance=%v held=%v earned=%v, want 5.0/3.0/5.0", bal, held, earned)
	}

	// ── STEP 4: NOW redeem 3 LXC — succeeds against the finalized spendable LENS ──
	res, err := dt.ConvertLENStoLXC(ctx, ws, 3*uLXC)
	if err != nil {
		t.Fatalf("after finalize the same conversion must succeed: %v", err)
	}
	if res.Rate != 1.0 {
		t.Errorf("rate = %v, want 1.0 (Phase1FloorRate; no approved rate seeded)", res.Rate)
	}
	if res.LENSSpent != 3*uLENS || res.LXCMinted != 3*uLXC || res.NewLENSBalance != 2*uLENS || res.NewLXCBalance != 3*uLXC {
		t.Errorf("convert result = %+v, want LENSSpent=3 LXCMinted=3 NewLENS=2 NewLXC=3 (3 LXC × rate 1.0)", res)
	}

	// ── STEP 5: full accounting in the DB ──
	bal, held, locked, _ := readBalances(t, pool, ctx, ws)
	if bal != 2*uLENS {
		t.Errorf("spendable balance after redeem = %v, want 2.0 (5 − 3)", bal)
	}
	// HELD and LOCKED untouched by the convert: still-held LENS and collateral
	// are exactly what the finalize/seed left them.
	if held != 3*uLENS {
		t.Errorf("held_balance = %v, want 3.0 — convert must NOT touch still-held LENS", held)
	}
	if locked != 2*uLENS {
		t.Errorf("locked_balance = %v, want 2.0 — convert must NOT touch staking collateral", locked)
	}
	var lxcBal int64
	if err := pool.QueryRow(ctx, `SELECT balance FROM lxc_balances WHERE workspace_id=$1`, ws).Scan(&lxcBal); err != nil {
		t.Fatal(err)
	}
	if lxcBal != 3*uLXC {
		t.Errorf("lxc_balances = %v, want 3.0", lxcBal)
	}
	// Both ledger rows written: the LENS-side convert_to_lxc debit and the
	// LXC-side convert_from_lens credit.
	var lensDebit, lxcCredit int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM lens_token_ledger WHERE workspace_id=$1 AND type='convert_to_lxc' AND amount=-3000000`, ws).Scan(&lensDebit); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM lxc_ledger WHERE workspace_id=$1 AND type='convert_from_lens' AND amount=3000000`, ws).Scan(&lxcCredit); err != nil {
		t.Fatal(err)
	}
	if lensDebit != 1 || lxcCredit != 1 {
		t.Errorf("ledger rows: lens convert_to_lxc=%d, lxc convert_from_lens=%d, want 1/1", lensDebit, lxcCredit)
	}

	// ── STEP 6: held LENS STILL isn't spendable, definitively ──
	// balance=2, held=3, locked=2. A 4-LXC conversion needs 4 spendable LENS;
	// it must FAIL even though balance+held+locked = 7 ≥ 4 — proving held and
	// locked are NOT counted as spendable. This is the money-safety seam: only
	// finalized, spendable LENS is redeemable.
	if _, err := dt.ConvertLENStoLXC(ctx, ws, 4*uLXC); !errors.Is(err, ErrInsufficientLENSFor) {
		t.Fatalf("only spendable balance (2) is redeemable; 4 LXC must fail despite held+locked present: got %v", err)
	}
}

// rewardLoopSchema creates the minimal real schema the seam touches: the LENS
// balances/ledger (with held_balance + locked_balance), the LXC balances/ledger,
// and an empty conversion_rate_history (so CurrentRate falls to Phase1FloorRate).
func rewardLoopSchema(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS lens_token_ledger`,
		`DROP TABLE IF EXISTS lens_token_balances`,
		`DROP TABLE IF EXISTS lxc_ledger`,
		`DROP TABLE IF EXISTS lxc_balances`,
		`DROP TABLE IF EXISTS conversion_rate_history`,
		`CREATE TABLE lens_token_balances (
			workspace_id    TEXT PRIMARY KEY,
			balance         BIGINT NOT NULL DEFAULT 0,
			-- migration 0115: hand-rolled schema, so a migration-added column must be added here too.
			cash_backed_ulxc BIGINT NOT NULL DEFAULT 0,
			held_balance    BIGINT NOT NULL DEFAULT 0,
			locked_balance  BIGINT NOT NULL DEFAULT 0,
			lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent  BIGINT NOT NULL DEFAULT 0,
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE lens_token_ledger (
			id            UUID NOT NULL DEFAULT gen_random_uuid(),
			workspace_id  TEXT NOT NULL,
			amount        BIGINT NOT NULL,
			balance_after BIGINT NOT NULL,
			type          TEXT NOT NULL,
			description   TEXT NOT NULL DEFAULT '',
			metadata      JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (id, workspace_id)
		)`,
		`CREATE TABLE lxc_balances (
			workspace_id    TEXT PRIMARY KEY,
			balance         BIGINT NOT NULL DEFAULT 0,
			-- migration 0115: this fixture hand-rolls the schema, so a column added by a
			-- migration has to be added here too or the real code cannot write it.
			cash_backed_ulxc BIGINT NOT NULL DEFAULT 0, test_funded_ulxc BIGINT NOT NULL DEFAULT 0, -- 0158
			lifetime_minted BIGINT NOT NULL DEFAULT 0,
			lifetime_spent  BIGINT NOT NULL DEFAULT 0,
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE lxc_ledger (
			id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id  TEXT NOT NULL,
			amount        BIGINT NOT NULL,
			balance_after BIGINT NOT NULL,
			type          TEXT NOT NULL,
			description   TEXT NOT NULL DEFAULT '',
			metadata      JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		// 0158: the LXC balance writer reads the workspace's open holds.
		`CREATE TABLE IF NOT EXISTS lxc_reservations (reservation_id TEXT PRIMARY KEY, scoped_key_id TEXT NOT NULL,
			workspace_id TEXT NOT NULL, held_ulxc BIGINT NOT NULL, settled_ulxc BIGINT,
			status TEXT NOT NULL DEFAULT 'held' CHECK (status IN ('held','settled','released')),
			requested_model TEXT, request_id TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), resolved_at TIMESTAMPTZ)`,
		`ALTER TABLE lxc_reservations ADD COLUMN IF NOT EXISTS platform_fee_bps INTEGER NOT NULL DEFAULT 0`, // 0197 (B32.11)
		`ALTER TABLE lxc_reservations ADD COLUMN IF NOT EXISTS replays INTEGER NOT NULL DEFAULT 0`,          // 0386 (B50.47)
		`CREATE TABLE conversion_rate_history (
			id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			rate          DOUBLE PRECISION NOT NULL,
			fair_rate     DOUBLE PRECISION NOT NULL DEFAULT 0,
			backing_value DOUBLE PRECISION NOT NULL DEFAULT 0,
			circulating   DOUBLE PRECISION NOT NULL DEFAULT 0,
			spread        DOUBLE PRECISION NOT NULL DEFAULT 0,
			previous_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
			approved_by   TEXT NOT NULL DEFAULT '',
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
}

func readBalances(t *testing.T, pool *pgxpool.Pool, ctx context.Context, ws string) (bal, held, locked, earned int64) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT balance, held_balance, locked_balance, lifetime_earned FROM lens_token_balances WHERE workspace_id=$1`, ws).
		Scan(&bal, &held, &locked, &earned); err != nil {
		t.Fatalf("readBalances: %v", err)
	}
	return
}
