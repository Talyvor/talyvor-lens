package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/workspace"
	"github.com/talyvor/lens/migrations"
)

// B17.1 — the synthetic routes, through the real handlers on a migrated schema.

func syntheticDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG synthetic-workspace test")
	}
	const schema = "cmd_synthetic_realpg"
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatalf("reset schema: %v", err)
		}
	}
	if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = conn.Close(ctx)
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestSynthetic_RoutesAre404WithoutTheKey(t *testing.T) {
	r := chi.NewRouter()
	mountSyntheticRoutes(r, "", syntheticDeps{})
	for _, path := range []string{"/v1/synthetic/workspaces", "/v1/synthetic/workspaces/reset"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"count":100}`))
		req.Header.Set(syntheticKeyHeader, "anything")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("POST %s with LENS_SYNTHETIC_KEY unset = %d, want 404", path, w.Code)
		}
	}
}

func TestSynthetic_AHundredAreCreatedAndResetInOneCallEach(t *testing.T) {
	pool := syntheticDB(t)
	ctx := context.Background()
	ws := workspace.New(pool)
	dual := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountSyntheticRoutes(r, "the-key", syntheticDeps{
		workspaces: ws,
		credits:    dual,
		answers:    storedanswers.New(pool, nil),
		audit:      pool,
		mint: func(workspaceID, _ string, _ []string, _ time.Duration) (string, error) {
			return "tok-" + workspaceID, nil
		},
	})
	call := func(path, key, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set(syntheticKeyHeader, key)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	one := func(sql string, args ...any) int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}

	// A wrong key is refused, and the refusal is on the record.
	if w := call("/v1/synthetic/workspaces", "not-the-key", `{"count":100}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key = %d, want 401", w.Code)
	}
	if n := one(`SELECT count(*) FROM workspaces`); n != 0 {
		t.Fatalf("a refused call created %d workspaces", n)
	}

	// ONE call creates a hundred, each synthetic, pooling, and granted its test credits on the ledger.
	w := call("/v1/synthetic/workspaces", "the-key", `{"count":100}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body.String())
	}
	var out struct {
		Created    int             `json:"created"`
		Workspaces []syntheticUser `json:"workspaces"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Created != 100 || len(out.Workspaces) != 100 {
		t.Fatalf("create returned %d / %d workspaces (%v), want 100", out.Created, len(out.Workspaces), err)
	}
	if n := one(`SELECT count(*) FROM workspaces WHERE synthetic AND cache_poolable AND active`); n != 100 {
		t.Fatalf("%d synthetic pooling workspaces in the database, want 100", n)
	}
	if n := one(`SELECT count(*) FROM lxc_ledger WHERE type = $1 AND amount = $2 AND metadata->>'synthetic' = 'true'`,
		economy.LXCTypeGrant, syntheticCreditULXC); n != 100 {
		t.Fatalf("%d test-credit grants on the ledger, want 100 of %d µLXC", n, syntheticCreditULXC)
	}
	first := out.Workspaces[0].WorkspaceID
	if !ws.GetSynthetic(first) || out.Workspaces[0].Token != "tok-"+first {
		t.Fatalf("%s: synthetic=%v token=%q", first, ws.GetSynthetic(first), out.Workspaces[0].Token)
	}

	// The harness uses the first one: it spends credits and stores an answer.
	if err := dual.SpendLXC(ctx, first, 5_000_000, "a synthetic question"); err != nil {
		t.Fatalf("spend: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO prompt_embeddings (provider, model, prompt_hash, embedding, response, workspace_id, embedding_model)
		VALUES ('anthropic', 'claude-haiku-4-5', 'h1', array_fill(0, ARRAY[1536])::vector, 'an answer', $1, 'text-embedding-3-small')`, first); err != nil {
		t.Fatalf("store an answer: %v", err)
	}
	// B17.268 — the second has an agent that kept every credit from an earlier night.
	second := out.Workspaces[1].WorkspaceID
	kept, err := dual.CreateAgent(ctx, second, "Kept", "u-owner")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := dual.FundAgent(ctx, second, kept.ID, syntheticCreditULXC); err != nil {
		t.Fatalf("fund the agent with every credit: %v", err)
	}

	// ONE call resets all hundred: the stored answer is gone and the credits are back.
	w = call("/v1/synthetic/workspaces/reset", "the-key", ``)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"reset":100`) {
		t.Fatalf("reset = %d %s, want 200 and 100 reset", w.Code, w.Body.String())
	}
	if n := one(`SELECT count(*) FROM prompt_embeddings WHERE workspace_id = $1`, first); n != 0 {
		t.Errorf("%d stored answers remain for %s after the reset", n, first)
	}
	if bal, err := dual.GetLXCBalance(ctx, first); err != nil || bal != syntheticCreditULXC {
		t.Errorf("%s balance after reset = %d (%v), want %d", first, bal, err, syntheticCreditULXC)
	}
	if n := one(`SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1 AND type = $2 AND amount = 5000000`,
		first, economy.LXCTypeGrant); n != 1 {
		t.Errorf("%s has %d restoring grants of 5,000,000 µLXC on the ledger, want 1", first, n)
	}
	// What the agent holds stays its own, and the workspace again has 1,000 LXC to fund another agent with.
	if n := one(`SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1 AND type = $2 AND amount = $3 AND balance_after = $4`,
		second, economy.LXCTypeGrant, syntheticCreditULXC, 2*syntheticCreditULXC); n != 1 {
		t.Errorf("%s has %d restoring grants of %d µLXC to %d on the ledger, want 1", second, n, syntheticCreditULXC, 2*syntheticCreditULXC)
	}
	next, err := dual.CreateAgent(ctx, second, "Next", "u-owner")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := dual.FundAgent(ctx, second, next.ID, syntheticCreditULXC); err != nil {
		t.Errorf("after the reset funding a new agent with %d µLXC: %v", syntheticCreditULXC, err)
	}
	if n := one(`SELECT balance_ulxc FROM agent_account_balances WHERE workspace_id = $1 AND account = $2`, second, "agent:"+kept.ID); n != syntheticCreditULXC {
		t.Errorf("the agent that kept its credits holds %d µLXC after the reset, want %d", n, syntheticCreditULXC)
	}

	// Every call is on the record, the refused one included.
	if got := one(`SELECT count(*) FROM synthetic_operations`); got != 3 {
		t.Errorf("synthetic_operations holds %d calls, want 3", got)
	}
	if got := one(`SELECT count(*) FROM synthetic_operations WHERE outcome = 'unauthorized'`); got != 1 {
		t.Errorf("%d unauthorized calls recorded, want 1", got)
	}
}

// syntheticRedis is the Redis at LENS_TEST_REDIS_URL (CI's service), emptied; without it, a miniredis.
func syntheticRedis(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("LENS_TEST_REDIS_URL")
	if url == "" {
		return redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("LENS_TEST_REDIS_URL: %v", err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("empty the test Redis: %v", err)
	}
	return rdb
}

// B26.1 — 2,000 test users reset in seconds, and a reset that names 500 touches exactly those 500.
func TestSynthetic_TwoThousandResetInSeconds_ANamedResetTouchesOnlyThose(t *testing.T) {
	pool := syntheticDB(t)
	rdb := syntheticRedis(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	one := func(sql string, args ...any) int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	const spent = syntheticCreditULXC - 5_000_000
	// 2,000 test users and one real workspace, each having spent 5 LXC, stored a private answer and a
	// cached copy in Redis.
	exec(`INSERT INTO workspaces (id, name, cache_prefix, active, synthetic, cache_poolable)
		SELECT 's' || lpad(g::text, 26, '0'), 'Synthetic user', 's' || g, true, true, true FROM generate_series(1, 2000) g`)
	exec(`INSERT INTO workspaces (id, name, cache_prefix, active) VALUES ('u-real', 'A real company', 'u-real', true)`)
	exec(`INSERT INTO lxc_balances (workspace_id, balance, lifetime_minted, lifetime_spent)
		SELECT id, $1, $2, 5000000 FROM workspaces`, spent, syntheticCreditULXC)
	exec(`INSERT INTO prompt_embeddings (provider, model, prompt_hash, embedding, response, workspace_id, embedding_model)
		SELECT 'anthropic', 'claude-haiku-4-5', 'h-' || id, array_fill(0, ARRAY[1536])::vector, 'an answer', id, 'text-embedding-3-small'
		FROM workspaces`)
	var ids []string
	rows, err := pool.Query(ctx, `SELECT id FROM workspaces WHERE synthetic ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	pipe := rdb.Pipeline()
	for _, id := range append([]string{"u-real"}, ids...) {
		pipe.Set(ctx, "lens:exact:copy-"+id, "a cached answer", 0)
		pipe.Set(ctx, "lens:exact:copy-"+id+":owner", id, 0)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatalf("seed Redis: %v", err)
	}
	cached := func(id string) bool {
		n, err := rdb.Exists(ctx, "lens:exact:copy-"+id, "lens:exact:copy-"+id+":owner").Result()
		if err != nil {
			t.Fatal(err)
		}
		return n == 2
	}

	r := chi.NewRouter()
	mountSyntheticRoutes(r, "the-key", syntheticDeps{
		workspaces: workspace.New(pool),
		credits:    economy.NewDualTokenStore(nil, pool, nil),
		answers:    storedanswers.New(pool, rdb),
		audit:      pool,
	})
	reset := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/synthetic/workspaces/reset", strings.NewReader(body))
		req.Header.Set(syntheticKeyHeader, "the-key")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	grants := `SELECT count(*) FROM lxc_ledger WHERE type = 'admin_grant' AND amount = 5000000 AND balance_after = $1 AND test
		AND metadata->>'funding' = 'grant' AND workspace_id = ANY($2)`

	// Naming a real workspace among test users is refused, and nothing is reset.
	if w := reset(`{"workspaces":["` + ids[0] + `","u-real"]}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "u-real") {
		t.Fatalf("a reset naming a real workspace = %d %s, want 400 naming it", w.Code, w.Body.String())
	}
	if n := one(`SELECT count(*) FROM lxc_ledger WHERE type = 'admin_grant'`); n != 0 {
		t.Fatalf("a refused reset wrote %d grants", n)
	}

	// A reset naming 500: those 500 each get one restoring grant on the ledger and lose their answer and
	// cached copy; the other 1,500 and the real workspace are not touched.
	named, rest := ids[:500], ids[500:]
	body, _ := json.Marshal(map[string][]string{"workspaces": named})
	if w := reset(string(body)); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"reset":500`) {
		t.Fatalf("named reset = %d %s, want 200 and 500 reset", w.Code, w.Body.String())
	}
	if n := one(grants, syntheticCreditULXC, named); n != 500 {
		t.Errorf("%d of the 500 named have their restoring grant on the ledger, want 500", n)
	}
	if n := one(`SELECT count(*) FROM lxc_ledger WHERE type = 'admin_grant'`); n != 500 {
		t.Errorf("%d grants on the ledger after a reset naming 500, want 500", n)
	}
	if n := one(`SELECT count(*) FROM lxc_balances WHERE balance = $1 AND workspace_id = ANY($2)`, spent, append(rest, "u-real")); n != 1501 {
		t.Errorf("%d of the 1,500 unnamed and the real workspace kept their balance, want 1501", n)
	}
	if n := one(`SELECT count(*) FROM prompt_embeddings WHERE workspace_id = ANY($1)`, named); n != 0 {
		t.Errorf("%d answers of the named 500 remain", n)
	}
	if n := one(`SELECT count(*) FROM prompt_embeddings`); n != 1501 {
		t.Errorf("%d answers remain, want the 1,500 unnamed and the real one", n)
	}
	if cached(named[0]) || cached(named[499]) || !cached(rest[0]) || !cached(rest[1499]) || !cached("u-real") {
		t.Errorf("Redis copies after the named reset: named %v %v, unnamed %v %v, real %v; want only the named gone",
			cached(named[0]), cached(named[499]), cached(rest[0]), cached(rest[1499]), cached("u-real"))
	}

	// A reset of all 2,000 answers in under five seconds; the real workspace is still untouched.
	start := time.Now()
	w := reset(``)
	took := time.Since(start)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"reset":2000`) {
		t.Fatalf("reset of all = %d %s, want 200 and 2000 reset", w.Code, w.Body.String())
	}
	if took >= 5*time.Second {
		t.Errorf("a reset of 2,000 took %s, want under 5s", took)
	}
	t.Logf("a reset of 2,000 synthetic workspaces took %s", took)
	if n := one(grants, syntheticCreditULXC, ids); n != 2000 {
		t.Errorf("%d of 2,000 have one restoring grant on the ledger, want 2000", n)
	}
	if n := one(`SELECT count(*) FROM lxc_balances WHERE workspace_id = 'u-real' AND balance = $1`, spent); n != 1 {
		t.Error("the real workspace's balance moved")
	}
	if n := one(`SELECT count(*) FROM prompt_embeddings`); n != 1 {
		t.Errorf("%d answers remain, want only the real workspace's", n)
	}
	if keys, err := rdb.Keys(ctx, "lens:exact:*").Result(); err != nil || len(keys) != 2 || !cached("u-real") {
		t.Errorf("Redis after the full reset holds %d copy keys (%v), want only the real workspace's 2", len(keys), err)
	}
}

// B26.1 — the clean-up deletes a week-old test user with what it owns, and keeps one that shares a money
// row with a real workspace (for the crossings report), a younger test user, and the real workspace.
func TestSynthetic_PurgeDeletesWeekOldTestUsers_KeepsOneThatCrossedToARealWorkspace(t *testing.T) {
	pool := syntheticDB(t)
	rdb := syntheticRedis(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	one := func(sql string, args ...any) int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	ws := workspace.New(pool)
	dual := economy.NewDualTokenStore(nil, pool, nil)
	exec(`INSERT INTO workspaces (id, name, cache_prefix, active, synthetic, cache_poolable, created_at) VALUES
		('s-old', 'old test user', 's-old', true, true, true, now() - interval '8 days'),
		('s-old-crossed', 'old test user that paid a real one', 's-old-crossed', true, true, true, now() - interval '8 days'),
		('s-young', 'young test user', 's-young', true, true, true, now() - interval '1 day')`)
	exec(`INSERT INTO workspaces (id, name, cache_prefix, active, created_at) VALUES ('u-real', 'A real company', 'u-real', true, now() - interval '30 days')`)
	for _, id := range []string{"s-old", "s-old-crossed", "s-young", "u-real"} {
		if _, err := dual.GrantLXC(ctx, id, syntheticCreditULXC, "credits", nil); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO prompt_embeddings (provider, model, prompt_hash, embedding, response, workspace_id, embedding_model)
			VALUES ('anthropic', 'claude-haiku-4-5', $1, array_fill(0, ARRAY[1536])::vector, 'an answer', $1, 'text-embedding-3-small')`, id)
		if err := rdb.Set(ctx, "lens:exact:copy-"+id+":owner", id, 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	// A money request between two test users, and one from a test user to a real company (from before the wall).
	exec(`INSERT INTO agent_money_requests (id, from_workspace_id, from_agent_id, to_workspace_id, to_agent_id, amount_ulxc) VALUES
		('mreq-test', 's-old', 'agent-a', 's-young', 'agent-b', 1000000),
		('mreq-crossed', 's-old-crossed', 'agent-c', 'u-real', 'agent-d', 1000000)`)

	if err := purgeStaleSynthetic(ctx, ws, storedanswers.New(pool, rdb), time.Now()); err != nil {
		t.Fatalf("purge: %v", err)
	}

	if n := one(`SELECT count(*) FROM workspaces WHERE id = 's-old'`); n != 0 {
		t.Error("the week-old test user is still there")
	}
	for _, id := range []string{"s-old-crossed", "s-young", "u-real"} {
		if n := one(`SELECT count(*) FROM workspaces WHERE id = $1`, id); n != 1 {
			t.Errorf("%s was deleted", id)
		}
		if n := one(`SELECT count(*) FROM prompt_embeddings WHERE workspace_id = $1`, id); n != 1 {
			t.Errorf("%s lost its stored answer", id)
		}
		if n := one(`SELECT count(*) FROM lxc_balances WHERE workspace_id = $1 AND balance = $2`, id, syntheticCreditULXC); n != 1 {
			t.Errorf("%s lost its balance", id)
		}
	}
	// What the old test user owned is gone, except its ledger rows, which are append-only and marked test.
	for _, q := range []string{
		`SELECT count(*) FROM prompt_embeddings WHERE workspace_id = 's-old'`,
		`SELECT count(*) FROM lxc_balances WHERE workspace_id = 's-old'`,
		`SELECT count(*) FROM agent_money_requests WHERE id = 'mreq-test'`,
	} {
		if n := one(q); n != 0 {
			t.Errorf("%s = %d after the purge, want 0", q, n)
		}
	}
	if n := one(`SELECT count(*) FROM lxc_ledger WHERE workspace_id = 's-old' AND test`); n != 1 {
		t.Errorf("the old test user's ledger rows = %d, want its 1 grant kept, marked test", n)
	}
	if n := one(`SELECT count(*) FROM agent_money_requests WHERE id = 'mreq-crossed'`); n != 1 {
		t.Error("the crossing money row was deleted")
	}
	if n, _ := rdb.Exists(ctx, "lens:exact:copy-s-old:owner").Result(); n != 0 {
		t.Error("the old test user's cached copy is still in Redis")
	}
	if n, _ := rdb.Exists(ctx, "lens:exact:copy-s-young:owner", "lens:exact:copy-s-old-crossed:owner", "lens:exact:copy-u-real:owner").Result(); n != 3 {
		t.Errorf("%d of the 3 kept workspaces' cached copies remain, want 3", n)
	}
}
