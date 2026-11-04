package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/migrations"
)

// B19.1 — a workspace creates two agents, gives each a key, funds them and takes funds back, through
// the real routes on a migrated schema.

// agentRoutesDB gives each test its own database, cloned from a template migrated once per run: 161
// tests share this helper, and migrating for each of them took cmd/lens past its 600s CI timeout (B54.2).
func agentRoutesDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG agent routes test")
	}
	const schema = "cmd_agent_routes_realpg"
	ctx := context.Background()
	agentRoutesTemplateOnce.Do(func() { agentRoutesTemplateErr = agentRoutesTemplate(ctx, url, schema) })
	if agentRoutesTemplateErr != nil {
		t.Fatal(agentRoutesTemplateErr)
	}
	name := fmt.Sprintf("%s_%d", agentRoutesTemplateName, agentRoutesClones.Add(1))
	if err := agentRoutesAdmin(ctx, url, `CREATE DATABASE `+name+` TEMPLATE `+agentRoutesTemplateName); err != nil {
		t.Fatalf("clone the migrated database: %v", err)
	}
	t.Cleanup(func() { _ = agentRoutesAdmin(ctx, url, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`) })
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.Database = name
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

var (
	agentRoutesTemplateName = fmt.Sprintf("lens_cmd_agent_routes_%d", os.Getpid())
	agentRoutesTemplateOnce sync.Once
	agentRoutesTemplateErr  error
	agentRoutesClones       atomic.Int64
)

// agentRoutesTemplate creates the template database and migrates it into schema, as each test used to.
func agentRoutesTemplate(ctx context.Context, url, schema string) error {
	if err := agentRoutesAdmin(ctx, url, `CREATE DATABASE `+agentRoutesTemplateName); err != nil {
		return fmt.Errorf("create the template database: %w", err)
	}
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return err
	}
	cfg.Database = agentRoutesTemplateName
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	// Closed before any clone: Postgres refuses to copy a template that has a session open on it.
	defer conn.Close(ctx)
	for _, ddl := range []string{`CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`, `CREATE SCHEMA ` + schema} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			return fmt.Errorf("prepare the template: %w", err)
		}
	}
	if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// agentRoutesAdmin runs one statement on the base database; CREATE and DROP DATABASE cannot run on the
// database they name.
func agentRoutesAdmin(ctx context.Context, url, stmt string) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, stmt)
	return err
}

// dropAgentRoutesTemplate removes this run's template database; TestMain calls it after the tests.
func dropAgentRoutesTemplate() {
	if url := os.Getenv("LENS_TEST_DATABASE_URL"); url != "" {
		_ = agentRoutesAdmin(context.Background(), url, `DROP DATABASE IF EXISTS `+agentRoutesTemplateName+` WITH (FORCE)`)
	}
}

func TestAgentRoutes_AWorkspaceFundsTwoAgentsAndTakesFundsBack(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-agents"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))

	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	proxyKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "k", Scopes: []string{auth.ScopeProxy}}
	call := func(who *auth.AuthContext, method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	base := "/v1/workspaces/" + ws + "/agents"

	if code, _ := call(proxyKey, http.MethodPost, base, `{"name":"researcher"}`); code != http.StatusForbidden {
		t.Fatalf("a proxy key created an agent: %d, want 403", code)
	}
	code, a := call(owner, http.MethodPost, base, `{"name":"researcher"}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, a)
	}
	researcher := a["id"].(string)
	_, b := call(owner, http.MethodPost, base, `{"name":"writer"}`)
	writer := b["id"].(string)

	code, k := call(owner, http.MethodPost, base+"/"+researcher+"/keys", `{"name":"researcher key"}`)
	if code != http.StatusCreated || !strings.HasPrefix(k["key"].(string), tenant.KeyPrefix) {
		t.Fatalf("issue key = %d %v", code, k)
	}

	fund := func(agent string, ulxc string) (int, map[string]any) {
		return call(owner, http.MethodPost, base+"/"+agent+"/fund", `{"amount_ulxc":`+ulxc+`}`)
	}
	if code, out := fund(researcher, "10000000"); code != http.StatusOK || out["balance_ulxc"].(float64) != 10_000_000 {
		t.Fatalf("fund researcher = %d %v", code, out)
	}
	if code, out := fund(writer, "5000000"); code != http.StatusOK || out["balance_ulxc"].(float64) != 5_000_000 {
		t.Fatalf("fund writer = %d %v", code, out)
	}
	// 20 LXC in the workspace, 15 held by agents: 6 more cannot be allocated.
	if code, _ := fund(writer, "6000000"); code != http.StatusConflict {
		t.Fatalf("funding beyond the unallocated 5 LXC = %d, want 409", code)
	}
	if code, _ := call(proxyKey, http.MethodPost, base+"/"+writer+"/withdraw", `{"amount_ulxc":1}`); code != http.StatusForbidden {
		t.Fatalf("a proxy key withdrew from an agent: %d, want 403", code)
	}
	if code, out := call(owner, http.MethodPost, base+"/"+researcher+"/withdraw", `{"amount_ulxc":2000000}`); code != http.StatusOK || out["balance_ulxc"].(float64) != 8_000_000 {
		t.Fatalf("withdraw = %d %v", code, out)
	}
	if code, _ := call(owner, http.MethodPost, base+"/"+writer+"/withdraw", `{"amount_ulxc":5000001}`); code != http.StatusConflict {
		t.Fatalf("withdrawing more than the writer holds = %d, want 409", code)
	}

	code, book := call(proxyKey, http.MethodGet, base, ``)
	if code != http.StatusOK {
		t.Fatalf("book = %d", code)
	}
	if book["workspace_balance_ulxc"].(float64) != 20_000_000 || book["allocated_ulxc"].(float64) != 13_000_000 ||
		book["unallocated_ulxc"].(float64) != 7_000_000 || len(book["agents"].([]any)) != 2 {
		t.Errorf("book = %v, want workspace 20,000,000 = allocated 13,000,000 + unallocated 7,000,000, two agents", book)
	}
	var sum, n int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint, count(*) FROM agent_postings`).Scan(&sum, &n); err != nil {
		t.Fatal(err)
	}
	if sum != 0 || n != 6 {
		t.Errorf("agent_postings: %d rows summing to %d, want 6 rows (three entries) summing to 0", n, sum)
	}
}

// B17.26 — the app sends a Take back again, with the same Idempotency-Key, when the first met a deploy
// and got no answer: the LXC moves once, and the retry answers the balance it left.
func TestAgentRoutes_ATakeBackSentTwiceWithOneKeyMovesOnce(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-agents-retry"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 5000000, 5000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(path, body, key string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	base := "/v1/workspaces/" + ws + "/agents"
	_, a := call(base, `{"name":"south"}`, "")
	south := base + "/" + a["id"].(string)
	if code, out := call(south+"/fund", `{"amount_ulxc":1000000}`, "fund-1"); code != http.StatusOK {
		t.Fatalf("fund = %d %v", code, out)
	}
	for i := 0; i < 2; i++ {
		if code, out := call(south+"/withdraw", `{"amount_ulxc":250000}`, "back-1"); code != http.StatusOK || out["balance_ulxc"].(float64) != 750_000 {
			t.Fatalf("take back #%d with key back-1 = %d %v, want 200 and 750,000 µLXC", i+1, code, out)
		}
	}
	if code, out := call(south+"/withdraw", `{"amount_ulxc":250000}`, "back-2"); code != http.StatusOK || out["balance_ulxc"].(float64) != 500_000 {
		t.Fatalf("take back with a new key = %d %v, want 200 and 500,000 µLXC", code, out)
	}
	var n, sum int64
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_postings
	  WHERE workspace_id = $1 AND account = 'workspace' AND kind = 'withdraw'`, ws).Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	if n != 2 || sum != 500_000 {
		t.Errorf("the workspace was credited by %d take-back row(s) totalling %d µLXC, want 2 rows, 500,000 µLXC", n, sum)
	}
}

