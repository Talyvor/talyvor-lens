package main

// B28.243 — a handler that calls Authenticate itself answers a failed credential lookup 503 with
// Retry-After, not the 401 or 403 that tells a caller with a good key to sign in again.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/lens/internal/auth"
)

type storeDownAuthenticator struct{}

func (storeDownAuthenticator) Authenticate(*http.Request) (*auth.AuthContext, error) {
	return nil, fmt.Errorf("%w: connection refused", auth.ErrAuthUnavailable)
}

func TestDirectAuthenticateHandlers_StoreDownIs503(t *testing.T) {
	am := storeDownAuthenticator{}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handlers := map[string]http.Handler{
		"requireAdmin":               requireAdmin(am, next),
		"requireAdminOrOperatorRead": requireAdminOrOperatorRead(am, next),
		"requireAdminOrModerator":    requireAdminOrModerator(am, nil, next),
		"requireStepUp":              requireStepUp(am, nil, next),
		"GET /v1/workspaces":         newListMyWorkspacesHandler(am, nil),
		"output verdicts":            newOutputVerdictsWorkspaceHandler(am, nil),
		"mechanical verdict":         newMechanicalVerdictHandler(am, nil),
		"attribution":                newAttributionHandler(am, nil),
		"attribution by output":      newAttributionByOutputHandler(am, nil),
		"attribution list":           newAttributionListHandler(am, nil),
		"artifact commit":            newArtifactCommitHandler(am, nil),
		"bond create":                newBondCreateHandler(am, nil),
		"bond list":                  newBondListHandler(am, nil),
		"bond get":                   newBondGetHandler(am, nil),
		"adjudicate":                 newAdjudicateHandler(am, nil),
		"traffic revoke":             newTrafficRevokeHandler(am, nil, nil),
	}
	for name, h := range handlers {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", http.NoBody))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: a failed credential lookup answered %d, want 503", name, rec.Code)
			continue
		}
		if got := rec.Header().Get("Retry-After"); got != auth.AuthUnavailableRetryAfter {
			t.Errorf("%s: Retry-After = %q, want %q", name, got, auth.AuthUnavailableRetryAfter)
		}
	}
}
