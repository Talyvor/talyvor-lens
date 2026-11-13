package proxy

// THE FUNDING INVARIANT (real PG). A cross-tenant royalty must be funded by a real payment from the
// consumer of THAT SAME request. These tests pair BOTH ledgers in one assertion — the consumer's LXC
// debit and the contributor's LENS credit — because that pairing IS the invariant:
//
//   · consumer charged $0 (plain key / no reservation / released)  → contributor minted $0
//   · consumer charged $10                                          → contributor minted ≤ $5 (s=0.5)
//
// The mint reads the SETTLED charge (what the consumer actually paid), never an independently computed
// avoided_COGS that may exceed the bill.

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/workspace"
)

// fundingProxy wires BOTH economies into one Proxy: a real DualTokenStore (the consumer's LXC charge via
// the reservation seam) AND a real poolroyalty.Minter (the contributor's LENS royalty), so a test can
// assert the consumer debit and the contributor credit together.
func fundingProxy(t *testing.T) (*Proxy, *economy.DualTokenStore, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG funding-invariant test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		// consumer (LXC) side
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
		// contributor (LENS) side + earnverify substrate
		`CREATE TABLE IF NOT EXISTS lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0, lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS lens_token_ledger (id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL, amount BIGINT NOT NULL,
			balance_after BIGINT NOT NULL, type TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY (id, workspace_id))`,
		`CREATE TABLE IF NOT EXISTS pool_royalty_mints (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), request_id TEXT NOT NULL UNIQUE,
			requester_workspace_id TEXT NOT NULL, contributor_workspace_id TEXT NOT NULL, layer TEXT NOT NULL, entry_id TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', similarity DOUBLE PRECISION NOT NULL DEFAULT 0,
			avoided_cogs_usd DOUBLE PRECISION NOT NULL DEFAULT 0, minted_amount BIGINT NOT NULL DEFAULT 0, answer_sha256 TEXT NOT NULL DEFAULT '',
			prompt_sha256 TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'held', finalize_after TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS workspaces (id TEXT PRIMARY KEY, earn_verified BOOLEAN NOT NULL DEFAULT false)`,
		`CREATE TABLE IF NOT EXISTS lxc_purchases (workspace_id TEXT NOT NULL, status TEXT NOT NULL, lxc_amount BIGINT NOT NULL DEFAULT 0, livemode BOOLEAN)`,
		`TRUNCATE lxc_balances, lxc_ledger, agent_lxc_subbudgets, lxc_reservations, lens_token_balances, lens_token_ledger, pool_royalty_mints, workspaces, lxc_purchases`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}

	store := economy.NewDualTokenStore(nil, pool, nil)
	tokenLedger := mining.NewLedgerStore(pool)
	tokenLedger.SetMintVerifier(earnverify.New(false)) // U6 floor, wired unconditionally as production
	minter := poolroyalty.NewMinter(pool, tokenLedger, 0.5, func() bool { return true })

	p := &Proxy{}
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return true }, func() int { return 4096 })
	p.SetRoyaltyMinter(minter)
	return p, store, pool
}

// ⚠ FUNDS AS A PAYING CUSTOMER. Writing `balance` alone is UNBACKED credit under migration 0115
// (no purchase ever happened), and unbacked credit funds no royalty — so these tests would be
// asserting the new refusal rather than the behaviour they exist to check. A cash-funded
// workspace is what they mean, so the fixture says so.
func fundLXC(t *testing.T, pool *pgxpool.Pool, ws string, ulxc int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1,$2,$2)
		 ON CONFLICT (workspace_id) DO UPDATE SET balance = EXCLUDED.balance,
		     cash_backed_ulxc = EXCLUDED.cash_backed_ulxc`, ws, ulxc); err != nil {
		t.Fatal(err)
	}
}

func earnVerify(t *testing.T, pool *pgxpool.Pool, ws string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, earn_verified) VALUES ($1, true) ON CONFLICT (id) DO UPDATE SET earn_verified = true`, ws); err != nil {
		t.Fatal(err)
	}
}

