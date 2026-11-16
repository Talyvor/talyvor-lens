package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/testschema"
	"github.com/talyvor/lens/migrations"
)

// AN UNMEASURED ZERO REPORTED AS A MEASUREMENT — the second surface.
//
// token_events.cached is BOOLEAN NOT NULL DEFAULT false and NOTHING WRITES IT: neither
// insertTokenEventSQL nor insertCacheServeSQL names the column, and there is no UPDATE
// token_events anywhere. So `FILTER (WHERE cached)` counts zero rows on every deployment, and
// `FILTER (WHERE NOT cached)` selects every row.
//
// The REST endpoint was moved to serve_source by migration 0100, and internal/api/server.go says
// so in as many words. The MCP tools were never moved, so an assistant asking Lens what it saved
// was told, with a straight face:
//
//	get_cache_stats  → estimated_savings_usd = uncachedCost × (0 / total) = $0.00, always
//	get_cache_stats  → total_hit_rate        = 0 / total                  = 0,     always
//	get_spend_summary→ cache_hit_rate, cached_requests                    = 0,     always
//
// ⚠ THESE TESTS PROVE BY INVERSION, because "the new query returns a number" would also pass on
// the old one the moment any row happened to have cached=true. The fixture therefore sets the two
// columns in OPPOSITION:
//
//	row A: serve_source='upstream',           cached=TRUE   ← the old query's only hit; must NOT count
//	row B: serve_source='cache_hit_semantic', cached=FALSE  ← the real production shape; MUST count
//
// Under the old SQL A counts and B does not. Under the correct SQL B counts and A does not. No
// single number can satisfy both, so a green here cannot be produced by the code this replaces.
//
// ⚠ AND NOTE WHAT ROW A IS: production cannot produce it, because nothing writes cached at all.
// It exists ONLY to make the old behaviour observable. That is the legitimate use of an impossible
// fixture — proving a defect is gone — as opposed to resting a passing assertion on one.

func savingsTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_mcpsavings_%d", time.Now().UnixNano())

	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = ac.Close(ctx)
		t.Fatalf("create %s: %v", name, err)
	}
	_ = ac.Close(ctx)

	u, _ := url.Parse(admin)
	u.Path = "/" + name
	dsn := u.String()

	mc, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	if _, err := dbmigrate.Run(ctx, mc, migrations.FS); err != nil {
		_ = mc.Close(ctx)
		t.Fatalf("migrate %s: %v", name, err)
	}
	// B61.52: the accounts these tests move money for have confirmed who they are.
	if _, err := mc.Exec(ctx, testschema.ConfirmedAccountsSQL); err != nil {
		_ = mc.Close(ctx)
		t.Fatal(err)
	}
	_ = mc.Close(ctx)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, e := pgx.Connect(context.Background(), admin)
		if e != nil {
			return
		}
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = c.Close(context.Background())
	})
	return pool
}

const wsSavings = "ws-savings"

// seedOpposed writes the three rows the inversion needs. Costs are chosen so the correct answer
// and the old answer are different NUMBERS, not merely different code paths.
func seedOpposed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	rows := []struct {
		serveSource string
		cached      bool
		cost        float64
	}{
		// A — an upstream serve carrying cached=true. Impossible in production; the ONLY row the
		//     old `FILTER (WHERE cached)` would have counted.
		{"upstream", true, 3.00},
		// B — a real cache hit, cached=false, cost 0 (a served cache hit costs Talyvor nothing).
		{"cache_hit_semantic", false, 0.00},
		// C — an ordinary upstream serve. Denominator, and part of the money that was actually spent.
		{"upstream", false, 1.00},
		// D — a SECOND real cache hit. Present so the HIT RATE itself differs between the two
		//     definitions: correct = 2/4, old = 1/4. Without it the fixture gave 1/3 under BOTH,
		//     and the rate assertion passed on the broken query — a green for the wrong reason.
		{"cache_hit_exact", false, 0.00},
	}
	for i, r := range rows {
		if _, err := pool.Exec(ctx,
			`INSERT INTO token_events
			   (workspace_id, provider, model, input_tokens, output_tokens, cost_usd, serve_source, cached)
			 VALUES ($1,'openai','gpt-4o-mini',10,10,$2,$3,$4)`,
			wsSavings, r.cost, r.serveSource, r.cached); err != nil {
			t.Fatalf("seed row %d (%s): %v", i, r.serveSource, err)
		}
	}
}

func savingsCtx() context.Context {
	return auth.WithAuthContext(context.Background(), &auth.AuthContext{WorkspaceID: wsSavings})
}