// B17.33 — the app sends a pot move again, with the same Idempotency-Key, when the first met a deploy and
// got no answer: 1.2 LXC in, then 0.4 out sent twice under one key, leaves the pot 0.8 and the agent the rest.
func TestAgentPotRoutes_AMoveOutSentTwiceWithOneKeyMovesOnce(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-pots-retry"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 5000000, 5000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountAgentPotRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(path, body, key string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	base := "/v1/workspaces/" + ws + "/agents"
	_, a := call(base, `{"name":"saver"}`, "")
	saver := base + "/" + a["id"].(string)
	if code, out := call(saver+"/fund", `{"amount_ulxc":3000000}`, ""); code != http.StatusOK {
		t.Fatalf("fund = %d %v", code, out)
	}
	_, p := call(saver+"/pots", `{"name":"Reserve","kind":"goal","target_ulxc":2000000}`, "")
	pot := saver + "/pots/" + p["id"].(string)
	for i := 0; i < 2; i++ {
		if code, out := call(pot+"/in", `{"amount_ulxc":1200000}`, "in-1"); code != http.StatusOK || out["balance_ulxc"].(float64) != 1_200_000 {
			t.Fatalf("move in #%d with key in-1 = %d %v, want 200 and 1,200,000 µLXC", i+1, code, out)
		}
	}
	for i := 0; i < 2; i++ {
		if code, out := call(pot+"/out", `{"amount_ulxc":400000}`, "out-1"); code != http.StatusOK || out["balance_ulxc"].(float64) != 800_000 {
			t.Fatalf("move out #%d with key out-1 = %d %v, want 200 and 800,000 µLXC", i+1, code, out)
		}
	}
	var potHolds, agentHolds int64
	if err := pool.QueryRow(ctx, `SELECT
	    COALESCE(sum(amount_ulxc) FILTER (WHERE account = 'pot:' || $2), 0)::bigint,
	    COALESCE(sum(amount_ulxc) FILTER (WHERE account = 'agent:' || $3), 0)::bigint
	  FROM agent_postings WHERE workspace_id = $1`, ws, p["id"], a["id"]).Scan(&potHolds, &agentHolds); err != nil {
		t.Fatal(err)
	}
	if potHolds != 800_000 || agentHolds != 2_200_000 {
		t.Errorf("the ledger has the pot at %d µLXC and the agent at %d, want 800,000 and 2,200,000", potHolds, agentHolds)
	}
}

