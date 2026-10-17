package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-chi/chi/v5"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/redis/go-redis/v9"
	"github.com/talyvor/lens/internal/auth"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/learner"
	"github.com/talyvor/lens/internal/localrouter"
)

func newRouter(t *testing.T, s *Server) chi.Router {
	t.Helper()
	r := chi.NewRouter()
	s.Mount(r)
	return r
}

func doJSON(t *testing.T, r chi.Router, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	// Dashboard reads are authenticated; inject an admin identity so the #146
	// Phase-2 chokepoint honors the ?workspace_id= these tests pass.
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{IsAdmin: true}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode JSON: %v; body=%s", err, w.Body.String())
	}
	return w, got
}

func doJSONArray(t *testing.T, r chi.Router, path string) (*httptest.ResponseRecorder, []any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	// Dashboard reads are authenticated; inject an admin identity so the #146
	// Phase-2 chokepoint honors the ?workspace_id= these tests pass.
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{IsAdmin: true}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var got []any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode JSON array: %v; body=%s", err, w.Body.String())
	}
	return w, got
}

func newServerWithPool(t *testing.T, pool pgxmock.PgxPoolIface) *Server {
	t.Helper()
	t.Cleanup(pool.Close)
	return newServer(serverDeps{
		pool:      pool,
		version:   "test",
		startTime: time.Now(),
	})
}

func TestAPI_SpendSummary_ReturnsCorrectTotals(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	pool.ExpectQuery(`SUM\(cost_usd\)`).
		WithArgs("default", 30).
		WillReturnRows(
			pgxmock.NewRows([]string{
				"total_cost_usd", "total_input_tokens", "total_output_tokens",
				"total_requests", "cached_requests",
			}).AddRow(float64(1234.56), int64(1000000), int64(500000), int64(5000), int64(2500)),
		)

	s := newServerWithPool(t, pool)
	r := newRouter(t, s)
	_, got := doJSON(t, r, "/v1/api/spend/summary?workspace_id=default&days=30")

	if got["total_cost_usd"].(float64) != 1234.56 {
		t.Errorf("total_cost_usd = %v, want 1234.56", got["total_cost_usd"])
	}
	if int(got["total_requests"].(float64)) != 5000 {
		t.Errorf("total_requests = %v, want 5000", got["total_requests"])
	}
	if got["cache_hit_rate"].(float64) != 0.5 {
		t.Errorf("cache_hit_rate = %v, want 0.5", got["cache_hit_rate"])
	}
	if int(got["period_days"].(float64)) != 30 {
		t.Errorf("period_days = %v, want 30", got["period_days"])
	}
}

func TestAPI_SpendByModel_GroupsCorrectly(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	pool.ExpectQuery(`GROUP BY model`).
		WithArgs("default", 30).
		WillReturnRows(
			pgxmock.NewRows([]string{
				"model", "cost_usd", "requests", "input_tokens", "output_tokens",
			}).
				AddRow("gpt-4o", float64(890.12), int64(2000), int64(800000), int64(400000)).
				AddRow("claude-haiku-4-5", float64(100.00), int64(1500), int64(150000), int64(75000)),
		)

	s := newServerWithPool(t, pool)
	r := newRouter(t, s)
	_, got := doJSONArray(t, r, "/v1/api/spend/by-model?workspace_id=default&days=30")

	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	first := got[0].(map[string]any)
	if first["model"] != "gpt-4o" {
		t.Errorf("first model = %v, want gpt-4o", first["model"])
	}
	if first["cost_usd"].(float64) <= got[1].(map[string]any)["cost_usd"].(float64) {
		t.Errorf("expected first entry to have higher cost than second")
	}
}

