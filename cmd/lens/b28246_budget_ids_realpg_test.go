package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/talyvor/lens/internal/budgets"
)

func b28246Router(store *budgets.Store) chi.Router {
	reload := func(context.Context) error { return nil }
	r := chi.NewRouter()
	r.Get("/v1/workspaces/{wsID}/budgets/{id}", budgetGetHandler(store))
	r.Patch("/v1/workspaces/{wsID}/budgets/{id}", budgetUpdateHandler(store, reload))
	r.Delete("/v1/workspaces/{wsID}/budgets/{id}", budgetDeleteHandler(store, reload))
	return r
}

// B28.246 — a budget id that is not a UUID is the caller's mistake: 400, before
// the database ever sees it (it used to come back as a Postgres cast error, 500).
func TestB28246_BudgetIDThatIsNotAUUIDAnswers400(t *testing.T) {
	r := b28246Router(budgets.NewStore(b2613DB(t)))
	for _, method := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(method, "/v1/workspaces/ws-b28246/budgets/abc", nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s /budgets/abc = %d, want 400; body %s", method, rec.Code, rec.Body)
		}
	}
}

// B28.246 — deleting a budget that does not exist (or is another workspace's)
// answers 404, not 200, and leaves the other workspace's budget in place.
func TestB28246_DeletingAMissingBudgetAnswers404(t *testing.T) {
	ctx := context.Background()
	store := budgets.NewStore(b2613DB(t))
	theirs, err := store.Create(ctx, budgets.Budget{WorkspaceID: "ws-b28246-other", Scope: budgets.ScopeWorkspace, LimitUSD: 5})
	if err != nil {
		t.Fatal(err)
	}
	r := b28246Router(store)
	for _, id := range []string{uuid.NewString(), theirs.ID} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/workspaces/ws-b28246/budgets/"+id, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("DELETE /budgets/%s = %d, want 404; body %s", id, rec.Code, rec.Body)
		}
	}
	if _, err := store.Get(ctx, "ws-b28246-other", theirs.ID); err != nil {
		t.Fatalf("the other workspace's budget is gone: %v", err)
	}
}