// B19.2 — the owner sets an agent's rules and decides its approvals through the routes; a proxy key
// can read the rules but not change them, and a misspelt or invalid rule is refused rather than
// stored as no rule.
func TestAgentRoutes_TheOwnerSetsAnAgentsRulesAndDecidesItsApprovals(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-agents"
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	proxyKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "k", Scopes: []string{auth.ScopeProxy}}
	call := func(who *auth.AuthContext, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	agent, err := store.CreateAgent(ctx, ws, "researcher", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	rules := "/v1/workspaces/" + ws + "/agents/" + agent.ID + "/rules"
	const set = `{"max_per_request_ulxc":2000000,"hourly_limit_ulxc":3000000,"daily_limit_ulxc":10000000,"weekly_limit_ulxc":40000000,
		"approval_above_ulxc":1000000,"allowed_models":["gpt-4o"],"allowed_providers":["openai"],"active_from":"09:00","active_until":"18:00","timezone":"Europe/Bucharest"}`

	if code, _ := call(proxyKey, http.MethodPut, rules, set); code != http.StatusForbidden {
		t.Errorf("a proxy key set an agent's rules: %d, want 403", code)
	}
	for _, bad := range []string{`{"daily_limit":5}`, `{"active_from":"09:00"}`, `{"timezone":"Mars/Olympus"}`, `{"daily_limit_ulxc":-1}`, `{"hourly_limit_ulxc":-1}`,
		`{"model_daily_limits_ulxc":{"claude-opus-4-1":-1}}`, `{"model_daily_limits_ulxc":{"":5}}`,
		`{"model_daily_limits_ulxc":{"claude-opus-4-1":1,"Claude-Opus-4-1-20250805":2}}`, `{"requests_per_minute":-1}`, `{"requests_per_minute":3000000000}`,
		`{"blocked_payees":[""]}`, `{"allowed_payees":[" agt_x"]}`} {
		if code, body := call(owner, http.MethodPut, rules, bad); code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d %s, want 400", bad, code, body)
		}
	}
	if code, body := call(owner, http.MethodPut, rules, set); code != http.StatusOK {
		t.Fatalf("PUT rules = %d %s", code, body)
	}
	code, body := call(proxyKey, http.MethodGet, rules, "")
	var got economy.AgentRules
	if err := json.Unmarshal([]byte(body), &got); code != http.StatusOK || err != nil ||
		got.DailyLimitULXC != 10_000_000 || got.Timezone != "Europe/Bucharest" || len(got.AllowedModels) != 1 ||
		got.HourlyLimitULXC == nil || *got.HourlyLimitULXC != 3_000_000 || got.WeeklyLimitULXC == nil || *got.WeeklyLimitULXC != 40_000_000 {
		t.Errorf("GET rules = %d %s", code, body)
	}
	// B28.300: rules saved by a client that predates the hourly and weekly caps keep them; a zero clears one.
	for _, tc := range []struct{ put, hourly, weekly string }{
		{`{"daily_limit_ulxc":10000000}`, `"hourly_limit_ulxc":3000000`, `"weekly_limit_ulxc":40000000`},
		{`{"hourly_limit_ulxc":0}`, `"hourly_limit_ulxc":0`, `"weekly_limit_ulxc":40000000`},
	} {
		if code, body := call(owner, http.MethodPut, rules, tc.put); code != http.StatusOK || !strings.Contains(body, tc.hourly) || !strings.Contains(body, tc.weekly) {
			t.Errorf("PUT %s = %d %s, want %s and %s", tc.put, code, body, tc.hourly, tc.weekly)
		}
		if _, body := call(owner, http.MethodGet, rules, ""); !strings.Contains(body, tc.hourly) || !strings.Contains(body, tc.weekly) {
			t.Errorf("after PUT %s, GET = %s, want %s and %s", tc.put, body, tc.hourly, tc.weekly)
		}
	}
	// B28.301: a model's daily cap is saved and read back; rules saved without the field keep it, and {} clears it.
	for _, tc := range []struct{ put, want string }{
		{`{"model_daily_limits_ulxc":{"claude-opus-4-1":5000000,"claude-haiku-4-5":0}}`, `"model_daily_limits_ulxc":{"claude-opus-4-1":5000000}`},
		{`{"daily_limit_ulxc":10000000}`, `"model_daily_limits_ulxc":{"claude-opus-4-1":5000000}`},
		{`{"model_daily_limits_ulxc":{}}`, `"model_daily_limits_ulxc":{}`},
	} {
		if code, body := call(owner, http.MethodPut, rules, tc.put); code != http.StatusOK || !strings.Contains(body, tc.want) {
			t.Errorf("PUT %s = %d %s, want %s", tc.put, code, body, tc.want)
		}
		if _, body := call(proxyKey, http.MethodGet, rules, ""); !strings.Contains(body, tc.want) {
			t.Errorf("after PUT %s, GET = %s, want %s", tc.put, body, tc.want)
		}
	}
	// B28.302: the owner caps the agent's requests a minute; rules saved without the field keep it, and a zero clears it.
	for _, tc := range []struct{ put, want string }{
		{`{"requests_per_minute":60}`, `"requests_per_minute":60`},
		{`{"daily_limit_ulxc":10000000}`, `"requests_per_minute":60`},
		{`{"requests_per_minute":0}`, `"requests_per_minute":0`},
	} {
		if code, body := call(owner, http.MethodPut, rules, tc.put); code != http.StatusOK || !strings.Contains(body, tc.want) {
			t.Errorf("PUT %s = %d %s, want %s", tc.put, code, body, tc.want)
		}
		if _, body := call(proxyKey, http.MethodGet, rules, ""); !strings.Contains(body, tc.want) {
			t.Errorf("after PUT %s, GET = %s, want %s", tc.put, body, tc.want)
		}
	}
	if code, _ := call(owner, http.MethodPut, "/v1/workspaces/"+ws+"/agents/agt_nobody/rules", set); code != http.StatusNotFound {
		t.Errorf("rules for an agent that is not this workspace's = %d, want 404", code)
	}

	// An approval the agent's rules filed: listed, approved once by the owner, never by a proxy key.
	if _, err := pool.Exec(ctx, `INSERT INTO agent_approvals (id, workspace_id, agent_id, fingerprint, amount_ulxc, model)
		VALUES ('apr_1', $1, $2, 'fp', 3000000, 'gpt-4o')`, ws, agent.ID); err != nil {
		t.Fatal(err)
	}
	approvals := "/v1/workspaces/" + ws + "/agents/approvals"
	if code, body := call(owner, http.MethodGet, approvals, ""); code != http.StatusOK || !strings.Contains(body, `"status":"pending"`) {
		t.Errorf("GET approvals = %d %s", code, body)
	}
	if code, _ := call(proxyKey, http.MethodPost, approvals+"/apr_1/approve", ""); code != http.StatusForbidden {
		t.Errorf("a proxy key approved its own request: %d, want 403", code)
	}
	if code, body := call(owner, http.MethodPost, approvals+"/apr_1/approve", ""); code != http.StatusOK || !strings.Contains(body, `"status":"approved"`) {
		t.Errorf("approve = %d %s", code, body)
	}
	if code, _ := call(owner, http.MethodPost, approvals+"/apr_1/deny", ""); code != http.StatusNotFound {
		t.Errorf("deciding an approval twice = %d, want 404", code)
	}
}

