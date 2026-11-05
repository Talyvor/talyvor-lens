package scan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A passed model's finding becomes a pending report with every figure traced and the box complete; it is published
// only when a person approves it, and the database refuses an approval that names nobody. A report the second model
// fails is refused, and no report is written without the conflicts statement.
func TestReportsWaitForAPersonsApproval(t *testing.T) {
	s := modelStore(t)
	ctx := context.Background()
	m := NewModels(s)
	run := time.Now().UnixNano()
	passed, failed := fmt.Sprintf("eia.stocks.report-a-%d", run), fmt.Sprintf("eia.stocks.report-b-%d", run)
	t.Cleanup(func() {
		for _, series := range []string{passed, failed} {
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
		}
	})
	recordSeries(t, s, passed, weekly(8, 0.9, 1))
	if r, err := m.RunSeries(ctx, passed); err != nil || !r.Passed {
		t.Fatalf("%+v, %v", r, err)
	}
	mine := func(status, series string) []QueuedReport {
		t.Helper()
		all, err := NewWriter(s, nil, "", "", "").Queue(ctx, status)
		if err != nil {
			t.Fatal(err)
		}
		var out []QueuedReport
		for _, q := range all {
			if q.Series == series {
				out = append(out, q)
			}
		}
		return out
	}

	NewWriter(s, fakeInferer{"check": "PASS"}, "", "check", "").Run(ctx)
	if got := mine("", passed); len(got) != 0 {
		t.Fatalf("a report was written with no conflicts statement: %+v", got)
	}

	// The writer model says buy, so the template stands; the second model passes it.
	w := NewWriter(s, fakeInferer{"writer": `{"what_changed": "Buy now.", "why_it_matters": "x", "confidence": "x",
		"what_would_change": "x"}`, "check": "PASS"}, "writer", "check", "Talyvor has no interest in the oil traded.")
	w.Run(ctx)
	w.Run(ctx) // a second run writes nothing more
	pending := mine(ReportPending, passed)
	if len(pending) != 1 {
		t.Fatalf("pending: %+v (all: %+v)", pending, mine("", passed))
	}
	q := pending[0]
	if q.Writer != WrittenByTemplate || q.ModelCheck != "PASS" || len(Check(q.Draft)) != 0 ||
		q.Draft.Box.Conflicts != "Talyvor has no interest in the oil traded." || q.Draft.Box.MadeBy != MadeBy ||
		q.Draft.Box.TrackRecord != TrackRecordPath || len(q.Draft.Box.Data) != 1 ||
		q.Draft.Sources[0] != "Source: U.S. Energy Information Administration" {
		t.Fatalf("queued: %+v", q)
	}
	for _, f := range q.Draft.Figures {
		if f.Point == 0 && f.Backtest == 0 {
			t.Fatalf("figure %s is not traced to the engine", f.Name)
		}
		if f.Point != 0 {
			if _, err := s.ForDisplay(ctx, f.Point); err != nil {
				t.Fatalf("figure %s: %v", f.Name, err)
			}
		}
	}
	if got := mine(ReportApproved, passed); len(got) != 0 {
		t.Fatalf("published before review: %+v", got)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE scan_reports SET status = 'approved' WHERE id = $1`, q.ID); err == nil ||
		!strings.Contains(err.Error(), "scan_reports_decided_by_a_person") {
		t.Fatalf("the database let a report be approved by nobody: %v", err)
	}
	if _, err := w.Decide(ctx, q.ID, true, " ", ""); !errors.Is(err, ErrNoReviewer) {
		t.Fatalf("approved without a reviewer: %v", err)
	}
	got, err := w.Decide(ctx, q.ID, true, "nicolai", "read in full")
	if err != nil || got.Status != ReportApproved || got.ReviewedBy != "nicolai" || got.ReviewedAt == nil {
		t.Fatalf("%+v, %v", got, err)
	}
	if pub := mine(ReportApproved, passed); len(pub) != 1 || pub[0].ID != q.ID {
		t.Fatalf("published: %+v", pub)
	}
	if _, err := w.Decide(ctx, q.ID, false, "nicolai", ""); !errors.Is(err, ErrReportDecided) {
		t.Fatalf("decided twice: %v", err)
	}
	if _, err := w.Decide(ctx, -1, true, "nicolai", ""); !errors.Is(err, ErrReportNotFound) {
		t.Fatalf("decided a report that does not exist: %v", err)
	}

	recordSeries(t, s, failed, weekly(8, 0.9, 1))
	if r, err := m.RunSeries(ctx, failed); err != nil || !r.Passed {
		t.Fatalf("%+v, %v", r, err)
	}
	NewWriter(s, fakeInferer{"check": "FAIL: reads as a call to trade"}, "", "check", "None.").Run(ctx)
	refused := mine(ReportRefused, failed)
	if len(refused) != 1 || refused[0].Refusal != "the second model: FAIL: reads as a call to trade" {
		t.Fatalf("refused: %+v (all: %+v)", refused, mine("", failed))
	}
	if _, err := w.Decide(ctx, refused[0].ID, true, "nicolai", ""); !errors.Is(err, ErrReportDecided) {
		t.Fatalf("a refused report was approved: %v", err)
	}
}
