package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/phonesignin"
	"github.com/talyvor/lens/internal/workspace"
)

// B61.81 — the key routes' tightenings, mounted as main.go mounts them, on a migrated schema: removing a key needs the
// proof adding one does, and a register challenge is issued only in an account the person holds.

func b6181Routes(t *testing.T) (*pgxpool.Pool, *sharedApprovalChallenge, func(path string, body any) (int, map[string]any)) {
	t.Helper()
	const secret = "provision-secret"
	pool := phoneRoutesDB(t)
	store := &phonesignin.Store{DB: pool, Sender: partners.NewRegistry(nil).TextMessages, Key: []byte(secret),
		RP: &passkey.RelyingParty{ID: "app.talyvor.test", Origins: []string{"https://app.talyvor.test"}}}
	approval := &sharedApprovalChallenge{}
	r := chi.NewRouter()
	mountKeySignInRoutes(r, secret, store, approval, workspace.New(pool))
	return pool, approval, func(path string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
		req.Header.Set(provisionSecretHeader, secret)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
}

func b6181Person(t *testing.T, pool *pgxpool.Pool, id, phone string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO people (id, phone, identity, phone_sandbox, phone_confirmed_at)
		VALUES ($1, $2, $3, true, now())`, id, phone, "person:"+id); err != nil {
		t.Fatal(err)
	}
}

// A session alone removes no key: it takes a ticket from one of the person's keys (the one going included).
func TestB6181RemovingAKeyNeedsProof(t *testing.T) {
	pool, _, call := b6181Routes(t)
	ctx := context.Background()
	b6181Person(t, pool, "per_rm", "+447700908131")
	for _, id := range []string{"pky_rm_one", "pky_rm_two"} {
		if _, err := pool.Exec(ctx, `INSERT INTO person_keys (id, person_id, kind, platform, public_key) VALUES ($1, 'per_rm', 'device', 'test', $2)`,
			id, []byte(id)); err != nil {
			t.Fatal(err)
		}
	}
	if code, out := call("/v1/auth/passkey/keys/remove", map[string]any{"person_id": "per_rm", "key_id": "pky_rm_one"}); code != http.StatusForbidden || out["reason"] != "proof_needed" {
		t.Fatalf("removing a key with a session alone: %d %v, want 403 proof_needed", code, out)
	}
	// A ticket from signing in with the key being removed is proof.
	const ticket = "b6181-ticket-from-key-one"
	sum := sha256.Sum256([]byte(ticket))
	if _, err := pool.Exec(ctx, `INSERT INTO person_key_tickets (ticket_hash, person_id, key_id, expires_at) VALUES ($1, 'per_rm', 'pky_rm_one', $2)`,
		sum[:], time.Now().Add(phonesignin.KeyTicketLife)); err != nil {
		t.Fatal(err)
	}
	if code, out := call("/v1/auth/passkey/keys/remove", map[string]any{"person_id": "per_rm", "key_id": "pky_rm_one", "key_ticket": ticket}); code != http.StatusOK {
		t.Fatalf("removing a key with its own sign-in's ticket: %d %v", code, out)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM person_keys WHERE person_id = 'per_rm' AND removed_at IS NULL`).Scan(&left); err != nil || left != 1 {
		t.Fatalf("keys left = %d (%v), want 1", left, err)
	}
}

// With workspace_id set, the register challenge is issued only in one of the person's accounts.
func TestB6181RegisterChallengeOnlyInThePersonsAccount(t *testing.T) {
	pool, approval, call := b6181Routes(t)
	ctx := context.Background()
	b6181Person(t, pool, "per_ws", "+447700908141")
	b6181Person(t, pool, "per_other", "+447700908142")
	mine, theirs := deriveWorkspaceID("person:per_ws"), deriveWorkspaceID("person:per_other")
	for _, ws := range []string{mine, theirs} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	var before int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM person_key_challenges`).Scan(&before)
	code, out := call("/v1/auth/passkey/register/challenge", map[string]string{"person_id": "per_ws", "workspace_id": theirs})
	var after int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM person_key_challenges`).Scan(&after)
	if code != http.StatusForbidden || out["reason"] != "not_your_account" || len(approval.issued) != 0 || after != before {
		t.Fatalf("a challenge in another person's account: %d %v (approval issued %d, person challenges %d → %d), want 403 and nothing issued",
			code, out, len(approval.issued), before, after)
	}
	if code, out := call("/v1/auth/passkey/register/challenge", map[string]string{"person_id": "per_ws", "workspace_id": mine}); code != http.StatusOK || len(approval.issued) != 1 {
		t.Fatalf("a challenge in the person's own account: %d %v", code, out)
	}
}