func lxcBalance(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var b int64
	_ = pool.QueryRow(context.Background(), `SELECT COALESCE((SELECT balance FROM lxc_balances WHERE workspace_id=$1),0)`, ws).Scan(&b)
	return b
}

func lensHeld(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var h int64
	_ = pool.QueryRow(context.Background(), `SELECT COALESCE((SELECT held_balance FROM lens_token_balances WHERE workspace_id=$1),0)`, ws).Scan(&h)
	return h
}

func pooledHitFor(consumer, contributor string) *poolroyalty.ServedHit {
	return &poolroyalty.ServedHit{
		RequestID: "rq", RequesterWorkspace: consumer, ContributorWorkspace: contributor,
		Layer: "exact", EntryID: "e", Provider: "openai", Model: "gpt-4o",
	}
}

// (A) THE HOLE: a plain-key consumer (no reservation) triggers a cross-tenant pooled hit. The consumer
// pays $0 for the request; the contributor must be minted $0. BOTH ledgers asserted together.
func TestFundingInvariant_UnfundedPlainKey_MintsZero_Integration(t *testing.T) {
	p, _, pool := fundingProxy(t)
	ctx := context.Background()
	const consumer, contributor = "wsConsumer", "wsContributor"
	fundLXC(t, pool, consumer, 100_000_000) // consumer HAS LXC, but a plain key never reserves against it
	earnVerify(t, pool, contributor)        // contributor CAN earn — so a mint is blocked ONLY by the funding gate

	hit := pooledHitFor(consumer, contributor)
	prompt, served := "a cross-tenant prompt", []byte("a shared cached response body")

	// Plain key ⇒ NO reservation on ctx ⇒ the settle finds nothing ⇒ the consumer is charged $0.
	funded := p.settlePooledServe(ctx, "", p.pricePooledServe(hit, prompt, served, 4096))
	if funded != 0 {
		t.Fatalf("plain-key consumer funded=%v, want 0 (no reservation ⇒ no charge)", funded)
	}
	p.mintPooledRoyalty(ctx, hit, prompt, served, funded, workspace.LoggingMetadata)

	// PAIRED assertion: consumer paid nothing AND contributor earned nothing.
	if bal := lxcBalance(t, pool, consumer); bal != 100_000_000 {
		t.Fatalf("consumer LXC balance = %d, want 100_000_000 (a plain-key cache hit charges $0)", bal)
	}
	if held := lensHeld(t, pool, contributor); held != 0 {
		t.Fatalf("contributor LENS held = %d, want 0 — an UNFUNDED royalty is minting from nothing", held)
	}
}

