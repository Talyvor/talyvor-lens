package main

import (
	"context"
	"encoding/json"
	"errors"
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

// B28.308 — a limit raised until a time reverts by itself: after expiry a hold at the boosted size writes nothing.
// The boost is set and read through the routes the console uses; the holds are the agent's key's, judged at the
// time each names, so the test needs no clock to pass.
func TestB28308_AfterABoostEndsAHoldAtTheBoostedSizeWritesNothing(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, key = "ws-rule-boost", "key-rule-boost"
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "alice", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreditLXC(ctx, ws, 100_000_000, "top-up", nil); err != nil {
		t.Fatal(err)
	}
	a, err := store.CreateAgent(ctx, ws, "launch-day", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, ws, a.ID, key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, a.ID, 80_000_000); err != nil {
		t.Fatal(err)
	}
	base := "/v1/workspaces/" + ws + "/agents/" + a.ID
	if code, body := call(http.MethodPut, base+"/rules", `{"daily_limit_ulxc":10000000}`); code != http.StatusOK {
		t.Fatalf("PUT rules = %d %s", code, body)
	}

	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	code, body := call(http.MethodPost, base+"/rules/boosts",
		`{"rule":"daily_limit_ulxc","value":50000000,"until":"`+until.Format(time.RFC3339)+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST rules/boosts = %d %s", code, body)
	}
	var set economy.AgentRuleBoost
	if err := json.Unmarshal([]byte(body), &set); err != nil || set.RaisedFrom != 10_000_000 || set.Value != 50_000_000 ||
		!set.Until.Equal(until) || set.CreatedBy != "jwt:user:alice" {
		t.Fatalf("POST rules/boosts answered %s, want the daily limit raised from 10 to 50 LXC until %s by jwt:user:alice", body, until)
	}
	code, body = call(http.MethodGet, base+"/rules/boosts", "")
	var listed struct{ Boosts []economy.AgentRuleBoost }
	if err := json.Unmarshal([]byte(body), &listed); code != http.StatusOK || err != nil || len(listed.Boosts) != 1 ||
		listed.Boosts[0].Rule != "daily_limit_ulxc" || !listed.Boosts[0].Until.Equal(until) {
		t.Fatalf("GET rules/boosts = %d %s, want the one boost", code, body)
	}

	// The ledger: the agent's hold postings, and the reservation each hold opens.
	hold := func(ref string, at time.Time) error {
		return store.ReserveLXCForAgent(economy.WithAgentRequest(ctx, economy.AgentRequest{Model: "gpt-4o", Provider: "openai", At: at}),
			key, ws, ref, 30_000_000, economy.AgentDebitMeta{})
	}
	written := func(ref string) (postings, reservations int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_postings WHERE account = $1 AND kind = 'hold' AND ref = $2),
			(SELECT count(*) FROM lxc_reservations WHERE reservation_id = $2)`, "agent:"+a.ID, ref).Scan(&postings, &reservations); err != nil {
			t.Fatal(err)
		}
		return postings, reservations
	}

	// While the boost lasts, a 30 LXC hold — three times the daily limit — goes through and is on the ledger.
	if err := hold("res-boosted", until.Add(-time.Minute)); err != nil {
		t.Fatalf("a 30 LXC hold while the daily limit is boosted to 50 LXC: %v", err)
	}
	if p, res := written("res-boosted"); p != 1 || res != 1 {
		t.Fatalf("the hold under the boost wrote %d hold postings and %d reservations, want 1 and 1", p, res)
	}
	// From until on the daily limit is 10 LXC again, with nothing run to revert it: the same hold writes nothing.
	err = hold("res-expired", until)
	if !errors.Is(err, economy.ErrAgentRule) || !strings.Contains(err.Error(), "daily limit of $1.00 (10 LXC)") {
		t.Fatalf("a 30 LXC hold once the boost has ended = %v, want the 10 LXC daily limit's refusal", err)
	}
	if p, res := written("res-expired"); p != 0 || res != 0 {
		t.Fatalf("the hold after the boost ended wrote %d hold postings and %d reservations, want nothing", p, res)
	}

	// A boost raises the limit it was set over: lowered by the rules while it lasts, the limit is the rules' at once.
	if code, body := call(http.MethodPut, base+"/rules", `{"daily_limit_ulxc":5000000}`); code != http.StatusOK {
		t.Fatalf("PUT rules lowering the daily limit = %d %s", code, body)
	}
	if err := hold("res-lowered", until.Add(-time.Minute)); !errors.Is(err, economy.ErrAgentRule) || !strings.Contains(err.Error(), "daily limit of $0.50 (5 LXC)") {
		t.Fatalf("a 30 LXC hold once the boosted limit was lowered to 5 LXC = %v, want the 5 LXC daily limit's refusal", err)
	}
	if p, res := written("res-lowered"); p != 0 || res != 0 {
		t.Fatalf("the hold after the limit was lowered wrote %d hold postings and %d reservations, want nothing", p, res)
	}

	// Ending a boost early takes it off the list.
	if code, body := call(http.MethodDelete, base+"/rules/boosts/daily_limit_ulxc", ""); code != http.StatusNoContent {
		t.Fatalf("DELETE rules/boosts/daily_limit_ulxc = %d %s", code, body)
	}
	if code, body := call(http.MethodGet, base+"/rules/boosts", ""); code != http.StatusOK || !strings.Contains(body, `"boosts":[]`) {
		t.Fatalf("GET rules/boosts after ending it = %d %s, want none", code, body)
	}

	later := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, base + "/rules/boosts", `{"rule":"daily_limit_ulxc","value":5000000,"until":"` + later + `"}`, http.StatusBadRequest},   // not above the limit
		{http.MethodPost, base + "/rules/boosts", `{"rule":"weekly_limit_ulxc","value":10000000,"until":"` + later + `"}`, http.StatusBadRequest}, // no weekly limit to raise
		{http.MethodPost, base + "/rules/boosts", `{"rule":"daily_limit_ulxc","value":50000000,"until":"2020-01-01T00:00:00Z"}`, http.StatusBadRequest},
		{http.MethodPost, base + "/rules/boosts", `{"rule":"allowed_models","value":1,"until":"` + later + `"}`, http.StatusBadRequest},
		{http.MethodDelete, base + "/rules/boosts/daily_limit_ulxc", "", http.StatusNotFound},
		{http.MethodGet, "/v1/workspaces/" + ws + "/agents/agt_nobody/rules/boosts", "", http.StatusNotFound},
	} {
		if code, got := call(c.method, c.path, c.body); code != c.want {
			t.Errorf("%s %s %s = %d %s, want %d", c.method, c.path, c.body, code, got, c.want)
		}
	}
}
