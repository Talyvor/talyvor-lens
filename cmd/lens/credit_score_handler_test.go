package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

type fakeCreditScorer struct{ asked []string }

func (f *fakeCreditScorer) CreditScore(_ context.Context, ws string, _ time.Time) (economy.CreditScore, error) {
	f.asked = append(f.asked, "score:"+ws)
	return economy.CreditScore{WorkspaceID: ws, Score: 171, Band: "E"}, nil
}

func (f *fakeCreditScorer) RequestCreditScoreReview(_ context.Context, ws, by, note string, _ time.Time) (economy.CreditScoreReview, error) {
	f.asked = append(f.asked, "review:"+ws+":"+by+":"+note)
	return economy.CreditScoreReview{ID: "csr_1", WorkspaceID: ws, Status: "open"}, nil
}

// B30.39 — only the workspace's owner or an admin sees its credit score or asks for a review: an agent's own key
// is refused before the score is worked out.
func TestCreditScoreRoutes_OwnerOnly(t *testing.T) {
	store := &fakeCreditScorer{}
	r := chi.NewRouter()
	mountCreditScoreRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: "co", AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	agentKey := &auth.AuthContext{WorkspaceID: "co", AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "k", Scopes: []string{auth.ScopeProxy}}
	do := func(who *auth.AuthContext, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	for _, try := range []struct{ method, path string }{{http.MethodGet, "/v1/money/credit-score"}, {http.MethodPost, "/v1/money/credit-score/review"}} {
		if w := do(agentKey, try.method, try.path, `{}`); w.Code != http.StatusForbidden {
			t.Fatalf("agent key %s %s = %d %s, want 403", try.method, try.path, w.Code, w.Body)
		}
	}
	if len(store.asked) != 0 {
		t.Fatalf("an agent key reached the store: %v", store.asked)
	}
	if w := do(owner, http.MethodGet, "/v1/money/credit-score", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"band":"E"`) {
		t.Fatalf("owner GET = %d %s", w.Code, w.Body)
	}
	if w := do(owner, http.MethodPost, "/v1/money/credit-score/review", `{"note":"please look"}`); w.Code != http.StatusOK || store.asked[1] != "review:co:jwt:user:owner:please look" {
		t.Fatalf("owner review = %d %s, asked %v", w.Code, w.Body, store.asked)
	}
}
