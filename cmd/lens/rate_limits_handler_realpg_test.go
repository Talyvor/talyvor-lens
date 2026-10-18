package main

import (
	"context"
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
	"github.com/talyvor/lens/internal/ratelimit"
	"github.com/talyvor/lens/migrations"
)

// B39.9 — an operator raises one workspace to 5,000 a minute through the admin route; its 1,001st request in the
// minute is served while another workspace's is refused 429.
func TestRateLimits_ARaisedWorkspaceIsServedPastTheDefault(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG rate limits test")
	}
	const schema = "cmd_rate_limits_realpg"
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema, `SET search_path = ` + schema + `,public`} {
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

	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	limits := ratelimit.NewWorkspaceLimits(pool)
	limiter := ratelimit.New(rc, ratelimit.DefaultRules())

	admin := chi.NewRouter()
	admin.Put("/v1/admin/workspaces/{wsID}/rate-limits", newRateLimitsPutHandler(limits).ServeHTTP)
	put := httptest.NewRecorder()
	admin.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/v1/admin/workspaces/ws-fleet/rate-limits",
		strings.NewReader(`{"requests_per_second":5000,"requests_per_minute":5000,"requests_per_hour":50000}`)))
	if put.Code != http.StatusOK {
		t.Fatalf("PUT rate-limits = %d %s", put.Code, put.Body)
	}
	// The limiter reads a second copy loaded from the table, as another instance would.
	other := ratelimit.NewWorkspaceLimits(pool)
	if err := other.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	limiter.AttachWorkspaceLimits(other)

	served := ratelimit.RateLimitMiddleware(limiter)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// Each workspace has had 1,000 requests served this minute; the next one is its 1,001st.
	last := func(ws string) *httptest.ResponseRecorder {
		key := "lens:rl:" + ws + "::minute"
		if err := mr.Set(key, "1000"); err != nil {
			t.Fatal(err)
		}
		mr.SetTTL(key, 30*time.Second)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("X-Talyvor-Workspace", ws)
		rec := httptest.NewRecorder()
		served.ServeHTTP(rec, req)
		return rec
	}
	if got := last("ws-fleet"); got.Code != http.StatusOK {
		t.Errorf("raised workspace's 1,001st request in the minute = %d %s, want 200", got.Code, got.Body)
	}
	if got := last("ws-other"); got.Code != http.StatusTooManyRequests || !strings.Contains(got.Body.String(), `"limit_type":"minute"`) {
		t.Errorf("default workspace's 1,001st request in the minute = %d %s, want 429 on the minute", got.Code, got.Body)
	}
}