func TestAPI_CacheStats_CalculatesHitRate(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	// Repointed to serve_source (migration 0100): total=100, cache_hits=60 (exact=40, semantic=20),
	// uncached_cost=$50 → total_hit_rate 0.6, exact 0.4, semantic 0.2, savings = 50 * 0.6 = $30.
	pool.ExpectQuery(`COUNT\(\*\)`).
		WithArgs("default").
		WillReturnRows(
			pgxmock.NewRows([]string{
				"total", "cache_hits", "exact_hits", "semantic_hits", "uncached_cost",
			}).AddRow(int64(100), int64(60), int64(40), int64(20), float64(50.0)),
		)
	pool.ExpectQuery(`COUNT\(\*\) FROM prompt_embeddings`).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(15420)))

	s := newServerWithPool(t, pool)
	r := newRouter(t, s)
	_, got := doJSON(t, r, "/v1/api/cache/stats?workspace_id=default")

	if got["total_hit_rate"].(float64) != 0.6 {
		t.Errorf("total_hit_rate = %v, want 0.6", got["total_hit_rate"])
	}
	if got["exact_hit_rate"].(float64) != 0.4 {
		t.Errorf("exact_hit_rate = %v, want 0.4", got["exact_hit_rate"])
	}
	if got["semantic_hit_rate"].(float64) != 0.2 {
		t.Errorf("semantic_hit_rate = %v, want 0.2", got["semantic_hit_rate"])
	}
	if got["estimated_savings_usd"].(float64) != 30.0 {
		t.Errorf("estimated_savings_usd = %v, want 30", got["estimated_savings_usd"])
	}
	if int(got["entries_count"].(float64)) != 15420 {
		t.Errorf("entries_count = %v, want 15420", got["entries_count"])
	}
	if got["estimated_savings_usd"].(float64) != 30.0 {
		t.Errorf("estimated_savings_usd = %v, want 30.0", got["estimated_savings_usd"])
	}
}

func TestAPI_CacheTopPatterns_ReturnsSortedList(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	pool.ExpectQuery(`ORDER BY hit_count DESC`).
		WithArgs(10, "default").
		WillReturnRows(
			pgxmock.NewRows([]string{"prompt_hash", "hit_count", "tokens_saved", "last_seen"}).
				AddRow("hash-top", int64(450), int64(180000), now).
				AddRow("hash-mid", int64(120), int64(50000), now).
				AddRow("hash-low", int64(45), int64(10000), now),
		)

	s := newServerWithPool(t, pool)
	r := newRouter(t, s)
	_, got := doJSONArray(t, r, "/v1/api/cache/top-patterns?workspace_id=default&limit=10")

	if len(got) != 3 {
		t.Fatalf("got %d patterns, want 3", len(got))
	}
	first := got[0].(map[string]any)
	if first["prompt_hash"] != "hash-top" {
		t.Errorf("first prompt_hash = %v, want hash-top", first["prompt_hash"])
	}
	if int(first["hit_count"].(float64)) != 450 {
		t.Errorf("first hit_count = %v, want 450", first["hit_count"])
	}
}

func TestAPI_ModelsRecommendations_ReturnsInsights(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	// The learner's Analyse() runs this query.
	pool.ExpectQuery(`HAVING COUNT\(\*\) >= 3`).
		WillReturnRows(
			pgxmock.NewRows([]string{"prompt_hash", "hit_count", "avg_tokens", "last_seen"}).
				AddRow("hash-1", int64(120), float64(800), time.Now()).
				AddRow("hash-2", int64(50), float64(400), time.Now()),
		)

	// learner.New needs a NATS conn for JetStream.
	nc := runEmbeddedNATS(t)
	lrn := learner.New(nc, nil)
	// Replace its pool via the unexported path: we can't, so build via the
	// same exported constructor and rely on the same pool. The learner's
	// public New uses pool internally, but tests can't substitute. Instead,
	// we pass the pool both to the api server AND to a freshly-built
	// learner that uses it. The learner's New signature requires
	// *pgxpool.Pool, not the pgxDB interface — we work around that by
	// using nil and accepting that learner.Analyse goes through the api
	// server's pgxmock pool via the API handler calling lrn.Analyse, which
	// uses lrn's own pool (nil). So we can't actually drive that pool.
	//
	// Pragmatic fallback: provide a stub Analyser on the Server.
	stub := stubAnalyser{insights: []learner.PatternInsight{
		{PromptPattern: "hash-1", HitCount: 120, AvgTokensSaved: 800, Recommendation: "Cache this pattern — seen 120 times, saves ~800 tokens per hit"},
		{PromptPattern: "hash-2", HitCount: 50, AvgTokensSaved: 400, Recommendation: "Cache this pattern — seen 50 times, saves ~400 tokens per hit"},
	}}

	s := newServer(serverDeps{
		pool:      pool,
		analyser:  stub,
		version:   "test",
		startTime: time.Now(),
	})
	_ = lrn
	r := newRouter(t, s)
	_, got := doJSONArray(t, r, "/v1/api/models/recommendations")

	if len(got) != 2 {
		t.Fatalf("got %d recs, want 2", len(got))
	}
	first := got[0].(map[string]any)
	if first["pattern_hash"] != "hash-1" {
		t.Errorf("first pattern_hash = %v, want hash-1", first["pattern_hash"])
	}
	if int(first["hit_count"].(float64)) != 120 {
		t.Errorf("first hit_count = %v, want 120", first["hit_count"])
	}
	if first["estimated_monthly_savings_usd"].(float64) <= 0 {
		t.Errorf("estimated_monthly_savings_usd = %v, want > 0", first["estimated_monthly_savings_usd"])
	}
}

