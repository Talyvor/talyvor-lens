// Package byok is BYOK, the one subscription tier (B27.26): a workspace on the BYOK plan — or, B32.12, on a plan
// whose gates include own keys (business and enterprise; team with the BYOK add-on) — stores its own provider
// keys, its requests go upstream on them, and Talyvor charges it no tokens for those requests.
//
// Custody follows docs/provider-secret-envelope.md §"The contract": a key is accepted only while
// LENS_PROVIDER_SECRET_KEK is armed (the caller registers no route otherwise), it is sealed by
// internal/envelope with the row's owner and slot as aad, and nothing here returns it to an API — List
// shows the last four characters and nothing else. The one place a key is opened is OwnKeys, for the
// serving path, which sends it only to its own provider.
package byok

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/envelope"
	"github.com/talyvor/lens/internal/plans"
)

// Providers are the providers a workspace can bring a key for: each authenticates with one API key.
// vLLM (self-hosted) and Bedrock (AWS credentials) do not, so BYOK does not serve them.
var Providers = []string{"anthropic", "google", "groq", "mistral", "openai"}

// Supported reports whether provider takes a BYOK key.
func Supported(provider string) bool {
	for _, p := range Providers {
		if p == provider {
			return true
		}
	}
	return false
}

// ErrUnsupportedProvider is a key for a provider BYOK does not serve.
var ErrUnsupportedProvider = errors.New("byok: provider does not take a key (anthropic, google, groq, mistral, openai)")

// ErrInvalidKey is a key that cannot be a provider API key.
var ErrInvalidKey = errors.New("byok: the key must be 8 to 512 printable characters with no spaces")

// Key is what is ever shown of a stored key.
type Key struct {
	Provider  string    `json:"provider"`
	Last4     string    `json:"last4"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store keeps workspaces' provider keys, sealed.
type Store struct {
	pool *pgxpool.Pool
	ring *envelope.Keyring
}

// New returns a Store sealing under ring. A nil ring is custody disabled; the caller must not build one.
func New(pool *pgxpool.Pool, ring *envelope.Keyring) *Store {
	return &Store{pool: pool, ring: ring}
}

// aad binds a sealed key to its workspace and provider, so a row copied into another slot fails to open.
func aad(workspaceID, provider string) []byte { return []byte(workspaceID + "|" + provider) }

// Subscribed reports whether the workspace's plan lets it use its own provider keys (B32.12, LENS_PLAN_GATES):
// business, enterprise and byok include them, team has them with the BYOK add-on, and no other plan does.
func (s *Store) Subscribed(ctx context.Context, workspaceID string) (bool, error) {
	plan, err := plans.Of(ctx, s.pool, workspaceID)
	if err != nil {
		return false, fmt.Errorf("byok: %w", err)
	}
	return plan.OwnKeysAllowed(), nil
}

// Put stores (or replaces) the workspace's key for provider. It refuses, with a *plans.Refusal, a workspace whose
// plan does not let it use its own keys.
func (s *Store) Put(ctx context.Context, workspaceID, provider, key string) (Key, error) {
	if !Supported(provider) {
		return Key{}, ErrUnsupportedProvider
	}
	key = strings.TrimSpace(key)
	if !validKey(key) {
		return Key{}, ErrInvalidKey
	}
	plan, err := plans.Of(ctx, s.pool, workspaceID)
	if err != nil {
		return Key{}, fmt.Errorf("byok: %w", err)
	}
	if !plan.OwnKeysAllowed() {
		return Key{}, plan.RefuseOwnKeys(plans.Current())
	}
	sealed, err := s.ring.Seal([]byte(key), aad(workspaceID, provider))
	if err != nil {
		return Key{}, fmt.Errorf("byok: seal: %w", err)
	}
	out := Key{Provider: provider, Last4: key[len(key)-4:]}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO workspace_provider_keys
			(workspace_id, provider, key_id, wrapped_dek, dek_nonce, ciphertext, ct_nonce, last4)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (workspace_id, provider) DO UPDATE SET
			key_id = EXCLUDED.key_id, wrapped_dek = EXCLUDED.wrapped_dek, dek_nonce = EXCLUDED.dek_nonce,
			ciphertext = EXCLUDED.ciphertext, ct_nonce = EXCLUDED.ct_nonce, last4 = EXCLUDED.last4,
			updated_at = NOW()
		RETURNING updated_at`,
		workspaceID, provider, sealed.KeyID, sealed.WrappedDEK, sealed.DEKNonce, sealed.Ciphertext, sealed.CTNonce,
		out.Last4).Scan(&out.UpdatedAt)
	if err != nil {
		return Key{}, fmt.Errorf("byok: store key: %w", err)
	}
	return out, nil
}

// List is the workspace's stored keys: provider, last four characters, when set. Never the key.
func (s *Store) List(ctx context.Context, workspaceID string) ([]Key, error) {
	rows, err := s.pool.Query(ctx, `SELECT provider, last4, updated_at FROM workspace_provider_keys
		WHERE workspace_id = $1 ORDER BY provider`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("byok: list keys: %w", err)
	}
	defer rows.Close()
	keys := []Key{}
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.Provider, &k.Last4, &k.UpdatedAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// Delete removes the workspace's key for provider. false when there was none.
func (s *Store) Delete(ctx context.Context, workspaceID, provider string) (bool, error) {
	ct, err := s.pool.Exec(ctx, `DELETE FROM workspace_provider_keys WHERE workspace_id = $1 AND provider = $2`,
		workspaceID, provider)
	if err != nil {
		return false, fmt.Errorf("byok: delete key: %w", err)
	}
	return ct.RowsAffected() == 1, nil
}

// OwnKeys opens the workspace's keys, by provider, for the serving path — only while its plan lets it use them
// (Subscribed); empty otherwise. A key that cannot be opened (its KEK was dropped) is left out and the
// error says which, so the request is served on Talyvor's key rather than refused.
func (s *Store) OwnKeys(ctx context.Context, workspaceID string) (map[string]string, error) {
	keys := map[string]string{}
	if on, err := s.Subscribed(ctx, workspaceID); err != nil || !on {
		return keys, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT k.provider, k.key_id, k.wrapped_dek, k.dek_nonce, k.ciphertext, k.ct_nonce
		FROM workspace_provider_keys k WHERE k.workspace_id = $1`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("byok: read keys: %w", err)
	}
	defer rows.Close()
	var errs []error
	for rows.Next() {
		var provider string
		var sealed envelope.Sealed
		if err := rows.Scan(&provider, &sealed.KeyID, &sealed.WrappedDEK, &sealed.DEKNonce, &sealed.Ciphertext, &sealed.CTNonce); err != nil {
			return nil, err
		}
		if err := s.ring.Use(sealed, aad(workspaceID, provider), func(pt []byte) error {
			keys[provider] = string(pt)
			return nil
		}); err != nil {
			errs = append(errs, fmt.Errorf("byok: %s key of workspace %s cannot be opened, so it must be entered again: %w", provider, workspaceID, err))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return keys, errors.Join(errs...)
}

func validKey(k string) bool {
	if len(k) < 8 || len(k) > 512 {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] <= ' ' || k[i] > '~' {
			return false
		}
	}
	return true
}
