package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B32.12, B61.57 — through the routes the console uses: a Studio workspace's fourth agent and second member are
// answered 402 naming LENS_PLAN_GATES, Studio and Company; /v1/public/plan-gates serves Nicolai's values, and
// /v1/public/plans both ladders with each plan's price in every currency.
func TestB3212_TheConsoleIsToldWhichPlanAllowsIt(t *testing.T) {
	pool := agentRoutesDB(t)
	const ws = "ws-b3212-console"
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, economy.NewDualTokenStore(nil, pool, nil), tenant.NewStore(pool))
	r.Get("/v1/workspaces/{wsID}/plan", newWorkspacePlanHandler(pool))
	r.Get("/v1/workspaces/{wsID}/plan/seats", newSeatsCheckHandler(pool))
	r.Get("/v1/public/plan-gates", publicPlanGatesHandler)
	r.Get("/v1/public/plans", publicPlansHandler)
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
		return code == http.StatusPaymentRequired && out["plan"] == "business_studio" && out["allows"] == "business_company" &&
			out["setting"] == "LENS_PLAN_GATES" && out["error"] == words
	}

	for i := 0; i < 3; i++ {
		if code, out := call("POST", "/v1/workspaces/"+ws+"/agents", `{"name":"helper"}`); code != http.StatusCreated {
			t.Fatalf("Studio's agent %d = %d %v, want 201", i+1, code, out)
		}
	}
	if code, out := call("POST", "/v1/workspaces/"+ws+"/agents", `{"name":"one too many"}`); !refused(code, out, "Studio allows 3 agents, and all 3 are in use. Company allows 25.") || out["gate"] != "agents" {
		t.Fatalf("Studio's fourth agent = %d %v, want 402 in words naming Studio and Company", code, out)
	}
	if code, out := call("GET", "/v1/workspaces/"+ws+"/plan", ""); code != http.StatusOK || out["plan"] != "business_studio" || out["name"] != "Studio" ||
		out["account_type"] != "business" || out["agents_used"] != float64(3) {
		t.Fatalf("the workspace's plan = %d %v, want Studio, a business's, with 3 agents used", code, out)
	}
	if code, out := call("GET", "/v1/workspaces/"+ws+"/plan/seats?members=1", ""); code != http.StatusOK {
		t.Fatalf("Studio's first member = %d %v, want 200", code, out)
	}
	if code, out := call("GET", "/v1/workspaces/"+ws+"/plan/seats?members=2", ""); !refused(code, out, "Studio allows 1 seat, and it is taken. Company allows 5.") || out["gate"] != "seats" {
		t.Fatalf("Studio's second member = %d %v, want 402 in words naming Studio and Company", code, out)
	}
	code, out := call("GET", "/v1/public/plan-gates", "")
	company, _ := out["plans"].(map[string]any)["business_company"].(map[string]any)
	if code != http.StatusOK || company["agents"] != float64(25) || company["seats"] != float64(5) || company["own_provider_keys"] != "add_on" {
		t.Fatalf("/v1/public/plan-gates = %d %v, want Company at 25 agents, 5 seats and own keys with the add-on", code, out)
	}

	// Both ladders, in order, each plan with its name, agents, people and monthly price by currency.
	code, out = call("GET", "/v1/public/plans", "")
	var got []string
	for _, kind := range []string{"personal", "business"} {
		list, _ := out[kind].([]any)
		for _, p := range list {
			p := p.(map[string]any)
			price, _ := json.Marshal(p["price_minor"])
			got = append(got, fmt.Sprintf("%s %s %v/%v %s", p["id"], p["name"], p["agents"], p["people"], price))
		}
	}
	want := []string{
		`personal_solo Solo 3/1 {"EUR":0,"GBP":0,"USD":0}`,
		`personal_crew Crew 10/1 {"EUR":399,"GBP":399,"USD":399}`,
		`personal_fleet Fleet 25/1 {"EUR":899,"GBP":799,"USD":999}`,
		`business_studio Studio 3/1 {"EUR":1000,"GBP":1000,"USD":1000}`,
		`business_company Company 25/5 {"EUR":3500,"GBP":3500,"USD":3500}`,
		`business_group Group -1/25 {"EUR":12500,"GBP":12500,"USD":12500}`,
		`business_custom Custom -1/-1 null`,
	}
	if code != http.StatusOK || strings.Join(got, "\n") != strings.Join(want, "\n") || out["stablecoin_priced_in"] != "USD" {
		t.Fatalf("/v1/public/plans = %d\n%s\nwant\n%s", code, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
