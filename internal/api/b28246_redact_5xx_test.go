package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/budgets"
)

// B28.246 — a database error on a dashboard read answers a generic 500: the
// driver's words (here the host and port it could not reach) stay in the log.
func TestB28246_DashboardDBErrorAnswersGeneric500(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://lens@127.0.0.1:1/lens?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := &Server{}
	s.SetBudgetStore(budgets.NewStore(pool))

	rec := httptest.NewRecorder()
	s.handleBudgets(rec, apiReq("/v1/api/budgets", "ws-b28246", false))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"error":"internal server error"}` {
		t.Fatalf("500 body = %s, want the generic error and nothing from the database", body)
	}
}
