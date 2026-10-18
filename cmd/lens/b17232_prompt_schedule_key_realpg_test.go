package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// keyAsker answers every prompt, recording the key it was asked as.
type keyAsker struct{ keys []string }

func (a *keyAsker) AskAsKey(_ context.Context, _, keyID, _, _, _, _ string) (string, error) {
	a.keys = append(a.keys, keyID)
	return "42", nil
}

// B17.232 — an agent made on Agent Wallets has no key. A prompt scheduled on it from Chat is still asked at its time,
// as a proxy key of the workspace's that Lens issued the agent when the prompt was scheduled — not refused for having
// no key. A second schedule on the same agent issues no second key.
func TestB17232_APromptScheduledOnAnAgentWithNoKeyIsAskedAsAKeyLensIssuedIt(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-prompt-key"
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	a, err := store.CreateAgent(ctx, ws, "Scheduler", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(time.Hour).Truncate(time.Minute)
	schedule := func(prompt string) {
		t.Helper()
		body := `{"prompt":"` + prompt + `","provider":"openai","model":"gpt-4o","every":"once","first_run_at":"` + at.Format(time.RFC3339) + `"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+ws+"/agents/"+a.ID+"/prompt-schedules", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("schedule = %d %s", w.Code, w.Body.String())
		}
	}
	schedule("What is 6 * 7?")
	schedule("What is 40 + 2?")

	key, err := store.AgentKey(ctx, ws, a.ID)
	if err != nil || key == "" {
		t.Fatalf("after scheduling, the agent's key is %q (%v), want one Lens issued", key, err)
	}
	var keys int
	var scopes []string
	if err := pool.QueryRow(ctx, `SELECT count(*), max(k.scopes::text)::text[] FROM agent_account_keys a
		JOIN workspace_api_keys k ON k.id::text = a.scoped_key_id WHERE a.agent_id = $1 AND k.workspace_id = $2`, a.ID, ws).Scan(&keys, &scopes); err != nil {
		t.Fatal(err)
	}
	if keys != 1 || len(scopes) != 1 || scopes[0] != auth.ScopeProxy {
		t.Fatalf("two schedules left the agent %d keys scoped %v, want one proxy key", keys, scopes)
	}

	ask := &keyAsker{}
	answered, refused, err := store.RunPromptSchedules(ctx, at, ask)
	if err != nil || answered != 2 || refused != 0 {
		t.Fatalf("at their time: %d answered, %d refused (%v), want both answered", answered, refused, err)
	}
	for _, k := range ask.keys {
		if k != key {
			t.Fatalf("a prompt was asked as key %q, want the agent's %q", k, key)
		}
	}
	list, err := store.ListPromptSchedules(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range list {
		if len(sc.Runs) != 1 || sc.Runs[0].Outcome != "answered" {
			out, _ := json.Marshal(sc.Runs)
			t.Fatalf("schedule %q ran %s, want answered once", sc.Prompt, out)
		}
	}
}
