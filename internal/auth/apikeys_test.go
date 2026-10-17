package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/lens/internal/backpressure"
)

// expectInsert wires a permissive INSERT expectation for GenerateKey.
func expectInsert(pool pgxmock.PgxPoolIface) {
	pool.ExpectExec(`INSERT INTO api_keys`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(),
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
}

func newPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestGenerateKey_HasTLVPrefix(t *testing.T) {
	pool := newPool(t)
	expectInsert(pool)
	ks := newKeyStore(pool)

	raw, _, err := ks.GenerateKey(context.Background(), "default", "platform", "test", nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if !strings.HasPrefix(raw, "tlv_") {
		t.Errorf("key = %q, want tlv_ prefix", raw)
	}
}

func TestGenerateKey_KeyIs68Characters(t *testing.T) {
	pool := newPool(t)
	expectInsert(pool)
	ks := newKeyStore(pool)

	raw, _, err := ks.GenerateKey(context.Background(), "default", "platform", "test", nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if len(raw) != 68 {
		t.Errorf("len(key) = %d, want 68", len(raw))
	}
}

func TestValidate_ReturnsValidForCorrectKey(t *testing.T) {
	pool := newPool(t)
	expectInsert(pool)
	ks := newKeyStore(pool)

	raw, _, err := ks.GenerateKey(context.Background(), "default", "platform", "test", nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	got := ks.Validate(context.Background(), raw)
	if !got.Valid {
		t.Errorf("Validate(correct key) = invalid; reason=%q", got.Reason)
	}
}

func TestValidate_ReturnsInvalidForWrongKey(t *testing.T) {
	pool := newPool(t)
	pool.ExpectQuery(`FROM api_keys WHERE key_hash`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "key_hash", "workspace_id", "team", "name", "active", "created_at", "last_used_at", "expires_at",
		}))
	ks := newKeyStore(pool)

	got := ks.Validate(context.Background(), "tlv_bogus_key_does_not_exist")
	if got.Valid {
		t.Errorf("Validate(unknown) = valid; expected invalid")
	}
}

func TestValidate_ReturnsInvalidForRevokedKey(t *testing.T) {
	pool := newPool(t)
	expectInsert(pool)
	pool.ExpectExec(`UPDATE api_keys SET active = false`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	// After revoke, cache is cleared, so Validate runs a SELECT that finds nothing.
	pool.ExpectQuery(`FROM api_keys WHERE key_hash`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "key_hash", "workspace_id", "team", "name", "active", "created_at", "last_used_at", "expires_at",
		}))
	ks := newKeyStore(pool)

	raw, apiKey, _ := ks.GenerateKey(context.Background(), "default", "platform", "test", nil)
	if err := ks.Revoke(context.Background(), apiKey.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	got := ks.Validate(context.Background(), raw)
	if got.Valid {
		t.Errorf("Validate(revoked key) = valid; expected invalid")
	}
}

func TestValidate_ReturnsInvalidForExpiredKey(t *testing.T) {
	pool := newPool(t)
	expectInsert(pool)
	ks := newKeyStore(pool)

	past := time.Now().Add(-time.Hour)
	raw, _, _ := ks.GenerateKey(context.Background(), "default", "platform", "test", &past)

	got := ks.Validate(context.Background(), raw)
	if got.Valid {
		t.Errorf("Validate(expired key) = valid; expected invalid")
	}
}

func TestRevoke_RemovesKeyFromCache(t *testing.T) {
	pool := newPool(t)
	expectInsert(pool)
	pool.ExpectExec(`UPDATE api_keys SET active = false`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	ks := newKeyStore(pool)

	raw, apiKey, _ := ks.GenerateKey(context.Background(), "default", "team", "name", nil)

	// Sanity: key is in the cache before revoke.
	if !ks.cacheContains(raw) {
		t.Fatal("setup: expected key in cache after Generate")
	}
	if err := ks.Revoke(context.Background(), apiKey.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if ks.cacheContains(raw) {
		t.Error("Revoke did not remove key from in-memory cache")
	}
}

func TestMiddleware_401WhenNoKeyProvided(t *testing.T) {
	ks := newKeyStore(nil)
	called := false
	h := AuthMiddleware(ks, nil)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if called {
		t.Error("next handler should not be called when no key is provided")
	}
}

func TestMiddleware_401ForInvalidKey(t *testing.T) {
	pool := newPool(t)
	pool.ExpectQuery(`FROM api_keys WHERE key_hash`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "key_hash", "workspace_id", "team", "name", "active", "created_at", "last_used_at", "expires_at",
		}))
	ks := newKeyStore(pool)

	called := false
	h := AuthMiddleware(ks, nil)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer tlv_bogus")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if called {
		t.Error("next handler should not be called for invalid key")
	}
}

func TestMiddleware_CallsNextHandlerForValidKey(t *testing.T) {
	pool := newPool(t)
	expectInsert(pool)
	ks := newKeyStore(pool)

	raw, _, _ := ks.GenerateKey(context.Background(), "default", "team", "test", nil)

	called := false
	h := AuthMiddleware(ks, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !called {
		t.Error("next handler was not called for a valid key")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestMiddleware_SetsWorkspaceHeaderFromAPIKey(t *testing.T) {
	pool := newPool(t)
	expectInsert(pool)
	ks := newKeyStore(pool)

	raw, _, _ := ks.GenerateKey(context.Background(), "team-a-workspace", "team-a", "test", nil)

	var sawWorkspace, sawTeam string
	h := AuthMiddleware(ks, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawWorkspace = r.Header.Get("X-Talyvor-Workspace")
		sawTeam = r.Header.Get("X-Talyvor-Team")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	// Client-sent workspace must be overridden by the middleware.
	req.Header.Set("X-Talyvor-Workspace", "spoofed-by-client")
	// B42.10: a key with its own team cannot be re-attributed by the header.
	req.Header.Set("X-Talyvor-Team", "spoofed-team")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if sawWorkspace != "team-a-workspace" {
		t.Errorf("X-Talyvor-Workspace = %q, want %q", sawWorkspace, "team-a-workspace")
	}
	if sawTeam != "team-a" {
		t.Errorf("X-Talyvor-Team = %q, want %q", sawTeam, "team-a")
	}
}

// ─── last_used_at writer bound (#174) ─────────────

// signalDB is a minimal pgxDB whose Exec signals execCh, letting the
// async-updater tests synchronize without sleeps-as-assertions.
type signalDB struct{ execCh chan struct{} }

func (s *signalDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	s.execCh <- struct{}{}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (s *signalDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (s *signalDB) QueryRow(context.Context, string, ...any) pgx.Row        { return nil }

// seedCachedKey plants an active key in the in-memory cache so Validate takes
// the cache-hit path straight into finishValidate (no DB read involved).
func seedCachedKey(ks *KeyStore, raw string) {
	ks.mu.Lock()
	ks.cache[hashKey(raw)] = &cacheEntry{
		key:      &APIKey{ID: "k-test", WorkspaceID: "ws-test", Active: true, CreatedAt: time.Now()},
		cachedAt: time.Now(),
	}
	ks.mu.Unlock()
}

// A saturated limiter sheds the last_used_at update — validation still
// succeeds (the bound is bookkeeping-only) — and admits again once freed.
func TestFinishValidate_LimiterSaturated_ShedsUpdate(t *testing.T) {
	db := &signalDB{execCh: make(chan struct{}, 4)}
	ks := newKeyStore(db)
	seedCachedKey(ks, "tlv_rawkey")
	l := backpressure.New(1)
	ks.SetWriteLimiter(l)

	if !l.TryAcquire() { // occupy the only slot, as an in-flight writer would
		t.Fatal("setup: could not occupy the slot")
	}
	res := ks.Validate(context.Background(), "tlv_rawkey")
	if !res.Valid {
		t.Fatal("validation itself must SUCCEED when the bookkeeping update is shed")
	}
	select {
	case <-db.execCh:
		t.Fatal("saturated limiter must shed the last_used_at update")
	case <-time.After(100 * time.Millisecond):
	}
	if l.Dropped() != 1 {
		t.Fatalf("dropped = %d, want 1", l.Dropped())
	}

	l.Release()
	res = ks.Validate(context.Background(), "tlv_rawkey")
	if !res.Valid {
		t.Fatal("second validation must succeed")
	}
	select {
	case <-db.execCh:
	case <-time.After(2 * time.Second):
		t.Fatal("freed limiter must admit the update")
	}
}

// A nil limiter (not wired / bound disabled) preserves pre-limiter behavior.
func TestFinishValidate_NilLimiter_Updates(t *testing.T) {
	db := &signalDB{execCh: make(chan struct{}, 1)}
	ks := newKeyStore(db)
	seedCachedKey(ks, "tlv_rawkey2")
	res := ks.Validate(context.Background(), "tlv_rawkey2")
	if !res.Valid {
		t.Fatal("validation must succeed")
	}
	select {
	case <-db.execCh:
	case <-time.After(2 * time.Second):
		t.Fatal("nil limiter must not gate the last_used_at update")
	}
}