// ⚠ THE HEADLINE. estimated_savings_usd must stop being structurally $0.00.
func TestGetCacheStats_SavingsIsNotStructurallyZero(t *testing.T) {
	pool := savingsTestPool(t)
	seedOpposed(t, pool)
	s := &Server{pool: pool}

	out, err := s.toolGetCacheStats(savingsCtx(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("toolGetCacheStats: %v", err)
	}
	m := out.(map[string]any)

	rate := m["total_hit_rate"].(float64)
	savings := m["estimated_savings_usd"].(float64)

	// 2 of 4 rows are real cache hits; the old query would have said 1 of 4.
	if rate <= 0 {
		t.Fatalf("total_hit_rate = %v with a cache_hit_semantic row present. This is the unmeasured "+
			"zero: FILTER (WHERE cached) counts nothing because nothing writes that column.", rate)
	}
	if wantRate := 0.5; rate < wantRate-1e-9 || rate > wantRate+1e-9 {
		t.Errorf("total_hit_rate = %v, want %v (2 cache_hit rows of 4; the old query yields 0.25)", rate, wantRate)
	}
	if savings <= 0 {
		t.Fatalf("estimated_savings_usd = $%.2f. Lens told the caller it saved NOTHING while a cache "+
			"hit was served — a number that has never been anything but zero on any deployment.", savings)
	}
	// savings = non-cache-hit spend × hit rate = (3.00 + 1.00) × 0.5
	if want := 2.00; savings < want-1e-9 || savings > want+1e-9 {
		t.Errorf("estimated_savings_usd = %v, want %v", savings, want)
	}
}

// ⚠ THE INVERSION. Row A (upstream, cached=true) must NOT be counted as a hit. Without this, the
// fix could be "count everything" and the test above would still pass.
func TestGetCacheStats_CachedTrueOnAnUpstreamRowIsNotAHit(t *testing.T) {
	pool := savingsTestPool(t)
	ctx := context.Background()
	// ONLY row A: an upstream serve with cached=true, and nothing else.
	if _, err := pool.Exec(ctx,
		`INSERT INTO token_events
		   (workspace_id, provider, model, input_tokens, output_tokens, cost_usd, serve_source, cached)
		 VALUES ($1,'openai','gpt-4o-mini',10,10,5.00,'upstream',TRUE)`, wsSavings); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := &Server{pool: pool}

	out, err := s.toolGetCacheStats(savingsCtx(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("toolGetCacheStats: %v", err)
	}
	m := out.(map[string]any)

	if rate := m["total_hit_rate"].(float64); rate != 0 {
		t.Fatalf("total_hit_rate = %v for a workspace whose only row was served by UPSTREAM. The "+
			"`cached` boolean is not the signal — serve_source is. Reading it means the reported hit "+
			"rate tracks a column no writer maintains.", rate)
	}
	if sav := m["estimated_savings_usd"].(float64); sav != 0 {
		t.Fatalf("estimated_savings_usd = $%.2f on a workspace that never had a cache hit — savings "+
			"invented from an upstream serve.", sav)
	}
}

// The same inversion on get_spend_summary, which reports cache_hit_rate and cached_requests.
func TestGetSpendSummary_HitRateFollowsServeSourceNotTheCachedColumn(t *testing.T) {
	pool := savingsTestPool(t)
	seedOpposed(t, pool)
	s := &Server{pool: pool}

	out, err := s.toolGetSpendSummary(savingsCtx(), json.RawMessage(`{"days":30}`))
	if err != nil {
		t.Fatalf("toolGetSpendSummary: %v", err)
	}
	m := out.(map[string]any)

	if got := m["cached_requests"].(int64); got != 2 {
		t.Fatalf("cached_requests = %d, want 2 — the two cache_hit_* rows (cached=false) are the hits, "+
			"and the upstream row carrying cached=true is not (the old query answers 1)", got)
	}
	if got := m["cache_hit_rate"].(float64); got <= 0 {
		t.Fatalf("cache_hit_rate = %v with a real cache hit present", got)
	}
	// Total cost must be untouched by the fix: 3.00 + 0.00 + 1.00.
	if got := m["total_cost_usd"].(float64); got < 4.0-1e-9 || got > 4.0+1e-9 {
		t.Errorf("total_cost_usd = %v, want 4.00 — the hit-rate fix must not move the money total", got)
	}
}

// ⚠ THE TWO SURFACES MUST AGREE. The whole defect is that one was fixed and the other was not, so
// the durable guard is not "MCP is correct" but "MCP and REST compute the same thing from the same
// rows". A future edit to either alone reds this.
func TestMCPAndRESTAgreeOnTheHitRateDefinition(t *testing.T) {
	pool := savingsTestPool(t)
	seedOpposed(t, pool)
	ctx := context.Background()

	// The REST definition, transcribed from internal/api/server.go's spendSummarySQL.
	var restHits int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FILTER (WHERE serve_source LIKE 'cache_hit%')
		   FROM token_events WHERE workspace_id = $1`, wsSavings).Scan(&restHits); err != nil {
		t.Fatalf("rest-shaped query: %v", err)
	}

	s := &Server{pool: pool}
	out, err := s.toolGetSpendSummary(savingsCtx(), json.RawMessage(`{"days":30}`))
	if err != nil {
		t.Fatalf("toolGetSpendSummary: %v", err)
	}
	mcpHits := out.(map[string]any)["cached_requests"].(int64)

	if mcpHits != restHits {
		t.Fatalf("MCP counts %d cache hits where REST counts %d, over identical rows. The two surfaces "+
			"disagree about what a cache hit IS — which is exactly how this defect survived migration "+
			"0100: the REST endpoint was moved to serve_source and the MCP one was left on `cached`.",
			mcpHits, restHits)
	}
}
