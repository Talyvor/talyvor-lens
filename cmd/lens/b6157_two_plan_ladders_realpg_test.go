package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/fees"
	"github.com/talyvor/lens/internal/plans"
	"github.com/talyvor/lens/internal/workspace"
)

// B61.57 — two ladders, by the account's type, on a migrated schema: a new personal account is on Solo with 3 agents
// and a new business on Studio; Solo's 4th agent is refused naming Crew; Solo is not refused live money by its plan;
// the owner moves along their own ladder only, never to the other type's or to Custom, with an audit row and nothing
// charged; a paid plan is refused while the Stripe key is live; a move down is refused while the account has more
// agents or people than the plan allows, saying how many; and each plan is quoted its own exchange fee.
func TestB6157_TwoLaddersByTheAccountsType(t *testing.T) {
	pool := syntheticDB(t)
	ctx := context.Background()
	wsm := workspace.New(pool)
	for id, kind := range map[string]string{"ws-b6157-person": "personal", "ws-b6157-company": "business"} {
		if err := wsm.RegisterWorkspace(ctx, workspace.Workspace{ID: id, Name: id, Active: true}, workspace.WithAccount(kind, "hybrid")); err != nil {
			t.Fatal(err)
		}
	}
	person, err := plans.Of(ctx, pool, "ws-b6157-person")
	if err != nil || person.Plan != plans.PersonalSolo || person.Agents != 3 || person.Seats != 1 || !person.LiveMoney {
		t.Fatalf("a new personal account = %+v, %v; want Solo, 3 agents, 1 person, live money allowed", person, err)
	}
	company, err := plans.Of(ctx, pool, "ws-b6157-company")
	if err != nil || company.Plan != plans.BusinessStudio || company.Agents != 3 || !company.LiveMoney {
		t.Fatalf("a new business account = %+v, %v; want Studio, 3 agents, live money allowed", company, err)
	}

	dual := economy.NewDualTokenStore(nil, pool, nil)
	for i := 1; i <= 3; i++ {
		if _, err := dual.CreateAgent(ctx, "ws-b6157-person", fmt.Sprintf("agent %d", i), "owner"); err != nil {
			t.Fatalf("Solo's agent %d: %v", i, err)
		}
	}
	_, err = dual.CreateAgent(ctx, "ws-b6157-person", "agent 4", "owner")
	var ref *plans.Refusal
	if !errors.As(err, &ref) || ref.Plan != plans.PersonalSolo || ref.Allows != plans.PersonalCrew ||
		err.Error() != "Solo allows 3 agents, and all 3 are in use. Crew allows 10." {
		t.Fatalf("Solo's 4th agent = %v, want refused naming Crew's 10", err)
	}

	r := chi.NewRouter()
	r.Put("/v1/workspaces/{wsID}/plan", newWorkspacePlanChangeHandler(pool, false))
	r.Put("/live/{wsID}/plan", newWorkspacePlanChangeHandler(pool, true))
	change := func(path, ws, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, strings.Replace(path, "{ws}", ws, 1), strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	const route = "/v1/workspaces/{ws}/plan"
	for name, c := range map[string]struct {
		ws, body string
		code     int
		words    string
	}{
		"a business plan for a person":  {"ws-b6157-person", `{"plan":"business_company"}`, http.StatusConflict, "A personal account chooses Solo, Crew or Fleet."},
		"a personal plan for a company": {"ws-b6157-company", `{"plan":"personal_crew"}`, http.StatusConflict, "A business account chooses Studio, Company or Group."},
		"Custom":                        {"ws-b6157-company", `{"plan":"business_custom"}`, http.StatusConflict, "Custom is set by a contract with Talyvor."},
		"the old Team":                  {"ws-b6157-company", `{"plan":"team"}`, http.StatusBadRequest, `There is no plan called "team".`},
		"the plan it is on":             {"ws-b6157-person", `{"plan":"personal_solo"}`, http.StatusConflict, "This account is already on Solo."},
	} {
		if code, out := change(route, c.ws, c.body); code != c.code || out["error"] != c.words {
			t.Errorf("%s = %d %v, want %d %q", name, code, out, c.code, c.words)
		}
	}
	if code, out := change("/live/{ws}/plan", "ws-b6157-person", `{"plan":"personal_crew"}`); code != http.StatusConflict ||
		out["error"] != "Crew cannot be chosen yet: paid plans open once Talyvor can take their price. This account stays on Solo." {
		t.Fatalf("a paid plan under a live Stripe key = %d %v, want refused in words", code, out)
	}

	if code, out := change(route, "ws-b6157-person", `{"plan":"personal_crew"}`); code != http.StatusOK || out["from"] != plans.PersonalSolo || out["plan"] != plans.PersonalCrew {
		t.Fatalf("Solo to Crew = %d %v, want 200", code, out)
	}
	var audit string
	if err := pool.QueryRow(ctx, `SELECT body FROM audit_chain WHERE workspace_id = 'ws-b6157-person' AND kind = 'workspace.plan'`).Scan(&audit); err != nil ||
		!strings.Contains(audit, `"from":"personal_solo"`) || !strings.Contains(audit, `"to":"personal_crew"`) || !strings.Contains(audit, `"by":`) {
		t.Fatalf("the plan change's audit row = %q, %v; want from Solo to Crew and who", audit, err)
	}
	var charged int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM lxc_ledger WHERE workspace_id = 'ws-b6157-person'`).Scan(&charged); err != nil || charged != 0 {
		t.Fatalf("the plan change wrote %d ledger rows (%v), want none: nothing is charged in Sandbox", charged, err)
	}
	for i := 4; i <= 5; i++ {
		if _, err := dual.CreateAgent(ctx, "ws-b6157-person", fmt.Sprintf("agent %d", i), "owner"); err != nil {
			t.Fatalf("Crew's agent %d: %v", i, err)
		}
	}
	if code, out := change(route, "ws-b6157-person", `{"plan":"personal_solo"}`); code != http.StatusConflict ||
		out["error"] != "Solo allows 3 agents, and this account has 5. Archive 2 agents first." || out["plan"] != plans.PersonalCrew {
		t.Fatalf("Crew down to Solo with 5 agents = %d %v, want refused saying to archive 2", code, out)
	}
	if code, out := change(route, "ws-b6157-company", `{"plan":"business_group"}`); code != http.StatusOK {
		t.Fatalf("Studio to Group = %d %v, want 200", code, out)
	}
	if code, out := change(route, "ws-b6157-company", `{"plan":"business_company","members":7}`); code != http.StatusConflict ||
		out["error"] != "Company allows 5 people, and this account has 7. Remove 2 people first." {
		t.Fatalf("Group down to Company with 7 people = %d %v, want refused saying to remove 2", code, out)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET plan = 'business_group' WHERE id = 'ws-b6157-person'`); err == nil {
		t.Fatal("the database took a business plan for a personal account; want its CHECK to refuse it")
	}

	// The exchange fee quoted to each plan (until B61.58's allowances): Solo 1%, Crew 0.5%, Fleet none; Studio,
	// Company and Group 0.6%.
	for plan, want := range map[string]int64{plans.PersonalSolo: 100, plans.PersonalCrew: 50, plans.PersonalFleet: 0,
		plans.BusinessStudio: 60, plans.BusinessCompany: 60, plans.BusinessGroup: 60} {
		ws, kind := "ws-b6157-fx-"+plan, plans.AccountBusiness
		if plans.Step(plan) >= 0 && strings.HasPrefix(plan, "personal_") {
			kind = plans.AccountPersonal
		}
		if err := wsm.RegisterWorkspace(ctx, workspace.Workspace{ID: ws, Name: ws, Active: true}, workspace.WithAccount(kind, "fiat")); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE workspaces SET plan = $2 WHERE id = $1`, ws, plan); err != nil {
			t.Fatal(err)
		}
		if bps, on, err := billing.FXMarginBPS(ctx, pool, ws, fees.Defaults()); err != nil || bps != want || on != plan {
			t.Errorf("%s's exchange fee = %d bps on %q (%v), want %d", plan, bps, on, err, want)
		}
	}
}

// B61.57 — after the migration every existing workspace is on its 5 Oct plan's successor by its type: free the first
// step, team (and byok, plus, pro and max) the second, business the third, enterprise Custom and a personal one Fleet;
// a business on a contract keeps Custom while it lasts and returns to its own plan after. A paying subscription's plan
// is never stored: when the subscription ends the account falls to its first step, keeping no paid plan unpaid.
func TestB6157_TheMigrationMovesEveryOldPlan(t *testing.T) {
	pool := syntheticDB(t)
	ctx := context.Background()
	sql, err := os.ReadFile("../../migrations/0398_two_plan_ladders.sql")
	if err != nil {
		t.Fatal(err)
	}
	type was struct{ kind, subscription, synthetic, want, stored string }
	cases := map[string]was{
		"b-free": {"business", "", "", plans.BusinessStudio, ""}, "p-free": {"personal", "", "", plans.PersonalSolo, ""},
		"b-plus": {"business", "plus", "", plans.BusinessCompany, ""}, "p-max": {"personal", "max", "", plans.PersonalCrew, ""},
		"b-team": {"business", "team", "", plans.BusinessCompany, ""}, "p-team": {"personal", "team", "", plans.PersonalCrew, ""},
		"b-byok": {"business", "byok", "", plans.BusinessCompany, ""}, "p-byok": {"personal", "byok", "", plans.PersonalCrew, ""},
		"b-business": {"business", "business", "", plans.BusinessGroup, ""}, "p-business": {"personal", "business", "", plans.PersonalFleet, ""},
		"b-syn-team":       {"business", "", "team", plans.BusinessCompany, plans.BusinessCompany},
		"b-syn-enterprise": {"business", "", "enterprise", plans.BusinessCustom, plans.BusinessCustom},
		"b-syn-paid":       {"business", "business", "team", plans.BusinessGroup, ""},
		"b-contract":       {"business", "team", "", plans.BusinessCustom, ""},
		"p-contract":       {"personal", "", "", plans.PersonalFleet, plans.PersonalFleet},
	}
	for id, c := range cases {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type, synthetic, synthetic_plan)
			VALUES ($1, $1, $1, $2, $3 <> '', NULLIF($3, ''))`, id, c.kind, c.synthetic); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if c.subscription != "" {
			if _, err := pool.Exec(ctx, `INSERT INTO subscriptions (workspace_id, stripe_subscription_id, stripe_customer_id,
				price_id, status, livemode, last_event_at, plan, byok) VALUES ($1, 'sub_' || $1, 'cus_' || $1, 'price_' || $1,
				'active', false, NOW(), $2, $2 = 'byok')`, id, c.subscription); err != nil {
				t.Fatalf("%s: %v", id, err)
			}
		}
	}
	// The 5 Oct contract named enterprise: put it back as the migration finds it, then migrate again.
	if _, err := pool.Exec(ctx, `ALTER TABLE workspace_contracts DROP CONSTRAINT workspace_contracts_plan_check`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"b-contract", "p-contract"} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_contracts (workspace_id, plan, set_by) VALUES ($1, 'enterprise', 'op')`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("the migration again: %v", err)
	}
	for id, c := range cases {
		var stored *string
		if err := pool.QueryRow(ctx, `SELECT plan FROM workspaces WHERE id = $1`, id).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		got := ""
		if stored != nil {
			got = *stored
		}
		if got != c.stored {
			t.Errorf("%s stores plan %q, want %q", id, got, c.stored)
		}
		if p, err := plans.Of(ctx, pool, id); err != nil || p.Plan != c.want {
			t.Errorf("%s (%s on %q/%q) is on %q (%v), want %s", id, c.kind, c.subscription, c.synthetic, p.Plan, err, c.want)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE subscriptions SET status = 'canceled' WHERE workspace_id IN ('b-business', 'p-team')`); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"b-business": plans.BusinessStudio, "p-team": plans.PersonalSolo} {
		if p, err := plans.Of(ctx, pool, id); err != nil || p.Plan != want {
			t.Errorf("%s after its subscription ends is on %q (%v), want %s, its first step", id, p.Plan, err, want)
		}
	}
	var contract string
	if err := pool.QueryRow(ctx, `SELECT plan FROM workspace_contracts WHERE workspace_id = 'b-contract'`).Scan(&contract); err != nil || contract != plans.BusinessCustom {
		t.Fatalf("the business's contract names %q (%v), want %s", contract, err, plans.BusinessCustom)
	}
	if err := billing.EndContract(ctx, pool, "b-contract", "op"); err != nil {
		t.Fatal(err)
	}
	if p, err := plans.Of(ctx, pool, "b-contract"); err != nil || p.Plan != plans.BusinessCompany {
		t.Fatalf("the business after its contract ends is on %+v (%v), want Company, its own", p, err)
	}
}
