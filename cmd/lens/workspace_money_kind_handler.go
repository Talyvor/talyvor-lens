package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B59.6 — a workspace is Business or Personal, and Fiat only, Stablecoin and crypto only, or Hybrid
// (internal/economy/workspace_money_kind.go):
//
//	GET /v1/workspaces/{wsID}/account              {"account_type", "money_kind", "country", "registration_number"}
//	                                               the last two the sign-up profile (B59.40), "" when not given
//	PUT /v1/workspaces/{wsID}/account/money-kind   {"money_kind"}   a single kind becomes hybrid at any time; hybrid
//	                                               becomes a single kind only once the other side holds nothing: 409
//	                                               wrong_money_kind, naming the balances to empty, otherwise
//
// The type has no route: it is set when the workspace is made (/v1/provision) and never changes. Mounted in the authed
// group, so {wsID} is bound to the caller's credential; changing the kind takes the workspace's owner or an admin.

type workspaceMoneyKindBank interface {
	WorkspaceAccount(ctx context.Context, workspaceID string) (economy.WorkspaceAccount, error)
	SetWorkspaceMoneyKind(ctx context.Context, workspaceID, kind, by string) (economy.WorkspaceAccount, error)
}

func mountWorkspaceMoneyKindRoutes(r chi.Router, bank workspaceMoneyKindBank) {
	write := func(w http.ResponseWriter, out economy.WorkspaceAccount, err error) {
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
	}
	r.Get("/v1/workspaces/{wsID}/account", func(w http.ResponseWriter, req *http.Request) {
		out, err := bank.WorkspaceAccount(req.Context(), chi.URLParam(req, "wsID"))
		write(w, out, err)
	})
	r.Put("/v1/workspaces/{wsID}/account/money-kind", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			MoneyKind string `json:"money_kind"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.MoneyKind == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"money_kind": "fiat"|"stablecoin"|"hybrid"}`)
			return
		}
		by := ""
		if actx := auth.GetAuthContext(req.Context()); actx != nil {
			by = actx.UserID
		}
		out, err := bank.SetWorkspaceMoneyKind(req.Context(), chi.URLParam(req, "wsID"), in.MoneyKind, by)
		write(w, out, err)
	}))
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
