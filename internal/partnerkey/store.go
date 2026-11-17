// Package partnerkey is B62.5's partner read credential: the key the account partner's risk team reads every agent
// payment with. It has ONE power, reading GET /v1/partner/agent-activity, its CSV export and its summary, and never
// writes: auth.Manager refuses it on every other route (auth.ErrPartnerRoute), so a handler cannot widen it.
//
// An operator makes and revokes one with the admin key and a step-up code (cmd/lens/partner_activity_handler.go). It
// is stored as a sha256 hash (the raw key is shown once, at creation), and every read is a partner_key_uses row,
// written before the read is answered. Migration 2611170423a.
package partnerkey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// KeyPrefix marks a partner key. Disjoint from the workspace (tlv_ws_), session (tlv_sk_) and moderator
	// (tlv_mod_) prefixes, so auth.Manager can route on it without a lookup.
	KeyPrefix = "tlv_prk_"

	keyRandBytes     = 24
	displayPrefixLen = len(KeyPrefix) + 8
)

// ErrInvalid covers an unknown, malformed and revoked key alike, so the wire never says which.
var ErrInvalid = errors.New("partnerkey: invalid, unknown or revoked partner key")

// Key is one partner key, never its raw value.
type Key struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	KeyPrefix string     `json:"key_prefix"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	RevokedBy string     `json:"revoked_by,omitempty"`
}

// Use is one recorded read by a partner key.
type Use struct {
	KeyID     int64     `json:"key_id"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Query     string    `json:"query"`
	CreatedAt time.Time `json:"created_at"`
}

// Store reads and writes partner_keys and partner_key_uses.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a Store over pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func hash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ErrNotFound is a revoke of a key that does not exist.
var ErrNotFound = errors.New("partnerkey: no such partner key")

// Create makes a new key named name, created by by, and returns its raw value: the only time it exists outside the
// caller's hands.
func (s *Store) Create(ctx context.Context, name, by string) (string, *Key, error) {
	name, by = strings.TrimSpace(name), strings.TrimSpace(by)
	if name == "" || by == "" {
		return "", nil, errors.New("partnerkey: a name and the operator making it are required")
	}
	if s.pool == nil {
		return "", nil, errors.New("partnerkey: no database configured")
	}
	buf := make([]byte, keyRandBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("partnerkey: read random: %w", err)
	}
	raw := KeyPrefix + hex.EncodeToString(buf)
	k := &Key{Name: name, KeyPrefix: raw[:displayPrefixLen], CreatedBy: by}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO partner_keys (name, key_hash, key_prefix, created_by) VALUES ($1, $2, $3, $4) RETURNING id, created_at`,
		k.Name, hash(raw), k.KeyPrefix, k.CreatedBy).Scan(&k.ID, &k.CreatedAt)
	if err != nil {
		return "", nil, fmt.Errorf("partnerkey: insert: %w", err)
	}
	return raw, k, nil
}

// Validate returns the key raw belongs to, or ErrInvalid when it is malformed, unknown or revoked. It reads the
// database every time, so a revocation takes effect on the next request.
func (s *Store) Validate(ctx context.Context, raw string) (*Key, error) {
	if !strings.HasPrefix(raw, KeyPrefix) || len(raw) != len(KeyPrefix)+keyRandBytes*2 || s.pool == nil {
		return nil, ErrInvalid
	}
	var k Key
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, key_prefix, created_by, created_at, revoked_at, revoked_by FROM partner_keys WHERE key_hash = $1`,
		hash(raw)).Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.CreatedBy, &k.CreatedAt, &k.RevokedAt, &k.RevokedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("partnerkey: lookup: %w", err)
	}
	if k.RevokedAt != nil {
		return nil, ErrInvalid
	}
	return &k, nil
}

// Revoke stops key id from working. Revoking an already-revoked key keeps its first revocation.
func (s *Store) Revoke(ctx context.Context, id int64, by string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE partner_keys SET revoked_at = now(), revoked_by = $2 WHERE id = $1 AND revoked_at IS NULL`, id, by)
	if err != nil {
		return fmt.Errorf("partnerkey: revoke: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM partner_keys WHERE id = $1)`, id).Scan(&exists); err != nil {
			return fmt.Errorf("partnerkey: revoke: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}

// List returns every key, newest first.
func (s *Store) List(ctx context.Context) ([]Key, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, key_prefix, created_by, created_at, revoked_at, revoked_by FROM partner_keys ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("partnerkey: list: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Key, error) {
		var k Key
		err := row.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.CreatedBy, &k.CreatedAt, &k.RevokedAt, &k.RevokedBy)
		return k, err
	})
}

// RecordUse appends one read by key keyID to the audit trail.
func (s *Store) RecordUse(ctx context.Context, keyID int64, method, path, query string) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO partner_key_uses (key_id, method, path, query) VALUES ($1, $2, $3, $4)`, keyID, method, path, query); err != nil {
		return fmt.Errorf("partnerkey: record use: %w", err)
	}
	return nil
}

// Uses returns key keyID's recorded reads, newest first, at most limit.
func (s *Store) Uses(ctx context.Context, keyID int64, limit int) ([]Use, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT key_id, method, path, query, created_at FROM partner_key_uses WHERE key_id = $1 ORDER BY id DESC LIMIT $2`,
		keyID, limit)
	if err != nil {
		return nil, fmt.Errorf("partnerkey: uses: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Use, error) {
		var u Use
		err := row.Scan(&u.KeyID, &u.Method, &u.Path, &u.Query, &u.CreatedAt)
		return u, err
	})
}
