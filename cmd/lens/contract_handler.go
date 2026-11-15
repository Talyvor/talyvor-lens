package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/billing"
)

// contract_handler.go — B32.10: the operator's contracts. Custom (business_custom, B61.57) is contracted and invoiced
// by the operator, never chosen in the app; recording it here makes billing.PlanOf answer business_custom for a
// business, with the contract's own fees.

// contractActor is the operator who acts: the one X-Talyvor-Operator names, or the body's actor. A body naming
// someone else than the header is refused, as the operator audit trail's own record route refuses it.
func contractActor(req *http.Request, body string) (string, bool) {
	actor := strings.TrimSpace(body)
	if named := strings.TrimSpace(req.Header.Get(moderatorOperatorHeader)); named != "" {
		if actor != "" && actor != named {
			return "", false
		}
		actor = named
	}
	return actor, true
}

func writeContractErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, billing.ErrInvalidContract):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, billing.ErrNoContract):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}

// newContractGetHandler — GET /v1/admin/workspaces/{wsID}/contract: the workspace's plan as PlanOf answers it,
// and its contract (null when it has none).
func newContractGetHandler(pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		plan, err := billing.PlanOf(req.Context(), pool, wsID)
		if err != nil {
			writeContractErr(w, err)
			return
		}
		c, err := billing.ContractOf(req.Context(), pool, wsID)
		if err != nil {
			writeContractErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"plan": plan, "contract": c})
	})
}

// newContractPutHandler — PUT /v1/admin/workspaces/{wsID}/contract {"plan":"business_custom", "platform_fee_bps",
// "fx_margin_bps", "reference", "actor"}: puts a business on a Custom contract, or replaces its terms.
func newContractPutHandler(pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Plan           string `json:"plan"`
			PlatformFeeBPS *int64 `json:"platform_fee_bps"`
			FXMarginBPS    *int64 `json:"fx_margin_bps"`
			Reference      string `json:"reference"`
			Actor          string `json:"actor"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, 8<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		actor, ok := contractActor(req, in.Actor)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "actor in the body is not the operator "+moderatorOperatorHeader+" names")
			return
		}
		c, err := billing.SetContract(req.Context(), pool, billing.Contract{WorkspaceID: chi.URLParam(req, "wsID"),
			Plan: in.Plan, PlatformFeeBPS: in.PlatformFeeBPS, FXMarginBPS: in.FXMarginBPS, Reference: in.Reference}, actor)
		if err != nil {
			writeContractErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"plan": c.Plan, "contract": c})
	})
}

// newContractDeleteHandler — DELETE /v1/admin/workspaces/{wsID}/contract (?actor= when no X-Talyvor-Operator):
// ends the contract; the workspace is back on the plan its subscription bills, or free.
func newContractDeleteHandler(pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		actor, ok := contractActor(req, req.URL.Query().Get("actor"))
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "actor is not the operator "+moderatorOperatorHeader+" names")
			return
		}
		if err := billing.EndContract(req.Context(), pool, wsID, actor); err != nil {
			writeContractErr(w, err)
			return
		}
		plan, err := billing.PlanOf(req.Context(), pool, wsID)
		if err != nil {
			writeContractErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"plan": plan, "contract": nil})
	})
}
