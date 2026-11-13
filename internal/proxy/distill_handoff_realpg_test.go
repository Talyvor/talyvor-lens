package proxy

// THE DISTILL SERVE-SIDE FUNDING HANDOFF (real PG). recordDistillServes must write the consumer's
// SETTLED charge onto the basis (RecordRoyaltyBasisFunded), so the async DistillMinter mints s × that
// charge — never s × avoided_COGS (which can exceed the bill). These tests pair ALL THREE facts in one
// assertion, because that pairing IS the invariant:
//
//   · consumer settled $X            → basis.settled_charge_usd = $X → contributor minted s × $X
//   · consumer had NO reservation    → basis.settled_charge_usd = 0  → contributor minted $0
//   · consumer's delivered > hold    → the CLAMPED (settled) amount funds the mint, NOT the delivered $
//
// The value flows the way serve() composes it: settleReservation RETURNS the USD actually paid (clamped
// to the hold), and that return — not servedCostUSD — is what recordDistillServes stamps. Sourcing
// servedCostUSD instead would over-state the payment on any output overrun and mint more than was
// collected: the exact invariant #351/#353 exist to protect.

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/distillattrib"
	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/workspace"
)

// distillHandoffProxy wires the consumer's LXC reservation seam, a real distillattrib.Store as the
// attribution sink, and a real DistillMinter — so one test asserts the basis row, the mint, and the
// consumer debit together. Returns the proxy, the LXC store, the distill minter, and the pool.
func distillHandoffProxy(t *testing.T) (*Proxy, *economy.DualTokenStore, *poolroyalty.DistillMinter, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG distill handoff test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	for _, ddl := range []string{
		// consumer (LXC) + reservation seam
		`CREATE TABLE IF NOT EXISTS lxc_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0, cash_backed_ulxc BIGINT NOT NULL DEFAULT 0, test_funded_ulxc BIGINT NOT NULL DEFAULT 0, -- 0158 
			lifetime_minted BIGINT NOT NULL DEFAULT 0, lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS lxc_ledger (id BIGSERIAL PRIMARY KEY, workspace_id TEXT NOT NULL, amount BIGINT NOT NULL,
			balance_after BIGINT NOT NULL, type TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', metadata JSONB, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS agent_lxc_subbudgets (scoped_key_id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL,
			ceiling_lxc BIGINT NOT NULL DEFAULT 50000000, spent_lxc BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		// B19.1: the spend path asks whether the key belongs to an agent account (migration 0140).
		`CREATE TABLE IF NOT EXISTS agent_accounts (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, name TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS agent_account_keys (scoped_key_id TEXT PRIMARY KEY, agent_id TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		// B19.13: the workspace's own spend reads what its agents hold.
		`CREATE TABLE IF NOT EXISTS agent_postings (workspace_id TEXT NOT NULL, account TEXT NOT NULL, amount_ulxc BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS agent_account_balances (workspace_id TEXT NOT NULL, account TEXT NOT NULL, balance_ulxc BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS lxc_reservations (reservation_id TEXT PRIMARY KEY, scoped_key_id TEXT NOT NULL,
			workspace_id TEXT NOT NULL, held_ulxc BIGINT NOT NULL, settled_ulxc BIGINT,
			status TEXT NOT NULL DEFAULT 'held' CHECK (status IN ('held','settled','released')),
			requested_model TEXT, request_id TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), resolved_at TIMESTAMPTZ)`,
		`ALTER TABLE lxc_reservations ADD COLUMN IF NOT EXISTS platform_fee_bps INTEGER NOT NULL DEFAULT 0`, // 0197 (B32.11)
		`ALTER TABLE lxc_reservations ADD COLUMN IF NOT EXISTS replays INTEGER NOT NULL DEFAULT 0`,          // 0384 (B50.47)
		// contributor (LENS) side
		`CREATE TABLE IF NOT EXISTS lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0, lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS lens_token_ledger (id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL, amount BIGINT NOT NULL,
			balance_after BIGINT NOT NULL, type TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY (id, workspace_id))`,
		`CREATE TABLE IF NOT EXISTS workspaces (id TEXT PRIMARY KEY, earn_verified BOOLEAN NOT NULL DEFAULT false)`,
		`CREATE TABLE IF NOT EXISTS lxc_purchases (workspace_id TEXT NOT NULL, status TEXT NOT NULL, lxc_amount BIGINT NOT NULL DEFAULT 0, livemode BOOLEAN)`,
		// distill basis + mints — DROP+CREATE so the settled_charge_usd column (0105) is guaranteed.
		`DROP TABLE IF EXISTS distill_royalty_mints`,
		`DROP TABLE IF EXISTS distill_royalty_basis`,
		`CREATE TABLE distill_royalty_basis (owner_workspace_id TEXT NOT NULL, requester_workspace_id TEXT NOT NULL,
			content_hash TEXT NOT NULL, avoided_cogs_usd DOUBLE PRECISION NOT NULL, settled_charge_usd DOUBLE PRECISION,
			vision_model TEXT NOT NULL, vision_input_tokens INTEGER NOT NULL, vision_output_tokens INTEGER NOT NULL,
			captured_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY (owner_workspace_id, requester_workspace_id, content_hash))`,
		`CREATE TABLE distill_royalty_mints (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), request_id TEXT NOT NULL UNIQUE,
			contributor_workspace_id TEXT NOT NULL, requester_workspace_id TEXT NOT NULL, content_hash TEXT NOT NULL,
			avoided_cogs_usd DOUBLE PRECISION NOT NULL, minted_amount BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'held',
			finalize_after TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`TRUNCATE lxc_balances, lxc_ledger, agent_lxc_subbudgets, lxc_reservations, lens_token_balances, lens_token_ledger, workspaces, lxc_purchases`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}

	store := economy.NewDualTokenStore(nil, pool, nil)
	tokenLedger := mining.NewLedgerStore(pool)
	tokenLedger.SetMintVerifier(earnverify.New(false)) // U6 floor, as production
	minter := poolroyalty.NewDistillMinter(pool, tokenLedger, 0.5, func() bool { return true })

	p := &Proxy{}
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return true }, func() int { return 4096 })
	p.distillAttribSink = distillattrib.NewStore(pool) // in-package: the attribution sink under test
	return p, store, minter, pool
}

// ocrFact is a consented cross-tenant OCR distill serve (visionModel != "" ⇒ the funded-basis branch),
// owner=contributor, avoided_COGS deliberately LARGER than any charge so a mint-off-avoided regression
// would be visible.
func ocrFact(owner, hash string) distillServeFact {
	return distillServeFact{
		owner: owner, hash: hash,
		avoidedCOGSUSD: 20.0, visionModel: "gpt-4o-mini", visionInputTokens: 500, visionOutputTokens: 20,
	}
}

func basisCharge(t *testing.T, pool *pgxpool.Pool, hash string) (avoided float64, charge *float64, exists bool) {
	t.Helper()
	var a float64
	var c *float64
	err := pool.QueryRow(context.Background(),
		`SELECT avoided_cogs_usd, settled_charge_usd FROM distill_royalty_basis WHERE content_hash=$1`, hash).Scan(&a, &c)
	if err != nil {
		return 0, nil, false
	}
	return a, c, true
}

// (A) FUNDED, clamp-safe: consumer settles exactly $10 (hold ≫ delivered). The basis records the $10
// CHARGE (avoided is $20), and the sweeper mints s × $10 = $5 = 50,000,000 µLENS held — never s × $20.
func TestDistillHandoff_Funded_MintsOffSettledCharge_Integration(t *testing.T) {
	p, store, minter, pool := distillHandoffProxy(t)
	ctx := context.Background()
	const contributor, consumer = "wsA", "wsB"
	const initialLXC = int64(1_000_000_000) // $100
	fundLXC(t, pool, consumer, initialLXC)
	earnVerify(t, pool, contributor)
	if err := store.SetAgentCeiling(ctx, "agent", consumer, initialLXC); err != nil {
		t.Fatal(err)
	}

	// Large hold (maxOut 2M ⇒ hold ≫ $10), then settle the delivered value at exactly $10 (no clamp).
	rctx, blocked := p.agentReserveBlocks(ctx, "agent", consumer, "gpt-4o", "prompt held against", "rq-a", 2_000_000)
	if blocked {
		t.Fatal("well-funded reserve must not block")
	}
	settled := p.settleReservation(rctx, 10.0, "gpt-4o") // the value serve() captures and hands to recordDistillServes
	if settled != 10.0 {
		t.Fatalf("settled charge = $%v, want $10 (hold was larger, no clamp)", settled)
	}
	if bal := lxcBalance(t, pool, consumer); bal != initialLXC-100_000_000 {
		t.Fatalf("consumer LXC after $10 settle = %d, want %d", bal, initialLXC-100_000_000)
	}

	// THE HANDOFF: record the OCR serve with the SETTLED charge.
	p.recordDistillServes(rctx, consumer, workspace.LoggingMetadata, []distillServeFact{ocrFact(contributor, "h1")}, settled)

	avoided, charge, ok := basisCharge(t, pool, "h1")
	if !ok {
		t.Fatal("basis row not written")
	}
	if charge == nil || *charge != 10.0 {
		t.Fatalf("basis.settled_charge_usd = %v, want $10 (the settled charge, not NULL)", charge)
	}
	if avoided != 20.0 {
		t.Fatalf("basis.avoided_cogs_usd = %v, want $20 (avoided kept as provenance)", avoided)
	}

	if n, err := minter.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce: n=%d err=%v, want 1", n, err)
	}
	held := lensHeld(t, pool, contributor)
	if held != 50_000_000 {
		t.Fatalf("contributor LENS held = %d, want 50_000_000 (s × $10 CHARGE, NOT s × $20 avoided)", held)
	}
	if v := float64(held) / 1e6 * economy.LXCUSDValue; v > settled+1e-9 {
		t.Fatalf("ROYALTY EXCEEDS CHARGE: mint $%v > consumer charge $%v", v, settled)
	}
}

// (B) CLAMP — the money-critical proof. Delivered ($50) EXCEEDS the hold; the settle clamps to the hold,
// so the consumer paid the HELD amount. The basis must carry the CLAMPED (settled) amount and the mint
// must follow it — never the $50 delivered. Sourcing servedCostUSD here would mint > what was collected.
func TestDistillHandoff_DeliveredExceedsHold_FundsClampedNotDelivered_Integration(t *testing.T) {
	p, store, minter, pool := distillHandoffProxy(t)
	ctx := context.Background()
	const contributor, consumer = "wsA", "wsB"
	const initialLXC = int64(1_000_000_000) // $100 — plenty of balance; the HOLD is what bounds the charge
	fundLXC(t, pool, consumer, initialLXC)
	earnVerify(t, pool, contributor)
	if err := store.SetAgentCeiling(ctx, "agent", consumer, initialLXC); err != nil {
		t.Fatal(err)
	}

	// SMALL hold: maxOut=1 + short prompt ⇒ hold ≪ $50. Read the actual held from the balance drop.
	rctx, blocked := p.agentReserveBlocks(ctx, "agent", consumer, "gpt-4o", "hi", "rq-b", 1)
	if blocked {
		t.Fatal("funded reserve must not block")
	}
	heldULXC := initialLXC - lxcBalance(t, pool, consumer)
	if heldULXC <= 0 {
		t.Fatalf("hold did not debit (held=%d)", heldULXC)
	}
	heldUSD := float64(heldULXC) * economy.LXCUSDValue / 1e6

	settled := p.settleReservation(rctx, 50.0, "gpt-4o") // delivered $50 ≫ hold ⇒ clamps to the held amount
	if settled >= 50.0 {
		t.Fatalf("settle did NOT clamp: settled=$%v, delivered=$50, held=$%v — the hold must bound the charge", settled, heldUSD)
	}
	if settled != heldUSD {
		t.Fatalf("settled charge = $%v, want the clamped hold $%v (never bill above the hold)", settled, heldUSD)
	}
	if bal := lxcBalance(t, pool, consumer); bal != initialLXC-heldULXC {
		t.Fatalf("consumer LXC = %d, want %d (charged the clamped hold, not $50)", bal, initialLXC-heldULXC)
	}

	p.recordDistillServes(rctx, consumer, workspace.LoggingMetadata, []distillServeFact{ocrFact(contributor, "h2")}, settled)

	_, charge, ok := basisCharge(t, pool, "h2")
	if !ok || charge == nil {
		t.Fatal("basis row not written with a charge")
	}
	if *charge != heldUSD {
		t.Fatalf("basis.settled_charge_usd = $%v, want the CLAMPED $%v — NOT the $50 delivered (that would over-state the payment)", *charge, heldUSD)
	}

	if n, err := minter.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce: n=%d err=%v, want 1", n, err)
	}
	held := lensHeld(t, pool, contributor)
	wantMint := int64(0.5 * heldUSD / economy.LXCUSDValue * 1e6) // s × clamped charge, in µLENS
	if held != wantMint {
		t.Fatalf("contributor LENS held = %d, want %d (s × the CLAMPED $%v, not s × $50)", held, wantMint, heldUSD)
	}
	// The invariant, strict: the minted value never exceeds what the consumer actually paid.
	if v := float64(held) / 1e6 * economy.LXCUSDValue; v > settled+1e-9 {
		t.Fatalf("ROYALTY EXCEEDS CHARGE: mint $%v > consumer charge $%v (clamp violated)", v, settled)
	}
}

// (C) UNFUNDED: no reservation on the context ⇒ settle returns $0 ⇒ the handoff writes settled_charge_usd
// = 0 ⇒ the sweeper skips the row and mints nothing. The consumer paid nothing; the contributor earns
// nothing. Fail-closed by construction.
func TestDistillHandoff_NoReservation_WritesZero_MintsNothing_Integration(t *testing.T) {
	p, _, minter, pool := distillHandoffProxy(t)
	ctx := context.Background()
	const contributor, consumer = "wsA", "wsB"
	fundLXC(t, pool, consumer, 100_000_000) // has balance, but a plain key never reserved against it
	earnVerify(t, pool, contributor)        // contributor CAN earn — only the funding gate can stop the mint

	settled := p.settleReservation(ctx, 10.0, "gpt-4o") // NO reservation on ctx ⇒ $0 charged
	if settled != 0 {
		t.Fatalf("no reservation ⇒ settled=$%v, want $0", settled)
	}
	p.recordDistillServes(ctx, consumer, workspace.LoggingMetadata, []distillServeFact{ocrFact(contributor, "h3")}, settled)

	_, charge, ok := basisCharge(t, pool, "h3")
	if !ok {
		t.Fatal("basis row not written")
	}
	if charge == nil || *charge != 0 {
		t.Fatalf("basis.settled_charge_usd = %v, want 0 (a row the sweeper skips — never a charge we cannot prove)", charge)
	}
	if n, err := minter.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("RunOnce: n=%d err=%v, want 0 (unfunded reuse mints nothing)", n, err)
	}
	if held := lensHeld(t, pool, contributor); held != 0 {
		t.Fatalf("contributor LENS held = %d, want 0 — an unfunded distill reuse is minting from nothing", held)
	}
	if bal := lxcBalance(t, pool, consumer); bal != 100_000_000 {
		t.Fatalf("consumer LXC = %d, want 100_000_000 (never charged)", bal)
	}
}
