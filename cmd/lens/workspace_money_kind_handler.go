package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/economy"
)

// B59.6 — a workspace is Business or Personal, and Fiat only, Stablecoin and crypto only, or Hybrid
// (internal/economy/workspace_money_kind.go):
//
//	GET /v1/workspaces/{wsID}/account              {"account_type", "money_kind", "country", "registration_number"}
//	                                               the last two the sign-up profile (B59.40), "" when not given
//	POST /v1/workspaces/{wsID}/account/money-kind/challenge   B61.6: an account's kind of money is fixed, so
//	PUT /v1/workspaces/{wsID}/account/money-kind              each is refused 409, code and field kind_fixed, with
//	                                               KindFixedSentence and the workspace's money_kind, signed or not, at
//	                                               any balance; nothing changes
//	PUT /v1/admin/workspaces/{wsID}/money-kind     {"money_kind", "actor"}   the operator, for support, behind
//	                                               requireAdminStepUp (an operator_audit row per use): a single kind
//	                                               becomes hybrid at any time; hybrid becomes a single kind only once
//	                                               the other side holds nothing, 409 wrong_money_kind naming the
//	                                               balances to empty otherwise. Each change is in the audit trail.
//
// The type has no route: it is set when the workspace is made (/v1/provision) and never changes, and so is the kind
// for its owner. Mounted in the authed group, so {wsID} is bound to the caller's credential.

type workspaceMoneyKindBank interface {
	WorkspaceAccount(ctx context.Context, workspaceID string) (economy.WorkspaceAccount, error)
}

func mountWorkspaceMoneyKindRoutes(r chi.Router, bank workspaceMoneyKindBank) {
	r.Get("/v1/workspaces/{wsID}/account", func(w http.ResponseWriter, req *http.Request) {
		out, err := bank.WorkspaceAccount(req.Context(), chi.URLParam(req, "wsID"))
		switch {
		case errors.Is(err, economy.ErrWorkspaceNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, out)
		}
	})
	// B61.6: another kind is another account. Refused before the body is read, so no signature and no balance moves it.
	fixed := ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		acct, err := bank.WorkspaceAccount(req.Context(), chi.URLParam(req, "wsID"))
		switch {
		case errors.Is(err, economy.ErrWorkspaceNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusConflict, map[string]any{"error": economy.KindFixedSentence, "code": "kind_fixed",
				"field": "kind_fixed", "money_kind": acct.MoneyKind})
		}
	})
	r.Post("/v1/workspaces/{wsID}/account/money-kind/challenge", fixed)
	r.Put("/v1/workspaces/{wsID}/account/money-kind", fixed)
}

// operatorMoneyKindSetter is *economy.DualTokenStore.
type operatorMoneyKindSetter interface {
	OperatorSetWorkspaceMoneyKind(ctx context.Context, workspaceID, kind, operator string) (economy.WorkspaceAccount, error)
}

// newOperatorMoneyKindHandler — PUT /v1/admin/workspaces/{wsID}/money-kind {"money_kind", "actor"}: the operator sets a
// workspace's kind of money for support (B61.6). Mounted behind requireAdminStepUp, so no workspace key, session or
// role reaches it.
func newOperatorMoneyKindHandler(bank operatorMoneyKindSetter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			MoneyKind string `json:"money_kind"`
			Actor     string `json:"actor"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil || in.MoneyKind == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"money_kind": "fiat"|"stablecoin"|"hybrid", "actor"}`)
			return
		}
		actor, ok := contractActor(req, in.Actor)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "actor in the body is not the operator "+moderatorOperatorHeader+" names")
			return
		}
		out, err := bank.OperatorSetWorkspaceMoneyKind(req.Context(), chi.URLParam(req, "wsID"), in.MoneyKind, actor)
		switch {
		case writeMoneyKindRefusal(w, err):
		case errors.Is(err, economy.ErrWorkspaceNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrMoneyKind):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, out)
		}
	})
}

// writeMoneyKindRefusal answers a *economy.MoneyKindRefusal as 409 with code wrong_money_kind, the field refused, the
// workspace's kind and any balances still to empty, and is false for any other error.
func writeMoneyKindRefusal(w http.ResponseWriter, err error) bool {
	var r *economy.MoneyKindRefusal
	if !errors.As(err, &r) {
		return false
	}
	out := map[string]any{"error": api.Refusal(r.Error()), "code": "wrong_money_kind", "field": r.Field, "money_kind": r.MoneyKind}
	if len(r.Balances) > 0 {
		out["balances"] = r.Balances
	}
	writeJSONOK(w, http.StatusConflict, out)
	return true
}
