package scan_test

import (
	"context"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/scan"
)

var energySources = []string{"gie-agsi", "gie-alsi", "entsog", "eia", "ecmwf-open"}

func TestCollectorsStoreWithProvenanceAndRerunIsIdempotent(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	src := newSources(t)
	clear := func() {
		// The collectors' own series only: the provenance tests' "test." points stay theirs.
		if _, err := pool.Exec(ctx, `DELETE FROM scan_points WHERE kind = 'observed' AND source_id = ANY($1)
			AND series NOT LIKE 'test.%'`, energySources); err != nil {
			t.Fatal(err)
		}
	}
	clear()
	// The fixtures are served locally, so the register's pace is lifted for the test.
	if _, err := pool.Exec(ctx, `UPDATE scan_sources SET requests_per_minute = 60000 WHERE id = ANY($1)`, energySources); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clear()
		_, _ = pool.Exec(ctx, `UPDATE scan_sources SET requests_per_minute = NULL, enabled = FALSE WHERE id = ANY($1)`, energySources)
	})
	counts := func() map[string]int {
		out := map[string]int{}
		rows, err := pool.Query(ctx, `SELECT source_id, count(*) FROM scan_points WHERE kind = 'observed'
			AND source_id = ANY($1) AND series NOT LIKE 'test.%' GROUP BY source_id`, energySources)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				t.Fatal(err)
			}
			out[id] = n
		}
		return out
	}

	// Without EIA's key, its collector stays off, its source is switched off, and the status page says why.
	noEIA := scan.NewCollectors(s, src.Client(), map[string]string{scan.SettingGIE: "gie-key"}, src.collectors("gie-key", "")...)
	noEIA.Run(ctx)
	first := counts()
	want := map[string]int{"gie-agsi": 42, "gie-alsi": 2, "entsog": 18, "ecmwf-open": 45}
	for id, n := range want {
		if first[id] != n {
			t.Errorf("%s stored %d points, want %d (all: %v)", id, first[id], n, first)
		}
	}
	if first["eia"] != 0 {
		t.Errorf("EIA without its key stored %d points", first["eia"])
	}
	if eia, _ := s.Source(ctx, "eia"); eia.Enabled {
		t.Error("EIA's source is on without its key")
	}
	for _, st := range noEIA.Status(ctx) {
		switch {
		case st.Source == "eia" && (st.On || !strings.Contains(st.Note, scan.SettingEIA)):
			t.Errorf("EIA's status without its key: %+v", st)
		case st.Source != "eia" && (!st.On || st.Failing || st.LastRetrieved == nil):
			t.Errorf("%s's status: %+v", st.Source, st)
		case st.Source == "gie-agsi" && !strings.Contains(st.Note, "internal"):
			t.Errorf("GIE is internal only until its terms are confirmed: %+v", st)
		}
	}

	// Every stored point carries its provenance.
	var licence, hash, transforms string
	if err := pool.QueryRow(ctx, `SELECT licence, content_hash, transforms::text FROM scan_points
		WHERE source_id = 'entsog' AND series = 'entsog.flow.norway-emden' AND observed_at = '2025-10-01T04:00:00Z'`).
		Scan(&licence, &hash, &transforms); err != nil {
		t.Fatal(err)
	}
	if licence == "" || hash != src.sums["entsog-2025-10-01.json"] || !strings.Contains(transforms, "ITP-00081") {
		t.Errorf("provenance: licence %q hash %q transforms %s", licence, hash, transforms)
	}

	// A re-run stores nothing new; with its key, EIA joins.
	all := scan.NewCollectors(s, src.Client(), map[string]string{scan.SettingGIE: "gie-key", scan.SettingEIA: "eia-key"},
		src.collectors("gie-key", "eia-key")...)
	all.Run(ctx)
	second := counts()
	want["eia"] = 3
	for id, n := range want {
		if second[id] != n {
			t.Errorf("after a re-run %s holds %d points, want %d", id, second[id], n)
		}
	}
}
