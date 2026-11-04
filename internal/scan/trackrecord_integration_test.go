package scan

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Approving a report records its observation and range in the track record; the record waits until the outcome's
// reading arrives and is then scored once, hit or miss; and the database refuses any edit to it after that.
func TestTrackRecordScoresPublishedReportsAndIsNeverEdited(t *testing.T) {
	s := modelStore(t)
	ctx := context.Background()
	series := fmt.Sprintf("eia.stocks.track-%d", time.Now().UnixNano())
	t.Cleanup(func() { // the track record itself is never deleted: its rows carry this run's own series
		for _, q := range []string{
			`DELETE FROM scan_reports WHERE series = $1`,
			`DELETE FROM scan_point_inputs WHERE point_id IN (SELECT id FROM scan_points WHERE starts_with(series, $1))`,
			`DELETE FROM scan_points WHERE starts_with(series, $1)`,
			`DELETE FROM scan_backtests WHERE series = $1`,
		} {
			if _, err := s.pool.Exec(ctx, q, series); err != nil {
				t.Error(err)
			}
		}
	})
	recordSeries(t, s, series, weekly(8, 0.9, 1))
	if r, err := NewModels(s).RunSeries(ctx, series); err != nil || !r.Passed || r.Finding == nil {
		t.Fatalf("%+v, %v", r, err)
	}
	w := NewWriter(s, nil, "", "", "None.")
	w.Run(ctx)
	mine := func() *TrackEntry {
		t.Helper()
		tr, err := w.TrackRecord(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i, e := range tr.Records {
			if e.Series == series {
				return &tr.Records[i]
			}
		}
		return nil
	}
	queue, err := w.Queue(ctx, ReportPending)
	if err != nil {
		t.Fatal(err)
	}
	var report QueuedReport
	for _, q := range queue {
		if q.Series == series {
			report = q
		}
	}
	if report.ID == 0 {
		t.Fatal("no pending report")
	}
	if e := mine(); e != nil {
		t.Fatalf("recorded before it was published: %+v", e)
	}
	if _, err := w.Decide(ctx, report.ID, true, "nicolai", ""); err != nil {
		t.Fatal(err)
	}

	var f Finding
	if err := s.pool.QueryRow(ctx, `SELECT (finding ->> 'outlook_at')::timestamptz, (finding ->> 'outlook_low')::float8,
		(finding ->> 'outlook_high')::float8 FROM scan_backtests WHERE id = (SELECT backtest_id FROM scan_reports WHERE id = $1)`,
		report.ID).Scan(&f.OutlookAt, &f.OutlookLow, &f.OutlookHigh); err != nil {
		t.Fatal(err)
	}
	e := mine()
	if e == nil || e.ReportID != report.ID || e.Title != report.Draft.Title || len(e.Sources) == 0 ||
		!e.OutlookAt.Equal(f.OutlookAt) || e.OutlookLow != f.OutlookLow || e.OutlookHigh != f.OutlookHigh || e.Hit != nil {
		t.Fatalf("recorded at publishing: %+v, want the range %+v", e, f)
	}

	// Before the outcome's reading arrives nothing is scored; once it does, the record is scored a hit.
	if n, err := w.Score(ctx); err != nil || mine().Hit != nil {
		t.Fatalf("scored with no outcome: %d, %v", n, err)
	}
	outcome := (f.OutlookLow + f.OutlookHigh) / 2
	recordSeries(t, s, series, []storedPoint{{at: f.OutlookAt.Add(time.Hour), value: outcome}})
	if _, err := w.Score(ctx); err != nil {
		t.Fatal(err)
	}
	e = mine()
	if e.Hit == nil || !*e.Hit || e.Outcome == nil || *e.Outcome != outcome || e.ScoredAt == nil || e.OutcomePoint == nil {
		t.Fatalf("scored: %+v", e)
	}
	tr, err := w.TrackRecord(ctx)
	if err != nil || tr.Hits < 1 {
		t.Fatalf("tally: %+v, %v", tr, err)
	}

	// A past record is never edited: not its published range, not its score, and it is never deleted.
	for name, q := range map[string]string{
		"the range":  `UPDATE scan_track_record SET outlook_high = outlook_high + 1000 WHERE report_id = $1`,
		"the score":  `UPDATE scan_track_record SET hit = false, outcome_value = outlook_high + 1 WHERE report_id = $1`,
		"the title":  `UPDATE scan_track_record SET title = 'better' WHERE report_id = $1`,
		"the record": `DELETE FROM scan_track_record WHERE report_id = $1`,
	} {
		if _, err := s.pool.Exec(ctx, q, report.ID); err == nil || !strings.Contains(err.Error(), "never edited") {
			t.Errorf("%s was edited: %v", name, err)
		}
	}
	if after := mine(); after == nil || after.OutlookHigh != e.OutlookHigh || !*after.Hit || after.Title != e.Title {
		t.Fatalf("the record changed: %+v", after)
	}
}
