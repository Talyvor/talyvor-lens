package scan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

func modelStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG model test")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := NewStore(pool)
	if err := s.SetEnabled(ctx, "eia", true); err != nil {
		t.Fatal(err)
	}
	return s
}

func recordSeries(t *testing.T, s *Store, series string, pts []storedPoint) {
	t.Helper()
	for _, p := range pts {
		if _, err := s.Record(context.Background(), Observation{SourceID: "eia", Series: series, ObservedAt: p.at,
			Value: p.value, Unit: "thousand barrels", RetrievedAt: time.Now().UTC(),
			ContentHash: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", Transforms: []string{}}); err != nil {
			t.Fatal(err)
		}
	}
}

// A series whose model passed its backtest is assessed, every figure stored with its inputs and its source's credit;
// one whose model failed is refused and nothing is derived from it; both backtests are on the operator's page.
func TestModelsBacktestThenAssessOrRefuse(t *testing.T) {
	s := modelStore(t)
	ctx := context.Background()
	m := NewModels(s)
	run := time.Now().UnixNano()
	good, bad := fmt.Sprintf("eia.stocks.test-good-%d", run), fmt.Sprintf("eia.stocks.test-bad-%d", run)
	t.Cleanup(func() { // the energy collectors' test clears every EIA point, which these figures would hold on to
		for _, series := range []string{good, bad} {
			for _, q := range []string{
				`DELETE FROM scan_point_inputs WHERE point_id IN (SELECT id FROM scan_points WHERE starts_with(series, $1))`,
				`DELETE FROM scan_points WHERE starts_with(series, $1)`,
				`DELETE FROM scan_backtests WHERE series = $1`,
			} {
				if _, err := s.pool.Exec(ctx, q, series); err != nil {
					t.Error(err)
				}
			}
		}
	})
	recordSeries(t, s, good, weekly(8, 0.9, 1))
	recordSeries(t, s, bad, weekly(8, 0, 2))

	r, err := m.RunSeries(ctx, good)
	if err != nil || !r.Passed || r.Finding == nil {
		t.Fatalf("good series: %+v, %v", r, err)
	}
	f := r.Finding
	if !(f.NormalLow < f.NormalHigh && f.OutlookLow < f.OutlookHigh) || f.OutlookAt.Sub(f.At) != 4*oneWeek {
		t.Fatalf("finding: %+v", f)
	}
	for _, name := range []string{"normal_low", "normal_high", "z", "outlook_low", "outlook_high"} {
		id := f.Points[name]
		d, err := s.ForDisplay(ctx, id)
		if err != nil || len(d.Attribution) != 1 || d.Attribution[0] != "Source: U.S. Energy Information Administration" {
			t.Fatalf("%s (point %d): %+v, %v", name, id, d, err)
		}
		if in, _ := s.Inputs(ctx, id); len(in) < normalMinYears {
			t.Fatalf("%s lists %d inputs", name, len(in))
		}
	}
	if z, _ := s.ForDisplay(ctx, f.Points["z"]); z.Value != f.Z {
		t.Fatalf("stored z %v, finding z %v", z.Value, f.Z)
	}
	again, err := m.Assess(ctx, good)
	if err != nil || again.Points["z"] != f.Points["z"] {
		t.Fatalf("re-assessing stored the figures twice: %v vs %v, %v", again.Points, f.Points, err)
	}

	if _, err := m.Assess(ctx, bad); !errors.Is(err, ErrNotTested) {
		t.Fatalf("an untested series was assessed: %v", err)
	}
	r, err = m.RunSeries(ctx, bad)
	if err != nil || r.Passed || r.Finding != nil {
		t.Fatalf("noise series: %+v, %v", r, err)
	}
	if _, err := m.Assess(ctx, bad); !errors.Is(err, ErrModelRefused) {
		t.Fatalf("a failed model was used: %v", err)
	}
	var derived int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM scan_points WHERE series LIKE $1`, bad+".model.%").Scan(&derived); err != nil || derived != 0 {
		t.Fatalf("%d figures derived from a failed model (%v)", derived, err)
	}

	reports, err := m.Reports(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]Report{}
	for _, r := range reports {
		seen[r.Series] = r
	}
	if g, b := seen[good], seen[bad]; !g.Passed || g.Finding == nil || g.Finding.Points["z"] != f.Points["z"] || b.Passed ||
		b.Finding != nil || b.Verdict == "" || g.Method == "" {
		t.Fatalf("page: good %+v, bad %+v", g, b)
	}
}
