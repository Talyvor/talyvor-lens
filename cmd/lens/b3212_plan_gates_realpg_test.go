package main

import (
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

// B32.12 — through the routes the console uses: a Free workspace's fourth agent and second member are answered
// 402 naming LENS_PLAN_GATES, the free plan and the team plan; /v1/public/plan-gates serves Nicolai's values.
func TestB3212_TheConsoleIsToldWhichPlanAllowsIt(t *testing.T) {
	pool := agentRoutesDB(t)
	const ws = "ws-b3212-console"
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, economy.NewDualTokenStore(nil, pool, nil), tenant.NewStore(pool))
	r.Get("/v1/workspaces/{wsID}/plan", newWorkspacePlanHandler(pool))
	r.Get("/v1/workspaces/{wsID}/plan/seats", newSeatsCheckHandler(pool))
	r.Get("/v1/public/plan-gates", publicPlanGatesHandler)
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
	// B55.29: the refusal is in words; the setting is a field of its own.
	refused := func(code int, out map[string]any, words string) bool {
		return code == http.StatusPaymentRequired && out["plan"] == "free" && out["allows"] == "team" &&
			out["setting"] == "LENS_PLAN_GATES" && out["error"] == words
	}

	for i := 0; i < 3; i++ {
		if code, out := call("POST", "/v1/workspaces/"+ws+"/agents", `{"name":"helper"}`); code != http.StatusCreated {
			t.Fatalf("free's agent %d = %d %v, want 201", i+1, code, out)
		}
	}
	if code, out := call("POST", "/v1/workspaces/"+ws+"/agents", `{"name":"one too many"}`); !refused(code, out, "Free allows 3 agents, and all 3 are in use. Team allows 25.") || out["gate"] != "agents" {
		t.Fatalf("free's fourth agent = %d %v, want 402 in words naming the free plan and the team plan", code, out)
	}
	if code, out := call("GET", "/v1/workspaces/"+ws+"/plan", ""); code != http.StatusOK || out["plan"] != "free" || out["agents_used"] != float64(3) {
		t.Fatalf("the workspace's plan = %d %v, want free with 3 agents used", code, out)
	}
	if code, out := call("GET", "/v1/workspaces/"+ws+"/plan/seats?members=1", ""); code != http.StatusOK {
		t.Fatalf("free's first member = %d %v, want 200", code, out)
	}
	if code, out := call("GET", "/v1/workspaces/"+ws+"/plan/seats?members=2", ""); !refused(code, out, "Free allows 1 seat, and it is taken. Team allows 5.") || out["gate"] != "seats" {
		t.Fatalf("free's second member = %d %v, want 402 in words naming the free plan and the team plan", code, out)
	}
	code, out := call("GET", "/v1/public/plan-gates", "")
	team, _ := out["plans"].(map[string]any)["team"].(map[string]any)
	if code != http.StatusOK || team["agents"] != float64(25) || team["seats"] != float64(5) || team["own_provider_keys"] != "add_on" {
		t.Fatalf("/v1/public/plan-gates = %d %v, want team at 25 agents, 5 seats and own keys with the add-on", code, out)
	}
}
