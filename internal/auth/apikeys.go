package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/backpressure"
)

const (
	keyPrefix    = "tlv_"
	keyRandBytes = 32 // 32 bytes → 64 hex chars → 68 total with prefix
	cacheTTL     = 5 * time.Minute
)

// pgxDB is the subset of *pgxpool.Pool the auth store needs. nil pool means
// "in-memory only" — useful in tests that don't exercise the DB path.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type KeyStore struct {
	pool  pgxDB
	mu    sync.RWMutex
	cache map[string]*cacheEntry // key: sha256 hex of raw key

	// writeLimiter bounds the concurrent async last_used_at updaters that
	// finishValidate spawns — one goroutine per successful validation, i.e.
	// per authenticated request. nil = unbounded (pre-limiter behavior).
	// Same exposure class #165 bounded for attribution/pattern-capture:
	// under overload these writers pile up against the pool and convert
	// load into connection churn. last_used_at is best-effort bookkeeping,
	// so drop-on-overflow is the correct backpressure.
	writeLimiter *backpressure.Limiter
}

// SetWriteLimiter installs the shared observational-write bound (see
// internal/backpressure). Additive and optional: unset admits everything.
func (k *KeyStore) SetWriteLimiter(l *backpressure.Limiter) {
	if k != nil {
		k.writeLimiter = l
	}
}

// cacheEntry pairs an APIKey with the time it entered the cache so we can
// enforce a 5-minute TTL.
type cacheEntry struct {
	key      *APIKey
	cachedAt time.Time
}