// B19.3 — one company's agents pay each other inside the closed loop: a payment is ONE entry of two
// postings, the workspace's LXC and its lxc_ledger do not move, only the paying agent's own key (or the
// owner) can pay from it, and the payer's rules judge the payment.
func TestAgentRoutes_AnAgentPaysAnotherInOnePairOfLedgerRows(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-agents"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	call := func(who *auth.AuthContext, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	newAgent := func(name, key string, fund int64) string {
		t.Helper()
		a, err := store.CreateAgent(ctx, ws, name, "user-owner")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.AttachAgentKey(ctx, ws, a.ID, key); err != nil {
			t.Fatal(err)
		}
		if fund > 0 {
			if _, err := store.FundAgent(ctx, ws, a.ID, fund); err != nil {
				t.Fatal(err)
			}
		}
		return a.ID
	}
	buyer := newAgent("buyer", "key-buyer", 10_000_000)
	seller := newAgent("seller", "key-seller", 0)
	keyOf := func(id string) *auth.AuthContext {
		return &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: id, Scopes: []string{auth.ScopeProxy}}
	}
	pay := func(who *auth.AuthContext, from, body string) (int, string) {
		return call(who, "/v1/workspaces/"+ws+"/agents/"+from+"/pay", body)
	}
	ledgerRows := func() (rows, net int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount), 0)::bigint FROM lxc_ledger WHERE workspace_id = $1`, ws).Scan(&rows, &net); err != nil {
			t.Fatal(err)
		}
		return rows, net
	}
	ledgerBefore, netBefore := ledgerRows()

	if code, body := pay(keyOf("key-seller"), buyer, `{"to_agent_id":"`+seller+`","amount_ulxc":3000000}`); code != http.StatusForbidden {
		t.Errorf("the seller's key paid itself from the buyer: %d %s, want 403", code, body)
	}
	code, body := pay(keyOf("key-buyer"), buyer, `{"to_agent_id":"`+seller+`","amount_ulxc":3000000,"memo":"invoice 17"}`)
	var got economy.AgentPayment
	if err := json.Unmarshal([]byte(body), &got); code != http.StatusOK || err != nil ||
		got.FromBalanceULXC != 7_000_000 || got.ToBalanceULXC != 3_000_000 {
		t.Fatalf("the buyer pays the seller 3 LXC = %d %s", code, body)
	}

	// ONE entry, TWO postings, summing to zero, between the two agents and nothing else.
	var postings, sum int64
	var accounts string
	if err := pool.QueryRow(ctx, `SELECT count(*), sum(amount_ulxc)::bigint, string_agg(account || '=' || amount_ulxc, ' ' ORDER BY amount_ulxc)
		FROM agent_postings WHERE entry_id = $1 AND kind = 'pay'`, got.EntryID).Scan(&postings, &sum, &accounts); err != nil {
		t.Fatal(err)
	}
	if want := "agent:" + buyer + "=-3000000 agent:" + seller + "=3000000"; postings != 2 || sum != 0 || accounts != want {
		t.Errorf("the payment posted %d rows summing to %d: %s — want %s", postings, sum, accounts, want)
	}
	// Inside the closed loop: the workspace's LXC and its lxc_ledger did not move, nor did what is allocated.
	if rows, net := ledgerRows(); rows != ledgerBefore || net != netBefore {
		t.Errorf("lxc_ledger moved: %d rows netting %d, was %d netting %d", rows, net, ledgerBefore, netBefore)
	}
	book, err := store.AgentBook(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if book.WorkspaceBalanceULXC != 20_000_000 || book.AllocatedULXC != 10_000_000 || book.SpentULXC != 0 {
		t.Errorf("book = %+v: want 20 LXC in the workspace, 10 allocated, nothing spent", book)
	}

	// The seller can now spend what it was paid, and pay it back.
	if code, body := pay(keyOf("key-seller"), seller, `{"to_agent_id":"`+buyer+`","amount_ulxc":1000000}`); code != http.StatusOK {
		t.Errorf("the seller pays 1 LXC back = %d %s", code, body)
	}
	// Refusals: beyond the payer's balance, to itself, to an agent that is not this workspace's, and
	// beyond the payer's daily limit (the 3 LXC it already paid counts; the 1 LXC it was paid does not).
	if code, _ := pay(keyOf("key-buyer"), buyer, `{"to_agent_id":"`+seller+`","amount_ulxc":9000000}`); code != http.StatusConflict {
		t.Errorf("paying beyond the balance = %d, want 409", code)
	}
	if code, _ := pay(keyOf("key-buyer"), buyer, `{"to_agent_id":"`+buyer+`","amount_ulxc":1}`); code != http.StatusBadRequest {
		t.Errorf("paying itself = %d, want 400", code)
	}
	if code, _ := pay(keyOf("key-buyer"), buyer, `{"to_agent_id":"agt_elsewhere","amount_ulxc":1}`); code != http.StatusNotFound {
		t.Errorf("paying another workspace's agent = %d, want 404", code)
	}
	if _, err := store.SetAgentRules(ctx, ws, buyer, economy.AgentRules{DailyLimitULXC: 3_500_000, AllowedModels: []string{"gpt-4o"}}); err != nil {
		t.Fatal(err)
	}
	if code, body := pay(keyOf("key-buyer"), buyer, `{"to_agent_id":"`+seller+`","amount_ulxc":600000}`); code != http.StatusForbidden || !strings.Contains(body, "daily limit") {
		t.Errorf("paying past the daily limit = %d %s, want 403 naming it", code, body)
	}
	if code, body := pay(keyOf("key-buyer"), buyer, `{"to_agent_id":"`+seller+`","amount_ulxc":400000}`); code != http.StatusOK {
		t.Errorf("paying within the daily limit, with a model rule set = %d %s, want 200 — a payment has no model", code, body)
	}

	// The seller's statement: newest first, each line against its counterparty with the balance it left.
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/agents/"+seller+"/statement", nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(), keyOf("key-seller")))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var st struct {
		Lines []economy.AgentStatementLine `json:"lines"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); w.Code != http.StatusOK || err != nil || len(st.Lines) != 3 {
		t.Fatalf("statement = %d %s", w.Code, w.Body.String())
	}
	for i, want := range []struct {
		amount, after int64
		memo          string
	}{{400_000, 2_400_000, ""}, {-1_000_000, 2_000_000, ""}, {3_000_000, 3_000_000, "invoice 17"}} {
		l := st.Lines[i]
		if l.Kind != "pay" || l.AmountULXC != want.amount || l.BalanceAfterULXC != want.after || l.Ref != want.memo ||
			l.Counterparty != "agent:"+buyer {
			t.Errorf("statement line %d = %+v, want pay %d against the buyer leaving %d", i, l, want.amount, want.after)
		}
	}
}

