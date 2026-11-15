package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B61.35 — through the routes the apps call, on a migrated schema: an agent that exists reads back the picture its
// colour had; a new agent keeps the kind of work and the picture it was made with, and its colour follows the picture;
// the owner changes either with PATCH; an unknown kind or picture is refused in words.
func TestAgentRoutes_AnAgentKeepsItsKindAndItsPicture(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-pictures"
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	base := "/v1/workspaces/" + ws + "/agents"
	agentIn := func(id string) map[string]any {
		t.Helper()
		code, out := call(http.MethodGet, base, ``)
		if code != http.StatusOK {
			t.Fatalf("GET agents = %d %v", code, out)
		}
		for _, row := range out["agents"].([]any) {
			if a := row.(map[string]any); a["id"] == id {
				return a
			}
		}
		t.Fatalf("agent %s is not in the book: %v", id, out["agents"])
		return nil
	}

	// An agent made before pictures, as its row stands: it wears its colour's picture and has no kind yet.
	if _, err := pool.Exec(ctx, `INSERT INTO agent_accounts (id, workspace_id, name, owner_user_id, colour, face)
		VALUES ('agt_before', $1, 'Atlas', 'owner', 'sky', 'circle')`, ws); err != nil {
		t.Fatal(err)
	}
	if a := agentIn("agt_before"); a["picture"] != "sky" || a["colour"] != "sky" || a["role"] != "" {
		t.Fatalf("an agent that exists = %v, want picture sky, colour sky, no role", a)
	}

	// One of ours, and one of the owner's own making: each keeps what it was made with.
	code, ours := call(http.MethodPost, base, `{"name":"Atlas","role":"research","picture":"cobalt"}`)
	if code != http.StatusCreated || ours["role"] != "research" || ours["picture"] != "cobalt" || ours["colour"] != "cobalt" {
		t.Fatalf("create research cobalt = %d %v", code, ours)
	}
	code, own := call(http.MethodPost, base, `{"name":"Quill","role":"other","role_word":"  Grant writing ","picture":"rose"}`)
	if code != http.StatusCreated || own["role"] != "other" || own["role_word"] != "Grant writing" || own["colour"] != "rose" {
		t.Fatalf("create own kind = %d %v", code, own)
	}
	if a := agentIn(own["id"].(string)); a["role"] != "other" || a["role_word"] != "Grant writing" || a["picture"] != "rose" || a["colour"] != "rose" {
		t.Fatalf("the book reads %v, want other / Grant writing / rose", a)
	}

	// Refused in words, and nothing is made.
	for body, want := range map[string]string{
		`{"name":"X","role":"astrology"}`:                                           `"astrology" is not a kind of agent`,
		`{"name":"X","picture":"remy"}`:                                             `"remy" is not an agent picture`,
		`{"name":"X","role":"other"}`:                                               `say in a word what it does`,
		`{"name":"X","role":"sales","role_word":"Deals"}`:                           `only an agent of the owner's own kind takes its own word`,
		`{"name":"X","role":"other","role_word":"` + strings.Repeat("w", 41) + `"}`: `at most 40 characters`,
		`{"name":"X","picture":"cobalt","colour":"rose"}`:                           `the cobalt picture is cobalt`,
	} {
		code, out := call(http.MethodPost, base, body)
		if msg, _ := out["error"].(string); code != http.StatusBadRequest || !strings.Contains(msg, want) {
			t.Fatalf("create %s = %d %v, want 400 saying %q", body, code, out, want)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_accounts WHERE workspace_id = $1 AND name = 'X'`, ws).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused creates made %d agents (%v)", n, err)
	}

	// The owner changes them; a new colour alone takes that colour's picture, so the two never disagree.
	id := ours["id"].(string)
	if code, out := call(http.MethodPatch, base+"/"+id, `{"role":"other","role_word":"Patents","picture":"mint"}`); code != http.StatusOK ||
		out["role"] != "other" || out["role_word"] != "Patents" || out["picture"] != "mint" || out["colour"] != "mint" {
		t.Fatalf("patch = %d %v, want other / Patents / mint", code, out)
	}
	if code, out := call(http.MethodPatch, base+"/"+id, `{"colour":"lemon"}`); code != http.StatusOK || out["picture"] != "lemon" || out["colour"] != "lemon" {
		t.Fatalf("patch colour = %d %v, want the lemon picture", code, out)
	}
	if code, out := call(http.MethodPatch, base+"/"+id, `{"role":"travel"}`); code != http.StatusOK || out["role"] != "travel" || out["role_word"] != "" {
		t.Fatalf("patch to travel = %d %v, want travel with no word of its own", code, out)
	}
	if code, out := call(http.MethodPatch, base+"/"+id, `{"picture":"nova"}`); code != http.StatusBadRequest ||
		!strings.Contains(out["error"].(string), `"nova" is not an agent picture`) {
		t.Fatalf("patch nova = %d %v, want 400", code, out)
	}
	if a := agentIn(id); a["role"] != "travel" || a["picture"] != "lemon" {
		t.Fatalf("after a refused patch the book reads %v, want travel / lemon", a)
	}
}