type APIKey struct {
	ID          string     `json:"id"`
	KeyHash     string     `json:"-"`
	WorkspaceID string     `json:"workspace_id"`
	Team        string     `json:"team"`
	Name        string     `json:"name"`
	Active      bool       `json:"active"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

type ValidationResult struct {
	Valid  bool
	APIKey *APIKey
	Reason string
}

func New(pool *pgxpool.Pool) *KeyStore {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newKeyStore(db)
}

func newKeyStore(pool pgxDB) *KeyStore {
	return &KeyStore{
		pool:  pool,
		cache: make(map[string]*cacheEntry),
	}
}

func hashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

const insertKeySQL = `INSERT INTO api_keys
  (id, key_hash, workspace_id, team, name, active, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`

// GenerateKey creates a fresh API key. The raw key string is returned
// ONCE and never persisted — only its sha256 hash lives in the database
// and the in-memory cache.
func (k *KeyStore) GenerateKey(ctx context.Context, workspaceID, team, name string, expiresAt *time.Time) (string, *APIKey, error) {
	buf := make([]byte, keyRandBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("auth: read random bytes: %w", err)
	}
	raw := keyPrefix + hex.EncodeToString(buf)
	hash := hashKey(raw)

	apiKey := &APIKey{
		ID:          uuid.NewString(),
		KeyHash:     hash,
		WorkspaceID: workspaceID,
		Team:        team,
		Name:        name,
		Active:      true,
		CreatedAt:   time.Now().UTC(),
		ExpiresAt:   expiresAt,
	}

	if k.pool != nil {
		if _, err := k.pool.Exec(ctx, insertKeySQL,
			apiKey.ID, apiKey.KeyHash, apiKey.WorkspaceID, apiKey.Team, apiKey.Name, apiKey.Active, apiKey.ExpiresAt,
		); err != nil {
			return "", nil, fmt.Errorf("auth: insert api_key: %w", err)
		}
	}

	k.mu.Lock()
	k.cache[hash] = &cacheEntry{key: apiKey, cachedAt: time.Now()}
	k.mu.Unlock()

	return raw, apiKey, nil
}

const selectKeySQL = `SELECT id, key_hash, workspace_id, team, name, active, created_at, last_used_at, expires_at
FROM api_keys
WHERE key_hash = $1 AND active = true`

const updateLastUsedSQL = `UPDATE api_keys SET last_used_at = NOW() WHERE id = $1`

// Validate checks the raw API key against the in-memory cache (hot path)
// then falls through to the DB on a cache miss. Returns ValidationResult
// with Reason populated on failure so the middleware can surface why.
func (k *KeyStore) Validate(ctx context.Context, raw string) ValidationResult {
	if raw == "" {
		return ValidationResult{Reason: "API key required"}
	}
	hash := hashKey(raw)

	if apiKey, ok := k.cacheLookup(hash); ok {
		return k.finishValidate(ctx, apiKey)
	}

	if k.pool == nil {
		return ValidationResult{Reason: "invalid API key"}
	}

	var apiKey APIKey
	err := k.pool.QueryRow(ctx, selectKeySQL, hash).Scan(
		&apiKey.ID, &apiKey.KeyHash, &apiKey.WorkspaceID, &apiKey.Team,
		&apiKey.Name, &apiKey.Active, &apiKey.CreatedAt, &apiKey.LastUsedAt, &apiKey.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ValidationResult{Reason: "invalid API key"}
	}
	if err != nil {
		slog.Warn("auth: validate query failed", slog.String("err", err.Error()))
		return ValidationResult{Reason: "invalid API key"}
	}

	k.mu.Lock()
	k.cache[hash] = &cacheEntry{key: &apiKey, cachedAt: time.Now()}
	k.mu.Unlock()

	return k.finishValidate(ctx, &apiKey)
}

// cacheLookup returns a cached APIKey unless the entry has gone stale.
// Returns (nil, false) when the entry is missing or expired so the
// caller refetches.
func (k *KeyStore) cacheLookup(hash string) (*APIKey, bool) {
	k.mu.RLock()
	entry, ok := k.cache[hash]
	k.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Since(entry.cachedAt) > cacheTTL {
		return nil, false
	}
	return entry.key, true
}

// finishValidate applies the active/expiry checks and fires the async
// last_used_at update. These run on every successful lookup, cache or DB.
func (k *KeyStore) finishValidate(ctx context.Context, apiKey *APIKey) ValidationResult {
	if !apiKey.Active {
		return ValidationResult{Reason: "API key has been revoked"}
	}
	if apiKey.ExpiresAt != nil && apiKey.ExpiresAt.Before(time.Now()) {
		return ValidationResult{Reason: "API key has expired"}
	}
	// Async last_used_at: fire and forget, fresh context so a request
	// cancel mid-handler doesn't abort the bookkeeping update. The 5s
	// timeout alone cannot prevent goroutine pile-up under overload (at
	// 250 req/s it admits ~1,250 concurrent writers); the limiter is the
	// actual bound — when saturated the update is dropped (best-effort
	// bookkeeping; the row just keeps a slightly staler timestamp).
	if k.pool != nil {
		if !k.writeLimiter.TryAcquire() {
			if k.writeLimiter.LogDrop() {
				slog.Warn("auth: last_used_at update dropped (writer bound reached; bookkeeping only)",
					slog.Int64("dropped_total", k.writeLimiter.Dropped()))
			}
		} else {
			go func(id string) {
				defer k.writeLimiter.Release()
				updateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, _ = k.pool.Exec(updateCtx, updateLastUsedSQL, id)
			}(apiKey.ID)
		}
	}
	_ = ctx
	return ValidationResult{Valid: true, APIKey: apiKey}
}

const revokeKeySQL = `UPDATE api_keys SET active = false WHERE id = $1`

// Revoke marks the key inactive in the DB and evicts every cache entry
// that maps to that key ID. Keyed by ID (not hash) so admin tools can
// revoke without having the raw key in hand.
func (k *KeyStore) Revoke(ctx context.Context, keyID string) error {
	if k.pool != nil {
		if _, err := k.pool.Exec(ctx, revokeKeySQL, keyID); err != nil {
			return fmt.Errorf("auth: revoke api_key: %w", err)
		}
	}
	k.mu.Lock()
	for hash, entry := range k.cache {
		if entry.key.ID == keyID {
			delete(k.cache, hash)
		}
	}
	k.mu.Unlock()
	return nil
}

// EvictWorkspace drops every cached key of a workspace, so a key whose row is gone is looked up, and refused,
// on its next use (B52.37: the account was deleted).
func (k *KeyStore) EvictWorkspace(workspaceID string) {
	k.mu.Lock()
	for hash, entry := range k.cache {
		if entry.key.WorkspaceID == workspaceID {
			delete(k.cache, hash)
		}
	}
	k.mu.Unlock()
}

const selectKeyWorkspaceSQL = `SELECT workspace_id FROM api_keys WHERE id = $1`

// WorkspaceForKeyID resolves the owning workspace of an API key by its ID, and
// whether the key exists. Used for the ownership check before a key is revoked
// (#146 Phase 3): a non-admin may revoke only keys in its OWN workspace. Reads
// the DB (so it sees keys not in the active cache); falls back to the in-memory
// cache when no pool is wired (tests). A missing key is (",", false, nil).
func (k *KeyStore) WorkspaceForKeyID(ctx context.Context, keyID string) (workspaceID string, found bool, err error) {
	if k.pool == nil {
		k.mu.RLock()
		defer k.mu.RUnlock()
		for _, entry := range k.cache {
			if entry.key.ID == keyID {
				return entry.key.WorkspaceID, true, nil
			}
		}
		return "", false, nil
	}
	var ws string
	if scanErr := k.pool.QueryRow(ctx, selectKeyWorkspaceSQL, keyID).Scan(&ws); scanErr != nil {
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("auth: lookup api_key workspace: %w", scanErr)
	}
	return ws, true, nil
}

const loadAllSQL = `SELECT id, key_hash, workspace_id, team, name, active, created_at, last_used_at, expires_at
FROM api_keys
WHERE active = true AND (expires_at IS NULL OR expires_at > NOW())`

func (k *KeyStore) LoadAll(ctx context.Context) error {
	if k.pool == nil {
		return nil
	}
	rows, err := k.pool.Query(ctx, loadAllSQL)
	if err != nil {
		return fmt.Errorf("auth: load all: %w", err)
	}
	defer rows.Close()

	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	for rows.Next() {
		apiKey := &APIKey{}
		if err := rows.Scan(
			&apiKey.ID, &apiKey.KeyHash, &apiKey.WorkspaceID, &apiKey.Team,
			&apiKey.Name, &apiKey.Active, &apiKey.CreatedAt, &apiKey.LastUsedAt, &apiKey.ExpiresAt,
		); err != nil {
			return fmt.Errorf("auth: scan api_key: %w", err)
		}
		k.cache[apiKey.KeyHash] = &cacheEntry{key: apiKey, cachedAt: now}
	}
	return rows.Err()
}

// cacheContains is a tiny test helper kept package-private — exported
// methods don't need to expose internal cache state.
func (k *KeyStore) cacheContains(raw string) bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	_, ok := k.cache[hashKey(raw)]
	return ok
}