// B19.5 — a statement for any period re-derives the same totals from the audit log (agent_postings), per
// agent and for the workspace, as JSON and as CSV; two adjoining periods chain into the whole; and asking
// for a closed period again, after more movements, answers the same bytes.
func TestAgentRoutes_AStatementForAnyPeriodReDerivesTheAuditLog(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-agents"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 50000000, 50000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	get := func(path string) (int, []byte) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.Bytes()
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	clock := func() time.Time { // the database's clock, between two committed movements
		t.Helper()
		var c time.Time
		must(pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&c))
		time.Sleep(time.Millisecond)
		return c.UTC()
	}
	newAgent := func(name, key string) string {
		t.Helper()
		a, err := store.CreateAgent(ctx, ws, name, "user-owner")
		must(err)
		must(store.AttachAgentKey(ctx, ws, a.ID, key))
		return a.ID
	}
	fund := func(id string, v int64) { t.Helper(); _, err := store.FundAgent(ctx, ws, id, v); must(err) }

	t0 := clock()
	buyer, seller := newAgent("buyer", "key-buyer"), newAgent("seller", "key-seller")
	fund(buyer, 10_000_000)
	fund(seller, 2_000_000)
	must(store.SpendLXCForAgent(ctx, "key-buyer", ws, "req-1", 260_000, "chat", economy.AgentDebitMeta{}))
	t1 := clock()
	_, err := store.PayAgent(ctx, ws, buyer, seller, 3_000_000, "=invoice 17")
	must(err)
	_, err = store.WithdrawAgent(ctx, ws, seller, 1_000_000)
	must(err)
	must(store.SpendLXCForAgent(ctx, "key-seller", ws, "req-2", 140_000, "chat", economy.AgentDebitMeta{}))
	t2 := clock()
	book, err := store.AgentBook(ctx, ws)
	must(err)
	balanceAtT2 := map[string]int64{}
	for _, a := range book.Agents {
		balanceAtT2["agent:"+a.ID] = a.BalanceULXC
	}

	period := func(from, to time.Time) string {
		return "from=" + url.QueryEscape(from.Format(time.RFC3339Nano)) + "&to=" + url.QueryEscape(to.Format(time.RFC3339Nano))
	}
	statement := func(path string) (economy.Statement, []byte) {
		t.Helper()
		code, body := get(path)
		var st economy.Statement
		if err := json.Unmarshal(body, &st); code != http.StatusOK || err != nil {
			t.Fatalf("GET %s = %d %s", path, code, body)
		}
		return st, body
	}
	agentPath := func(id, q string) string { return "/v1/workspaces/" + ws + "/agents/" + id + "/statement?" + q }
	wsPath := "/v1/workspaces/" + ws + "/agents/statement?"

	// The whole period closes each agent at the balance it had at t2.
	whole := map[string]economy.Statement{}
	for _, id := range []string{buyer, seller} {
		st, _ := statement(agentPath(id, period(t0, t2)))
		if len(st.Accounts) != 1 || st.Accounts[0].OpeningULXC != 0 || st.Accounts[0].ClosingULXC != balanceAtT2["agent:"+id] {
			t.Errorf("%s over [t0,t2) = %+v, want 0 → %d", id, st.Accounts, balanceAtT2["agent:"+id])
		}
		whole[id] = st
	}
	if a := whole[buyer].Accounts[0]; a.InULXC != 10_000_000 || a.OutULXC != 3_260_000 || a.ClosingULXC != 6_740_000 {
		t.Errorf("buyer = %+v, want in 10,000,000, out 3,260,000, closing 6,740,000", a)
	}

	// Two adjoining periods chain into the whole: the first closes where the second opens, and between
	// them they hold exactly the whole period's postings, in order.
	for _, id := range []string{buyer, seller} {
		first, _ := statement(agentPath(id, period(t0, t1)))
		second, _ := statement(agentPath(id, period(t1, t2)))
		if first.Accounts[0].ClosingULXC != second.Accounts[0].OpeningULXC || second.Accounts[0].ClosingULXC != whole[id].Accounts[0].ClosingULXC {
			t.Errorf("%s: [t0,t1) closes at %d, [t1,t2) opens at %d and closes at %d; the whole closes at %d",
				id, first.Accounts[0].ClosingULXC, second.Accounts[0].OpeningULXC, second.Accounts[0].ClosingULXC, whole[id].Accounts[0].ClosingULXC)
		}
		var ids []int64
		for _, l := range append(first.Lines, second.Lines...) {
			ids = append(ids, l.PostingID)
		}
		var wantIDs []int64
		for _, l := range whole[id].Lines {
			wantIDs = append(wantIDs, l.PostingID)
		}
		if fmt.Sprint(ids) != fmt.Sprint(wantIDs) {
			t.Errorf("%s: the two periods hold postings %v, the whole %v", id, ids, wantIDs)
		}
	}

	// The workspace's statement for [t1,t2), re-derived from the audit log by hand: every account's
	// opening and movement are sums of its agent_postings rows, every line is the row its posting_id
	// names, and the movements net to zero.
	wsSt, _ := statement(wsPath + period(t1, t2))
	var net int64
	for _, a := range wsSt.Accounts {
		var opening, moved int64
		must(pool.QueryRow(ctx, `SELECT
			COALESCE(sum(amount_ulxc) FILTER (WHERE created_at < $3), 0)::bigint,
			COALESCE(sum(amount_ulxc) FILTER (WHERE created_at >= $3 AND created_at < $4), 0)::bigint
			FROM agent_postings WHERE workspace_id = $1 AND account = $2`, ws, a.Account, t1, t2).Scan(&opening, &moved))
		if a.OpeningULXC != opening || a.ClosingULXC-a.OpeningULXC != moved || a.InULXC-a.OutULXC != moved {
			t.Errorf("%s = %+v; the audit log says opening %d, moved %d", a.Account, a, opening, moved)
		}
		net += moved
	}
	if net != 0 || len(wsSt.Accounts) != 4 || len(wsSt.Lines) != 6 {
		t.Errorf("workspace [t1,t2): %d accounts, %d lines, netting %d — want 4 accounts, 6 lines (three entries), netting 0", len(wsSt.Accounts), len(wsSt.Lines), net)
	}
	for _, l := range wsSt.Lines {
		var entry, account, kind, ref string
		var amount int64
		must(pool.QueryRow(ctx, `SELECT entry_id::text, account, kind, amount_ulxc, ref FROM agent_postings WHERE id = $1`, l.PostingID).
			Scan(&entry, &account, &kind, &amount, &ref))
		if entry != l.EntryID || account != l.Account || kind != l.Kind || amount != l.AmountULXC || ref != l.Ref {
			t.Errorf("line %+v does not match its audit-log row %s %s %s %d %q", l, entry, account, kind, amount, ref)
		}
	}

	// The CSV says the same: each account's opening plus its lines' amounts is its closing, and a memo a
	// spreadsheet would run is kept as text.
	code, csvBody := get(wsPath + period(t1, t2) + "&format=csv")
	records, err := csv.NewReader(bytes.NewReader(csvBody)).ReadAll()
	if code != http.StatusOK || err != nil || len(records) != 1+4+6+4 {
		t.Fatalf("CSV = %d %v: %s", code, err, csvBody)
	}
	running := map[string]int64{}
	for _, rec := range records[1:] {
		v, _ := strconv.ParseInt(rec[8], 10, 64)
		switch rec[4] {
		case "opening":
			running[rec[3]] = v
		case "closing":
			if running[rec[3]] != v {
				t.Errorf("CSV %s: opening + lines = %d, closing row says %d", rec[3], running[rec[3]], v)
			}
		default:
			amt, _ := strconv.ParseInt(rec[5], 10, 64)
			running[rec[3]] += amt
			if running[rec[3]] != v {
				t.Errorf("CSV line %v: running balance %d", rec, running[rec[3]])
			}
			if rec[4] == "pay" && rec[7] != "'=invoice 17" {
				t.Errorf("CSV memo = %q, want it kept as text", rec[7])
			}
		}
	}

	// More movements after t2 do not change a statement for a period that ended at t2.
	_, buyerJSON := statement(agentPath(buyer, period(t0, t2)))
	_, wsJSON := statement(wsPath + period(t0, t2))
	_, wsCSV := get(wsPath + period(t0, t2) + "&format=csv")
	fund(buyer, 1_000_000)
	_, err = store.PayAgent(ctx, ws, buyer, seller, 500_000, "later")
	must(err)
	if _, again := statement(agentPath(buyer, period(t0, t2))); !bytes.Equal(again, buyerJSON) {
		t.Errorf("the buyer's [t0,t2) statement changed after later movements:\n%s\n%s", buyerJSON, again)
	}
	if _, again := statement(wsPath + period(t0, t2)); !bytes.Equal(again, wsJSON) {
		t.Errorf("the workspace's [t0,t2) statement changed after later movements")
	}
	if _, again := get(wsPath + period(t0, t2) + "&format=csv"); !bytes.Equal(again, wsCSV) {
		t.Errorf("the workspace's [t0,t2) CSV changed after later movements")
	}

	for path, want := range map[string]int{
		agentPath(buyer, period(t2, t1)):         http.StatusBadRequest,
		agentPath(buyer, "from=yesterday"):       http.StatusBadRequest,
		agentPath(buyer, "format=xml"):           http.StatusBadRequest,
		agentPath("agt_elsewhere", "format=csv"): http.StatusNotFound,
	} {
		if code, body := get(path); code != want {
			t.Errorf("GET %s = %d %s, want %d", path, code, body, want)
		}
	}
}

