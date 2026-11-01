package scan_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/scan"
	"github.com/talyvor/lens/migrations"
)

var migrateOnce sync.Once

func testStore(t *testing.T) (*scan.Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG scan test")
	}
	ctx := context.Background()
	migrateOnce.Do(func() {
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatalf("connect for migrate: %v", err)
		}
		defer conn.Close(ctx)
		if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
			t.Fatalf("apply migrations: %v", err)
		}
	})
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return scan.NewStore(pool), pool
}

const hash = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func observation(source string) scan.Observation {
	now := time.Now().UTC()
	return scan.Observation{SourceID: source, Series: "test.eia.cushing", ObservedAt: now.Add(-24 * time.Hour),
		Value: 22.4, Unit: "million barrels", RetrievedAt: now, ContentHash: hash,
		Transforms: []string{"json field series[0].data[0][1]", "thousand barrels → million barrels"}}
}

func TestPointWithoutProvenanceIsRefused(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	if err := s.SetEnabled(ctx, "eia", true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.SetEnabled(ctx, "eia", false) })

	for name, strip := range map[string]func(*scan.Observation){
		"no source":         func(o *scan.Observation) { o.SourceID = "" },
		"no retrieval time": func(o *scan.Observation) { o.RetrievedAt = time.Time{} },
		"no content hash":   func(o *scan.Observation) { o.ContentHash = "" },
		"bad content hash":  func(o *scan.Observation) { o.ContentHash = "abc" },
		"no transforms":     func(o *scan.Observation) { o.Transforms = nil },
	} {
		o := observation("eia")
		strip(&o)
		if _, err := s.Record(ctx, o); !errors.Is(err, scan.ErrNoProvenance) {
			t.Errorf("%s: want ErrNoProvenance, got %v", name, err)
		}
	}
	// The database refuses it too, whoever writes.
	if _, err := pool.Exec(ctx, `INSERT INTO scan_points (kind, series, observed_at, value, unit, source_id, transforms)
		VALUES ('observed', 'test.raw', NOW(), 1, 'x', 'eia', '[]')`); err == nil {
		t.Error("the database stored an observed point without licence, retrieval time or hash")
	}

	id, err := s.Record(ctx, observation("eia"))
	if err != nil {
		t.Fatalf("a point with its provenance: %v", err)
	}
	var licence, h string
	var transforms int
	if err := pool.QueryRow(ctx, `SELECT licence, content_hash, jsonb_array_length(transforms) FROM scan_points WHERE id = $1`, id).
		Scan(&licence, &h, &transforms); err != nil {
		t.Fatal(err)
	}
	if licence == "" || h != hash || transforms != 2 {
		t.Errorf("stored provenance: licence %q hash %q transforms %d", licence, h, transforms)
	}

	if err := s.SetEnabled(ctx, "eia", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(ctx, observation("eia")); !errors.Is(err, scan.ErrSourceOff) {
		t.Errorf("a switched-off source: want ErrSourceOff, got %v", err)
	}
}

func TestRuledOutSourceCannotBeSwitchedOn(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"opensky", "adsb-exchange", "quiver", "global-fishing-watch", "aishub-raw"} {
		if err := s.SetEnabled(ctx, id, true); !errors.Is(err, scan.ErrRuledOut) {
			t.Errorf("%s: want ErrRuledOut, got %v", id, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE scan_sources SET enabled = TRUE WHERE id = $1`, id); err == nil {
			t.Errorf("%s: the database switched a ruled-out source on", id)
		}
		if _, err := s.Record(ctx, observation(id)); !errors.Is(err, scan.ErrSourceOff) {
			t.Errorf("%s: a point from it was not refused: %v", id, err)
		}
	}
}

func TestDerivedFigureListsItsInputs(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	for _, src := range []string{"eia", "gie-agsi"} {
		if err := s.SetEnabled(ctx, src, true); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.SetEnabled(ctx, src, false) })
	}
	a, err := s.Record(ctx, observation("eia"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Record(ctx, observation("eia"))
	if err != nil {
		t.Fatal(err)
	}
	d := scan.Derivation{Series: "test.eia.cushing.mean", ObservedAt: time.Now(), Value: 22.4, Unit: "million barrels",
		Transforms: []string{"mean"}}
	if _, err := s.Derive(ctx, d); !errors.Is(err, scan.ErrNoInputs) {
		t.Fatalf("a derived figure without inputs: want ErrNoInputs, got %v", err)
	}
	d.Inputs = []int64{a, b}
	mean, err := s.Derive(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := s.Inputs(ctx, mean)
	if err != nil || len(inputs) != 2 || inputs[0] != a || inputs[1] != b {
		t.Fatalf("inputs of the derived figure: %v %v, want [%d %d]", inputs, err, a, b)
	}

	// Shown to a user, a figure derived from a derived figure still carries the attribution of the source behind it.
	d2 := scan.Derivation{Series: "test.eia.cushing.anomaly", ObservedAt: time.Now(), Value: 0.3, Unit: "z-score",
		Transforms: []string{"z-score against five-year mean"}, Inputs: []int64{mean}}
	z, err := s.Derive(ctx, d2)
	if err != nil {
		t.Fatal(err)
	}
	shown, err := s.ForDisplay(ctx, z)
	if err != nil {
		t.Fatal(err)
	}
	if len(shown.Attribution) != 1 || !strings.Contains(shown.Attribution[0], "Energy Information Administration") {
		t.Errorf("attribution: %v", shown.Attribution)
	}

	// GIE is internal only until Nick confirms its terms: nothing from it reaches a user.
	g, err := s.Record(ctx, observation("gie-agsi"))
	if err != nil {
		t.Fatal(err)
	}
	mixed, err := s.Derive(ctx, scan.Derivation{Series: "test.mixed", ObservedAt: time.Now(), Value: 1, Unit: "x",
		Transforms: []string{"sum"}, Inputs: []int64{a, g}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ForDisplay(ctx, mixed); !errors.Is(err, scan.ErrNotDisplayable) {
		t.Errorf("a figure from an internal-only source: want ErrNotDisplayable, got %v", err)
	}
}
