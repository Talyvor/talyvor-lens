package main

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/plans"
)

// B32.12 — what each plan unlocks (internal/plans, LENS_PLAN_GATES).

// publicPlanGatesHandler is GET /v1/public/plan-gates: every plan's gates, as lens.env sets them, for /pricing.
// -1 is unlimited.
func publicPlanGatesHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSONOK(w, http.StatusOK, map[string]any{"order": plans.Order, "plans": plans.Current()})
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

// GET /v1/workspaces/{wsID}/plan → the workspace's plan, the gates it has, and the agents it has now.
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
		writeJSONOK(w, http.StatusOK, map[string]any{"plan": plan.Plan, "gated_as": plan.GatedAs, "byok_add_on": plan.BYOKAddOn,
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