func TestAPI_AlertsCircuits_ReturnsCircuitStates(t *testing.T) {
	nc := runEmbeddedNATS(t)
	am := alerts.New(nil, nc, nil)
	am.OpenCircuit("ws-a", "team-a", "search")

	s := newServer(serverDeps{
		alertManager: am,
		version:      "test",
		startTime:    time.Now(),
	})
	r := newRouter(t, s)
	_, got := doJSON(t, r, "/v1/api/alerts/circuits")

	if got["ws-a:team-a:search"] != "open" {
		t.Errorf("expected ws-a:team-a:search = open; got %v", got["ws-a:team-a:search"])
	}
}

func TestAPI_LocalStatus_ReturnsAvailability(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			_, _ = w.Write([]byte(`{"models":[{"name":"llama3.2","size":2.0}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	lr := localrouter.New(srv.URL)
	if !lr.CheckAvailability(context.Background()) {
		t.Fatal("setup: expected available")
	}

	s := newServer(serverDeps{
		localRouter: lr,
		version:     "test",
		startTime:   time.Now(),
	})
	r := newRouter(t, s)
	_, got := doJSON(t, r, "/v1/api/local/status")

	if got["available"] != true {
		t.Errorf("available = %v, want true", got["available"])
	}
	models, _ := got["models"].([]any)
	if len(models) != 1 {
		t.Errorf("models length = %d, want 1", len(models))
	}
}

func TestAPI_Health_ReturnsAllCheckStatuses(t *testing.T) {
	// postgres: pgxmock with successful ping.
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	pool.ExpectPing()

	// redis: miniredis.
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	// nats: embedded server.
	nc := runEmbeddedNATS(t)

	s := newServer(serverDeps{
		pool:        pool,
		redisClient: rc,
		natsConn:    nc,
		version:     "0.1.0",
		startTime:   time.Now().Add(-time.Hour),
	})
	r := newRouter(t, s)
	_, got := doJSON(t, r, "/v1/api/health")

	if got["status"] != "ok" {
		t.Errorf("status = %v, want ok", got["status"])
	}
	if got["version"] != "0.1.0" {
		t.Errorf("version = %v, want 0.1.0", got["version"])
	}
	if got["uptime_seconds"].(float64) < 1 {
		t.Errorf("uptime_seconds = %v, want >= 1", got["uptime_seconds"])
	}
	checks := got["checks"].(map[string]any)
	if checks["postgres"] != "ok" {
		t.Errorf("postgres = %v, want ok", checks["postgres"])
	}
	if checks["redis"] != "ok" {
		t.Errorf("redis = %v, want ok", checks["redis"])
	}
	if checks["nats"] != "ok" {
		t.Errorf("nats = %v, want ok", checks["nats"])
	}
}

// stubAnalyser stands in for *learner.Learner in tests so we don't have to
// configure a real learner pool to exercise the recommendations endpoint.
type stubAnalyser struct {
	insights []learner.PatternInsight
}

func (s stubAnalyser) AnalyseWorkspace(_ context.Context, _ string) ([]learner.PatternInsight, error) {
	return s.insights, nil
}

// runEmbeddedNATS spins up an in-process NATS server for tests that need it.
func runEmbeddedNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := &natsserver.Options{
		Host: "127.0.0.1", Port: -1,
		JetStream: true, StoreDir: t.TempDir(),
		NoLog: true, NoSigs: true,
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		srv.Shutdown()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		nc.Close()
		srv.Shutdown()
		srv.WaitForShutdown()
	})
	return nc
}