// B23.5 — a payment above the payer's approval amount files an approval that names who is being paid and
// why: on the row, and in the approvals the owner reads.
func TestAgentRoutes_AnApprovalSaysWhoIsBeingPaidAndWhy(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-agents"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	buyer, err := store.CreateAgent(ctx, ws, "buyer", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	seller, err := store.CreateAgent(ctx, ws, "Acme translator", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, buyer.ID, 10_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, buyer.ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}

	code, body := call(http.MethodPost, "/v1/workspaces/"+ws+"/agents/"+buyer.ID+"/pay",
		`{"to_agent_id":"`+seller.ID+`","amount_ulxc":3000000,"memo":"invoice 18"}`)
	if code != http.StatusForbidden || !strings.Contains(body, "apr_") {
		t.Fatalf("a 3 LXC payment above the 1 LXC approval amount = %d %s, want 403 naming the approval filed", code, body)
	}
	var row economy.Payee
	var memo string
	if err := pool.QueryRow(ctx, `SELECT payee_kind, payee_id, payee_name, memo FROM agent_approvals
		WHERE agent_id = $1 AND status = 'pending' AND amount_ulxc = 3000000`, buyer.ID).Scan(&row.Kind, &row.ID, &row.Name, &memo); err != nil {
		t.Fatalf("the approval's row: %v", err)
	}
	if want := (economy.Payee{Kind: "agent", ID: seller.ID, Name: "Acme translator"}); row != want || memo != "invoice 18" {
		t.Errorf("the approval's row names %+v, memo %q — want %+v, memo %q", row, memo, want, "invoice 18")
	}

	code, body = call(http.MethodGet, "/v1/workspaces/"+ws+"/agents/approvals", "")
	var got struct {
		Approvals []economy.AgentApproval `json:"approvals"`
	}
	if err := json.Unmarshal([]byte(body), &got); code != http.StatusOK || err != nil || len(got.Approvals) != 1 {
		t.Fatalf("GET approvals = %d %s", code, body)
	}
	if a := got.Approvals[0]; a.Payee == nil || *a.Payee != row || a.Memo != memo || a.AmountULXC != 3_000_000 {
		t.Errorf("GET approvals returned payee %+v, memo %q — the row says %+v, memo %q", a.Payee, a.Memo, row, memo)
	}
}
