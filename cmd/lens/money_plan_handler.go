package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
)

// B53.15 — Coach's month plan (economy/spend_plan.go).
//
//	GET /v1/money/plan        — the month plan: money now, the upcoming payments with their dates and whether each is
//	                            covered, budgets left, what is safe to spend each day until the next income, and each
//	                            off-plan finding with its numbers and the Coach's sentence; with no income set, Lens's
//	                            proposal from three months of money in, for the owner to confirm
//	PUT /v1/money/plan/income {payday, amount_minor, currency, payer} — set or confirm the expected income; answers the plan
//
// The workspace is the key's; only its owner or an admin may use these (spendingOwner). Mounted in the authed group.

type moneyPlanBank interface {
	SpendPlan(ctx context.Context, workspaceID string, now time.Time) (economy.SpendPlan, error)
	SetSpendIncome(ctx context.Context, workspaceID string, in economy.SpendIncome) error
}

func mountMoneyPlanRoutes(r chi.Router, bank moneyPlanBank) {
	plan := func(w http.ResponseWriter, req *http.Request, ws string) {
		out, err := bank.SpendPlan(req.Context(), ws, time.Now().UTC())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, out)
	}
	r.Get("/v1/money/plan", func(w http.ResponseWriter, req *http.Request) {
		if ws, _, ok := spendingOwner(w, req); ok {
			plan(w, req, ws)
		}
	})
	r.Put("/v1/money/plan/income", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		var in economy.SpendIncome
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"payday", "amount_minor", "currency", "payer"}`)
			return
		}
		switch err := bank.SetSpendIncome(req.Context(), ws, in); {
		case errors.Is(err, economy.ErrSpending):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			plan(w, req, ws)
		}
	})
}
