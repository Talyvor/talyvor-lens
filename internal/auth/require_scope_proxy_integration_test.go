package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/talyvor/lens/internal/tenant"
)

// End-to-end proof that proxy-scope enforcement holds through the REAL credential
// stack: a workspace key is created via the tenant store, validated by
// Manager.Authenticate (the only place the workspace-key branch runs — no mock
// seam), stamped onto the request by AuthMiddleware, and then judged by
// RequireScope. Real PG because the workspace-key scopes round-trip through the
// database.
func TestProxyScope_RealCredentials_EndToEnd(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG proxy-scope enforcement test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS workspace_api_keys (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL, key_hash TEXT NOT NULL,
			key_prefix TEXT NOT NULL, name TEXT NOT NULL DEFAULT '', scopes TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
			last_used_at TIMESTAMPTZ, expires_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE INDEX IF NOT EXISTS workspace_api_keys_prefix_idx ON workspace_api_keys (key_prefix)`,
		// Empty api_keys table so the AuthMiddleware fast path cleanly misses
		// (ErrNoRows) and falls through to Manager.Authenticate for tlv_ws_ keys.
		`CREATE TABLE IF NOT EXISTS api_keys (
			id TEXT PRIMARY KEY, key_hash TEXT UNIQUE NOT NULL, workspace_id TEXT NOT NULL DEFAULT 'default',
			team TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '', active BOOLEAN NOT NULL DEFAULT true,
			created_at TIMESTAMPTZ DEFAULT NOW(), last_used_at TIMESTAMPTZ, expires_at TIMESTAMPTZ)`,
		`TRUNCATE workspace_api_keys`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}

	store := tenant.NewStore(pool)
	keyStore := New(pool)
	const adminKey = "super-secret-admin"
	mgr := NewManager(adminKey, nil, keyStore, store)

	mkKey := func(ws string, scopes []string) string {
		raw, _, err := store.CreateAPIKey(ctx, ws, "k", scopes, nil)
		if err != nil {
			t.Fatalf("CreateAPIKey(ws=%s scopes=%v): %v", ws, scopes, err)
		}
		return raw
	}
	proxyKey := mkKey("ws_proxy", []string{ScopeProxy})
	analyticsKey := mkKey("ws_analytics", []string{ScopeAnalytics})
	// A real empty-scope key — INSERTED DIRECTLY, not minted.
	//
	// ⚠ IT CAN NO LONGER BE MINTED, and that is the point of the change this test now guards from
	// the other side: tenant.ValidateScopes refuses an empty list, so no NEW scopeless key can be
	// issued. What could still exist is a row that predates scopes; since B50.33 RequireScope passes
	// it nothing (migration 0324 gave the real ones {proxy}). Writing the row directly is exactly
	// that shape.
	emptyKey := func() string {
		raw, prefix, err := tenant.GenerateKey()
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(raw), bcrypt.MinCost)
		if err != nil {
			t.Fatalf("bcrypt: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO workspace_api_keys (workspace_id, key_hash, key_prefix, name, scopes, created_at)
			 VALUES ($1, $2, $3, $4, '{}', NOW())`, "ws_empty", string(hash), prefix, "legacy"); err != nil {
			t.Fatalf("insert legacy scopeless key: %v", err)
		}
		return raw
	}()

	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	hit := func(chain http.Handler, bearer string) int {
		r := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", nil)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, r)
		return rec.Code
	}

	// BEFORE — the proxy path as it stands on main: AuthMiddleware but no scope
	// guard. The analytics-only key (no proxy scope) reaches the handler. That
	// is precisely the leak this PR closes; asserting it here makes the fix
	// red-first at the integration layer.
	unguarded := AuthMiddleware(keyStore, mgr)(stub)
	if code := hit(unguarded, analyticsKey); code != http.StatusOK {
		t.Fatalf("PRECONDITION (unguarded proxy path): an analytics-only key must reach the handler — "+
			"that is the leak; got %d, want 200", code)
	}

	// AFTER — wrapped with RequireScope(proxy), exactly as the proxy routes now are.
	guarded := AuthMiddleware(keyStore, mgr)(RequireScope(ScopeProxy)(stub))
	for _, c := range []struct {
		name   string
		bearer string
		want   int
	}{
		{"proxy-scoped workspace key passes", proxyKey, http.StatusOK},
		{"analytics-only key forbidden (403 — authenticated, not 401)", analyticsKey, http.StatusForbidden},
		{"empty-scope key passes nothing (403, B50.33)", emptyKey, http.StatusForbidden},
		{"admin/global key unchanged (200)", adminKey, http.StatusOK},
		{"no credential rejected (401)", "", http.StatusUnauthorized},
	} {
		if code := hit(guarded, c.bearer); code != c.want {
			t.Errorf("%s: want %d, got %d", c.name, c.want, code)
		}
	}
}