// (B) FUNDED: the consumer is charged exactly $10 (settled reservation). The contributor is minted at most
// $5 of value (s=0.5) = 50,000,000 µLENS. BOTH ledgers asserted together, and the pairing invariant
// mintValue ≤ charge is asserted directly.
func TestFundingInvariant_Charged10_MintsAtMost5_Integration(t *testing.T) {
	p, store, pool := fundingProxy(t)
	ctx := context.Background()
	const consumer, contributor = "wsConsumer", "wsContributor"
	const initialLXC = int64(1_000_000_000) // $100 — comfortably covers a large hold
	fundLXC(t, pool, consumer, initialLXC)
	earnVerify(t, pool, contributor)
	if err := store.SetAgentCeiling(ctx, "agent", consumer, initialLXC); err != nil { // raise the sub-budget so a large hold is allowed
		t.Fatal(err)
	}

	// Reserve a LARGE hold (maxOut 2M ⇒ hold ≫ $10), then SETTLE the delivered value at exactly $10 so the
	// clamp does not bite and the consumer is charged precisely $10 = 100 LXC = 100,000,000 µLXC net.
	rctx, blocked := p.agentReserveBlocks(ctx, "agent", consumer, "gpt-4o", "prompt to hold against", "rq-b", 2_000_000)
	if blocked {
		t.Fatal("well-funded reserve must not block")
	}
	if balAfterHold := lxcBalance(t, pool, consumer); balAfterHold >= initialLXC {
		t.Fatalf("hold did not debit: balance %d", balAfterHold)
	}

	funded := p.settleReservation(rctx, 10.0, "gpt-4o") // the consumer is charged exactly $10 for this request
	if funded != 10.0 {
		t.Fatalf("settled charge = $%v, want $10 (the delivered value, hold was larger)", funded)
	}
	// Consumer NET debit is exactly $10 = 100,000,000 µLXC (hold released, delivered charged).
	if bal := lxcBalance(t, pool, consumer); bal != initialLXC-100_000_000 {
		t.Fatalf("consumer LXC after settle = %d, want %d ($10 charged)", bal, initialLXC-100_000_000)
	}
	_ = store

	hit := pooledHitFor(consumer, contributor)
	p.mintPooledRoyalty(rctx, hit, "prompt to hold against", []byte("served"), funded, workspace.LoggingMetadata)

	// Contributor minted s × $10 = $5 of value = 50 LENS = 50,000,000 µLENS at the $0.10 peg.
	held := lensHeld(t, pool, contributor)
	if held != 50_000_000 {
		t.Fatalf("contributor LENS held = %d, want 50_000_000 (s × $10 = $5 at the peg)", held)
	}
	// THE PAIRING INVARIANT, asserted in DOLLARS: the royalty's worth never exceeds what the consumer paid.
	mintValueUSD := float64(held) / 1e6 * economy.LXCUSDValue // µLENS → LENS → $ at the peg
	if mintValueUSD > funded+1e-9 {
		t.Fatalf("ROYALTY EXCEEDS CHARGE: mint $%v of value > consumer charge $%v", mintValueUSD, funded)
	}
	if mintValueUSD != 5.0 {
		t.Fatalf("mint value = $%v, want $5 (half of the $10 charge)", mintValueUSD)
	}
}

// (C) WIRING: the pooled settle on a genuinely reserved pooled hit funds a real mint (proves the fix
// is not "always zero" — a paid consumer DOES fund the contributor).
func TestFundingInvariant_ResolveFundsRealMint_Integration(t *testing.T) {
	p, _, pool := fundingProxy(t)
	ctx := context.Background()
	const consumer, contributor = "wsConsumer", "wsContributor"
	fundLXC(t, pool, consumer, 200_000_000)
	earnVerify(t, pool, contributor)

	rctx, _ := p.agentReserveBlocks(ctx, "agent", consumer, "gpt-4o", "prompt to hold against", "rq-c", 4096)
	hit := pooledHitFor(consumer, contributor)
	prompt, served := "prompt to hold against", []byte("a served cached response of some length")

	// The discount is SET here rather than left at the harness default, so this proves the funding tie
	// holds on the DISCOUNTED charge — which is the version that now ships. At r=0 it would prove the
	// old arrangement and quietly stop covering the new one.
	p.SetPoolConsumerDiscount(0.30)
	funded := p.settlePooledServe(rctx, "", p.pricePooledServe(hit, prompt, served, 4096)) // settles list×(1−r) via the real path
	if funded <= 0 {
		t.Fatalf("a reserved pooled hit must settle a positive charge, got $%v", funded)
	}
	p.mintPooledRoyalty(rctx, hit, prompt, served, funded, workspace.LoggingMetadata)

	held := lensHeld(t, pool, contributor)
	if held <= 0 {
		t.Fatalf("a FUNDED pooled hit must mint a positive royalty, got %d µLENS", held)
	}
	// Invariant still holds: mint value ≤ consumer charge.
	if mintValueUSD := float64(held) / 1e6 * economy.LXCUSDValue; mintValueUSD > funded+1e-9 {
		t.Fatalf("ROYALTY EXCEEDS CHARGE: mint $%v > charge $%v", mintValueUSD, funded)
	}
}
