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

// B57.10 — through the routes the apps call, on a migrated schema: an agent is created with the face its owner picks,
// or the one Lens picks; GET /agents, the forecast and safe to spend carry it; the owner changes it with PATCH; a
// value off the list is refused with a sentence; and a credential that is not the owner's changes nothing.
func TestAgentRoutes_EachAgentCarriesAColourAndAFace(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-faces"
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	proxyKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-proxy", Scopes: []string{"proxy"}}
	call := func(who *auth.AuthContext, method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	base := "/v1/workspaces/" + ws + "/agents"

	code, picked := call(owner, http.MethodPost, base, `{"name":"Research"}`)
	if code != http.StatusCreated || picked["colour"] == "" || picked["face"] == "" {
		t.Fatalf("create = %d %v, want a colour and a face Lens picked", code, picked)
	}
	code, chosen := call(owner, http.MethodPost, base, `{"name":"Sales","colour":"lime","face":"flower"}`)
	if code != http.StatusCreated || chosen["colour"] != "lime" || chosen["face"] != "flower" {
		t.Fatalf("create with a face = %d %v, want lime flower", code, chosen)
	}
	code, out := call(owner, http.MethodPost, base, `{"name":"Ops","colour":"teal"}`)
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), `"teal" is not an agent colour: pick cobalt, orchid, sky, mint, lemon, rose or lime`) {
		t.Fatalf("create with teal = %d %v, want 400 naming the colours", code, out)
	}

	id := chosen["id"].(string)
	if code, out := call(owner, http.MethodPatch, base+"/"+id, `{"face":"circle"}`); code != http.StatusOK || out["colour"] != "lime" || out["face"] != "circle" {
		t.Fatalf("patch face = %d %v, want lime circle", code, out)
	}
	if code, out := call(owner, http.MethodPatch, base+"/"+id, `{"face":"star"}`); code != http.StatusBadRequest ||
		!strings.Contains(out["error"].(string), `"star" is not an agent face: pick circle, squircle, hexagon or flower`) {
		t.Fatalf("patch star = %d %v, want 400 naming the faces", code, out)
	}
	if code, out := call(proxyKey, http.MethodPatch, base+"/"+id, `{"colour":"rose"}`); code != http.StatusForbidden {
		t.Fatalf("a proxy key's patch = %d %v, want 403", code, out)
	}
	var colour, face string
	if err := pool.QueryRow(ctx, `SELECT colour, face FROM agent_accounts WHERE id = $1`, id).Scan(&colour, &face); err != nil {
		t.Fatal(err)
	}
	if colour != "lime" || face != "circle" {
		t.Fatalf("stored %s %s, want lime circle: neither refused patch may change it", colour, face)
	}

	// The book, the forecast and safe to spend all carry it.
	for _, path := range []string{base, base + "/forecast", base + "/safe-to-spend"} {
		code, out := call(owner, http.MethodGet, path, ``)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d %v", path, code, out)
		}
		found := false
		for _, row := range out["agents"].([]any) {
			a := row.(map[string]any)
			if a["id"] == id || a["agent_id"] == id {
				found = a["colour"] == "lime" && a["face"] == "circle"
			}
		}
		if !found {
			t.Fatalf("GET %s does not carry lime circle for %s: %v", path, id, out["agents"])
		}
	}
}
