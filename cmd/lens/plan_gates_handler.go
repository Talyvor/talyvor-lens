package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/plans"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B32.12 — what each plan unlocks (internal/plans, LENS_PLAN_GATES).

// publicPlanGatesHandler is GET /v1/public/plan-gates: every plan's gates, as lens.env sets them, for /pricing,
// and which of them Lens enforces today (B56.8). -1 is unlimited.
func publicPlanGatesHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSONOK(w, http.StatusOK, map[string]any{"order": plans.Order, "plans": plans.Current(), "built": plans.Built})
}

// publicPlan is one plan as GET /v1/public/plans reads it.
type publicPlan struct {
	ID         string           `json:"id"`
	Name       string           `json:"name"`
	PriceMinor map[string]int64 `json:"price_minor"` // a month, by currency; null for Custom, which a contract prices
	ByContract bool             `json:"by_contract,omitempty"`
	Agents     int64            `json:"agents"` // -1 is unlimited
	People     int64            `json:"people"` // -1 is unlimited
	Gates      plans.Gates      `json:"gates"`
}

// publicPlansHandler is GET /v1/public/plans (B61.57), beside /v1/public/fees: both ladders, each plan's monthly price
// in every currency and what it allows, as lens.env sets them, so no app holds a figure. A stablecoin account pays the
// USD figure.
func publicPlansHandler(w http.ResponseWriter, _ *http.Request) {
	gates, prices := plans.Current(), plans.CurrentPrices()
	ladder := func(ids []string) []publicPlan {
		out := make([]publicPlan, 0, len(ids))
		for _, id := range ids {
			g := gates[id]
			out = append(out, publicPlan{ID: id, Name: plans.Name(id), PriceMinor: prices[id], ByContract: id == plans.BusinessCustom,
				Agents: g.Agents, People: g.Seats, Gates: g})
		}
		return out
	}
	writeJSONOK(w, http.StatusOK, map[string]any{"currencies": plans.Currencies, "stablecoin_priced_in": "USD",
		"personal": ladder(plans.PersonalLadder), "business": ladder(plans.BusinessLadder)})
}

// PUT /v1/workspaces/{wsID}/plan {"plan":"personal_crew","members":2} → the owner moves the account to another plan of
// its own ladder (B61.57): 200 with the plan it was on and is on now, the change in the audit trail in the same
// transaction; 409 saying why in words when the plan may not be chosen (plans.Change); 400 for a plan that does not
// exist. members is how many people the account has, where the caller keeps them (as /plan/seats takes it). Nothing is
// charged: while paidRefused (Lens's Stripe key is live) a paid plan is refused.
func newWorkspacePlanChangeHandler(pool *pgxpool.Pool, paidRefused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		who, ok := storedanswers.OwnerOrAdmin(req.Context())
		if !ok {
			writeJSONErr(w, http.StatusForbidden, "only the account's owner may change its plan")
			return
		}
		var in struct {
			Plan    string `json:"plan"`
			Members *int64 `json:"members"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "the body is {\"plan\": a plan's id}: "+err.Error())
			return
		}
		if in.Members != nil && *in.Members < 0 {
			writeJSONErr(w, http.StatusBadRequest, "members must be the number of people the account has, ≥ 0")
			return
		}
		wsID := chi.URLParam(req, "wsID")
		tx, err := pool.Begin(req.Context())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer func() { _ = tx.Rollback(req.Context()) }()
		from, err := plans.Change(req.Context(), tx, wsID, in.Plan, in.Members, paidRefused)
		var refusal *plans.ChangeRefusal
		switch {
		case errors.As(err, &refusal) && refusal.Unknown:
			writeJSONOK(w, http.StatusBadRequest, map[string]any{"error": refusal.Detail, "plan": refusal.Plan})
			return
		case errors.As(err, &refusal):
			writeJSONOK(w, http.StatusConflict, map[string]any{"error": refusal.Detail, "plan": refusal.Plan, "to": refusal.To})
			return
		case errors.Is(err, plans.ErrNoWorkspace):
			writeJSONErr(w, http.StatusNotFound, "no such account")
			return
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := economy.AppendAuditTx(req.Context(), tx, wsID, "workspace:"+wsID, "workspace.plan",
			map[string]any{"from": from, "to": in.Plan, "by": who}); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := tx.Commit(req.Context()); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"from": from, "plan": in.Plan, "name": plans.Name(in.Plan)})
	}
}

// writePlanRefusal answers a plan's refusal with 402 and the refusal itself — in words in "error", with the setting
// (where it names one), the plan and the plan that would allow it beside — or reports false when err is none.
func writePlanRefusal(w http.ResponseWriter, err error) bool {
	var r *plans.Refusal
	if !errors.As(err, &r) {
		return false
	}
	out := map[string]any{"error": r.Detail, "plan": r.Plan, "gate": r.Gate, "limit": r.Limit, "allows": r.Allows}
	if r.Setting != "" {
		out["setting"] = r.Setting
	}
	writeJSONOK(w, http.StatusPaymentRequired, out)
	return true
}

// GET /v1/workspaces/{wsID}/plan → the workspace's plan, its name and price, the ladder it chooses from, the gates it
// has, and the agents it has now.
func newWorkspacePlanHandler(db plans.Querier) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		plan, err := plans.Of(req.Context(), db, wsID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		var agents int64
		if err := db.QueryRow(req.Context(), `SELECT count(*) FROM agent_accounts WHERE workspace_id = $1 AND archived_at IS NULL AND kind = 'agent'`,
			wsID).Scan(&agents); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"plan": plan.Plan, "name": plans.Name(plan.Plan), "account_type": plan.AccountType,
			"ladder": plans.Ladder(plan.AccountType), "on_contract": plan.OnContract, "price_minor": plans.CurrentPrices()[plan.Plan],
			"gated_as": plan.GatedAs, "byok_add_on": plan.BYOKAddOn,
			"gates": plan.Gates, "own_provider_keys_allowed": plan.OwnKeysAllowed(), "agents_used": agents})
	}
}

// GET /v1/workspaces/{wsID}/plan/seats?members=N → 200 when the plan's seats take N members, else 402 saying so
// in words, with LENS_PLAN_GATES, the plan and the plan that would in fields of their own. The members of a workspace are kept where they are added,
// not in Lens: that feature asks here, with the count it would have, before it adds one.
func newSeatsCheckHandler(db plans.Querier) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		members, err := strconv.ParseInt(req.URL.Query().Get("members"), 10, 64)
		if err != nil || members < 0 {
			writeJSONErr(w, http.StatusBadRequest, "members must be the number of members the workspace would have, ≥ 0")
			return
		}
		plan, err := plans.Of(req.Context(), db, chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if writePlanRefusal(w, plan.CheckSeats(members)) {
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"plan": plan.Plan, "seats": plan.Seats, "members": members})
	}
}
