package budgets

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func newTestStore(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return newStore(pool), pool
}

func TestCreate_NormalizesWorkspaceScopeIDAndInserts(t *testing.T) {
	st, pool := newTestStore(t)
	now := time.Now()
	// scope_id passed empty for a workspace budget → normalized to ws id.
	pool.ExpectQuery(`INSERT INTO budgets`).
		WithArgs("ws1", "workspace", "ws1", "monthly", float64(500),
			[]float64{0.5, 0.8, 0.9}, "alert", (*time.Time)(nil), (*time.Time)(nil)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "spent_usd", "created_at", "updated_at"}).
			AddRow("bid-1", float64(0), now, now))

	b, err := st.Create(context.Background(), Budget{
		WorkspaceID: "ws1", Scope: ScopeWorkspace, LimitUSD: 500, Enforcement: EnforcementAlert,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if b.ID != "bid-1" || b.ScopeID != "ws1" {
		t.Fatalf("unexpected created budget: %+v", b)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestCreate_ValidationErrors(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	cases := []struct {
		name string
		b    Budget
		want error
	}{
		{"bad scope", Budget{WorkspaceID: "ws1", Scope: "nonsense"}, ErrInvalidScope},
		{"team without scope_id", Budget{WorkspaceID: "ws1", Scope: ScopeTeam}, ErrScopeIDRequired},
		{"bad enforcement", Budget{WorkspaceID: "ws1", Scope: ScopeTeam, ScopeID: "t", Enforcement: "nuke"}, ErrInvalidEnforcement},
		{"bad threshold", Budget{WorkspaceID: "ws1", Scope: ScopeTeam, ScopeID: "t", AlertThresholds: []float64{1.5}}, ErrInvalidThreshold},
		{"bad period", Budget{WorkspaceID: "ws1", Scope: ScopeTeam, ScopeID: "t", Period: "hourly"}, ErrInvalidPeriod},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := st.Create(ctx, c.b); !errors.Is(err, c.want) {
				t.Fatalf("got %v want %v", err, c.want)
			}
		})
	}
}

// TestReconcileSpent_PerScope is the core of the "spend attributed by
// scope" guarantee: workspace budgets sum by workspace_id (the corrected
// per-workspace path), team by team, sprint by sprint_id.
func TestReconcileSpent_PerScope(t *testing.T) {
	ctx := context.Background()

	t.Run("workspace sums by workspace_id", func(t *testing.T) {
		st, pool := newTestStore(t)
		pool.ExpectQuery(`SUM\(cost_usd\).*FROM token_events.*workspace_id = \$1`).
			WithArgs("ws1").
			WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(float64(42)))
		got, err := st.ReconcileSpent(ctx, Budget{WorkspaceID: "ws1", Scope: ScopeWorkspace, ScopeID: "ws1", Period: "monthly"})
		if err != nil || got != 42 {
			t.Fatalf("workspace reconcile: got %.2f err %v", got, err)
		}
		if err := pool.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("team sums by workspace_id AND team", func(t *testing.T) {
		st, pool := newTestStore(t)
		pool.ExpectQuery(`workspace_id = \$1 AND team = \$2`).
			WithArgs("ws1", "teamA").
			WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(float64(7)))
		got, err := st.ReconcileSpent(ctx, Budget{WorkspaceID: "ws1", Scope: ScopeTeam, ScopeID: "teamA", Period: "monthly"})
		if err != nil || got != 7 {
			t.Fatalf("team reconcile: got %.2f err %v", got, err)
		}
		if err := pool.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("sprint sums by workspace_id AND sprint_id", func(t *testing.T) {
		st, pool := newTestStore(t)
		pool.ExpectQuery(`workspace_id = \$1 AND sprint_id = \$2`).
			WithArgs("ws1", "sp1").
			WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(float64(3)))
		got, err := st.ReconcileSpent(ctx, Budget{WorkspaceID: "ws1", Scope: ScopeSprint, ScopeID: "sp1", Period: "monthly"})
		if err != nil || got != 3 {
			t.Fatalf("sprint reconcile: got %.2f err %v", got, err)
		}
		if err := pool.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})
}

// B28.246: a delete that removes nothing is ErrNotFound, so the route answers 404.
func TestDelete_MissingIsNotFound(t *testing.T) {
	st, pool := newTestStore(t)
	pool.ExpectExec(`DELETE FROM budgets`).
		WithArgs("bid-1", "ws1").
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	if err := st.Delete(context.Background(), "ws1", "bid-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete of a missing budget = %v, want ErrNotFound", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestNilPool_NoPanicNoWrite(t *testing.T) {
	st := NewStore(nil)
	ctx := context.Background()
	if _, err := st.List(ctx, "ws1"); err != nil {
		t.Fatalf("List on nil pool: %v", err)
	}
	if got, err := st.ReconcileSpent(ctx, Budget{WorkspaceID: "ws1", Scope: ScopeWorkspace, SpentUSD: 9}); err != nil || got != 9 {
		t.Fatalf("ReconcileSpent on nil pool should echo SpentUSD: got %.2f err %v", got, err)
	}
}
