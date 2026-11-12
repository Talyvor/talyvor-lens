package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/budgets"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

func b277DB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG budget PATCH test")
	}
	const schema = "cmd_b277_budget_patch_realpg"
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatalf("reset schema: %v", err)
		}
	}
	if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = conn.Close(ctx)
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func b277Router(store *budgets.Store) *chi.Mux {
	reload := func(context.Context) error { return nil }
	r := chi.NewRouter()
	r.Post("/v1/workspaces/{wsID}/budgets", budgetCreateHandler(store, reload))
	r.Patch("/v1/workspaces/{wsID}/budgets/{id}", budgetUpdateHandler(store, reload))
	return r
}

func b277Do(t *testing.T, r http.Handler, method, path, body string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w.Code, w.Body.String()
}

// B27.7 — PATCH {"limit_usd":20} changes the limit and nothing else: the
// thresholds, enforcement, period and end date the caller left out keep their
// values, read back from the row.
func TestB277_PatchOnlyLimitKeepsEveryOtherField(t *testing.T) {
	ctx := context.Background()
	pool := b277DB(t)
	store := budgets.NewStore(pool)
	const ws = "ws-b277"
	ends := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	b, err := store.Create(ctx, budgets.Budget{
		WorkspaceID: ws, Scope: budgets.ScopeTeam, ScopeID: "t1", Period: "weekly", LimitUSD: 5,
		AlertThresholds: []float64{0.25, 0.75}, Enforcement: budgets.EnforcementHardBlock, EndsAt: &ends,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := b277Router(store)

	code, body := b277Do(t, r, http.MethodPatch, "/v1/workspaces/"+ws+"/budgets/"+b.ID, `{"limit_usd":20}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH {limit_usd:20}: status %d, want 200 (body %s)", code, body)
	}

	var (
		limit      float64
		thresholds []float64
		enf, per   string
		endsAt     *time.Time
	)
	if err := pool.QueryRow(ctx, `SELECT limit_usd, alert_thresholds, enforcement, period, ends_at FROM budgets WHERE id = $1`, b.ID).
		Scan(&limit, &thresholds, &enf, &per, &endsAt); err != nil {
		t.Fatal(err)
	}
	if limit != 20 {
		t.Errorf("limit_usd = %v, want 20", limit)
	}
	if len(thresholds) != 2 || thresholds[0] != 0.25 || thresholds[1] != 0.75 {
		t.Errorf("alert_thresholds = %v, want the kept [0.25 0.75]", thresholds)
	}
	if enf != "hard_block" {
		t.Errorf("enforcement = %q, want the kept hard_block", enf)
	}
	if per != "weekly" {
		t.Errorf("period = %q, want the kept weekly", per)
	}
	if endsAt == nil || !endsAt.Equal(ends) {
		t.Errorf("ends_at = %v, want the kept %v", endsAt, ends)
	}

	// An explicit null is how a caller clears the end date.
	if code, body := b277Do(t, r, http.MethodPatch, "/v1/workspaces/"+ws+"/budgets/"+b.ID, `{"ends_at":null}`); code != http.StatusOK {
		t.Fatalf("PATCH {ends_at:null}: status %d (body %s)", code, body)
	}
	if err := pool.QueryRow(ctx, `SELECT limit_usd, ends_at FROM budgets WHERE id = $1`, b.ID).Scan(&limit, &endsAt); err != nil {
		t.Fatal(err)
	}
	if endsAt != nil || limit != 20 {
		t.Errorf("after PATCH {ends_at:null}: ends_at = %v, limit_usd = %v; want nil and the kept 20", endsAt, limit)
	}
}

// B27.7 — no answer from the budget write routes carries the database's own
// words, whichever way the write fails.
func TestB277_BudgetWriteErrorsCarryNoDatabaseText(t *testing.T) {
	ctx := context.Background()
	pool := b277DB(t)
	store := budgets.NewStore(pool)
	const ws = "ws-b277-err"
	b, err := store.Create(ctx, budgets.Budget{WorkspaceID: ws, Scope: budgets.ScopeWorkspace, LimitUSD: 5})
	if err != nil {
		t.Fatal(err)
	}
	r := b277Router(store)
	base := "/v1/workspaces/" + ws + "/budgets"

	check := func(name, method, path, body string, want int) {
		t.Helper()
		code, got := b277Do(t, r, method, path, body)
		if code != want {
			t.Errorf("%s: status %d, want %d (body %s)", name, code, want, got)
		}
		if strings.Contains(got, "SQLSTATE") {
			t.Errorf("%s: database error text reached the caller: %s", name, got)
		}
	}
	check("second budget for the same scope", http.MethodPost, base, `{"scope":"workspace","limit_usd":9}`, http.StatusConflict)
	check("malformed budget id", http.MethodPatch, base+"/not-a-uuid", `{"limit_usd":1}`, http.StatusBadRequest) // B28.246: refused before the database

	// With the table gone every write is a server fault, answered plainly.
	if _, err := pool.Exec(ctx, `DROP TABLE budgets`); err != nil {
		t.Fatal(err)
	}
	check("create with the table gone", http.MethodPost, base, `{"scope":"team","scope_id":"t9","limit_usd":1}`, http.StatusInternalServerError)
	check("patch with the table gone", http.MethodPatch, base+"/"+b.ID, `{"limit_usd":1}`, http.StatusInternalServerError)
}
