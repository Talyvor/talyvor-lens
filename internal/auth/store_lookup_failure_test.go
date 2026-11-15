package auth

// B28.243 — a database hiccup is 503, not a sign-out, for every credential whose store is asked:
// a workspace key (tenant.Store), an api_keys key (KeyStore) and a moderator key. B27.5 did this
// for session keys only; the other three still answered 401 when the lookup itself failed.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/moderatorkey"
	"github.com/talyvor/lens/internal/tenant"
)

// deadPool is a pool whose every query fails to connect — nothing listens on port 1. It is the
// hiccup itself: not a miss, a database that did not answer.
func deadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://lens@127.0.0.1:1/lens?connect_timeout=1&sslmode=disable")
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestStoreLookupFailureIs503AndAMissStays401(t *testing.T) {
	keys := map[string]string{
		"workspace key": tenant.KeyPrefix + strings.Repeat("a", 40),
		"api_keys key":  keyPrefix + strings.Repeat("b", 2*keyRandBytes),
		"moderator key": moderatorkey.KeyPrefix + strings.Repeat("c", 48),
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	// A store with no database answers "not found" — the wrong-key case, which must stay 401.
	miss := NewManager("b28243-global", nil, New(nil), tenant.NewStore(nil)).
		WithModeratorKeys(moderatorkey.NewStore(nil))
	pool := deadPool(t)
	down := NewManager("b28243-global", nil, New(pool), tenant.NewStore(pool)).
		WithModeratorKeys(moderatorkey.NewStore(pool))

	chains := map[string]func(*Manager) http.Handler{
		"AuthMiddleware":     func(m *Manager) http.Handler { return AuthMiddleware(m.keyStore, m)(ok) },
		"Manager.Middleware": func(m *Manager) http.Handler { return m.Middleware(nil, nil)(ok) },
	}
	for chain, wrap := range chains {
		for name, raw := range keys {
			if chain == "Manager.Middleware" && name == "api_keys key" {
				continue // Manager.Middleware never asks the KeyStore
			}
			rec := httptest.NewRecorder()
			wrap(miss).ServeHTTP(rec, skRequest(raw))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s, unknown %s: %d, want 401 — only a failed lookup may become a 503", chain, name, rec.Code)
			}

			rec = httptest.NewRecorder()
			wrap(down).ServeHTTP(rec, skRequest(raw))
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("%s, %s with the database down: %d, want 503 — a 401 signs the person out for a "+
					"database hiccup (%s)", chain, name, rec.Code, rec.Body.String())
				continue
			}
			if got := rec.Header().Get("Retry-After"); got != AuthUnavailableRetryAfter {
				t.Errorf("%s, %s: Retry-After = %q, want %q", chain, name, got, AuthUnavailableRetryAfter)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["code"] != authUnavailableCode {
				t.Errorf("%s, %s: body = %s, want code %q", chain, name, rec.Body.String(), authUnavailableCode)
			}
		}
	}

	// The global key needs no database: it still works while the database is down.
	rec := httptest.NewRecorder()
	AuthMiddleware(down.keyStore, down)(ok).ServeHTTP(rec, skRequest("b28243-global"))
	if rec.Code != http.StatusOK {
		t.Fatalf("global key with the database down: %d, want 200", rec.Code)
	}
}
