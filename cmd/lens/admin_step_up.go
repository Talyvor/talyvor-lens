package main

import (
	"context"
	"net/http"
	"strings"

	"github.com/talyvor/lens/internal/operatoraudit"
)

// admin_step_up.go — B28.264: THE MOST DANGEROUS ADMIN ACTIONS NEED A SECOND FACTOR, AND EVERY USE IS IN THE TRAIL.
//
//	POST /v1/admin/conversion-rate/approve              changes the LENS→LXC price
//	POST /v1/admin/held-mints/{mint_type}/adjudicate    revokes held payouts (the four mint tables and traffic)
//	POST /v1/admin/lxc/grant                            mints LXC
//
// The admin key alone no longer takes any of them. Each also needs the operator named in X-Talyvor-Operator and a
// current step-up code in X-Talyvor-Step-Up, once only, exactly as requireStepUp asks for the compliance actions; and
// each use is an operator_audit row — who, which action, on which path, and the method that authorised it — written
// before the action runs. A row that cannot be written stops the action: an admin-key use nobody can see later is
// the thing this closes.

// adminStepUpMethod is the method an operator_audit row names for a use authorised by requireAdminStepUp.
const adminStepUpMethod = "admin key + step-up code"

// adminUseRecorder is *operatoraudit.Store.
type adminUseRecorder interface {
	Record(ctx context.Context, e operatoraudit.Entry) (operatoraudit.Entry, error)
}

// requireAdminStepUp is requireStepUp, then an operator_audit row for the use, then next. action is the row's action.
func requireAdminStepUp(am adminAuthenticator, v stepUpVerifier, audit adminUseRecorder, action string, next http.Handler) http.HandlerFunc {
	return requireStepUp(am, v, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := audit.Record(r.Context(), operatoraudit.Entry{
			Actor:  strings.TrimSpace(r.Header.Get(moderatorOperatorHeader)),
			Action: action,
			Target: r.Method + " " + r.URL.Path,
			Detail: "method: " + adminStepUpMethod,
		}); err != nil {
			writeJSONErr(w, http.StatusServiceUnavailable, "not done: the audit row for this admin action could not be written: "+err.Error())
			return
		}
		next.ServeHTTP(w, r)
	}))
}
