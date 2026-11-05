package scan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Only an approved report is published: a pending, refused or rejected one is never listed and its id is not found.
// The approved one is listed under its family with its sections, its figures' points, its sources and the box, and
// never the refusal, the reviewer or the review note.
func TestOnlyApprovedReportsArePublished(t *testing.T) {
	s := modelStore(t)
	ctx := context.Background()
	series := fmt.Sprintf("eia.stocks.published-%d", time.Now().UnixNano())
	t.Cleanup(func() {
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
	if r, err := NewModels(s).RunSeries(ctx, series); err != nil || !r.Passed {
		t.Fatalf("%+v, %v", r, err)
	}
	w := NewWriter(s, nil, "", "", "None.")
	w.Run(ctx)
	var approved int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM scan_reports WHERE series = $1 AND status = 'pending'`, series).
		Scan(&approved); err != nil {
		t.Fatal(err)
	}
	// Three more of the same report, a week apart, one in each other state.
	others := map[string]int64{}
	for i, status := range []string{ReportPending, ReportRefused, ReportRejected} {
		var id int64
		if err := s.pool.QueryRow(ctx, `INSERT INTO scan_reports (series, finding_at, backtest_id, draft, writer,
			model_check, status, refusal, reviewed_by, reviewed_at, review_note)
			SELECT series, finding_at - make_interval(weeks => $2), backtest_id, draft, writer, model_check, $3,
				CASE WHEN $3 = 'refused' THEN 'secret refusal' END,
				CASE WHEN $3 = 'rejected' THEN 'secret reviewer' END, CASE WHEN $3 = 'rejected' THEN NOW() END, ''
			FROM scan_reports WHERE id = $1 RETURNING id`, approved, i+1, status).Scan(&id); err != nil {
			t.Fatal(err)
		}
		others[status] = id
	}
	if _, err := w.Decide(ctx, approved, true, "secret reviewer", "secret note"); err != nil {
		t.Fatal(err)
	}

	listed := func(family string) map[int64]PublishedReport {
		t.Helper()
		all, err := w.Published(ctx, family)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]PublishedReport{}
		for _, p := range all {
			if p.Series == series {
				out[p.ID] = p
			}
		}
		return out
	}
	for status, id := range others {
		if _, ok := listed("")[id]; ok {
			t.Errorf("a %s report is listed", status)
		}
		if _, err := w.PublishedReport(ctx, id); !errors.Is(err, ErrReportNotFound) {
			t.Errorf("a %s report was found: %v", status, err)
		}
	}
	if got := listed("crops"); len(got) != 0 {
		t.Fatalf("an oil report is listed under crops: %+v", got)
	}
	p, ok := listed("oil")[approved]
	if !ok || len(listed("")) != 1 {
		t.Fatalf("the approved report is not listed alone: %+v", listed(""))
	}
	one, err := w.PublishedReport(ctx, approved)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(p)
	b, _ := json.Marshal(one)
	if string(a) != string(b) {
		t.Fatalf("the list and the report differ:\n%s\n%s", a, b)
	}
	if p.Family != "oil" || p.Title == "" || p.WhatChanged == "" || p.Confidence == "" || len(p.Sources) == 0 ||
		p.Box.MadeBy != MadeBy || p.Box.Conflicts != "None." || p.Box.TrackRecord != TrackRecordPath ||
		len(p.Box.Data) == 0 || p.Box.Method == "" || p.Box.Backtest == "" || p.PublishedAt.IsZero() {
		t.Fatalf("published: %s", a)
	}
	charted := 0
	for _, f := range p.Figures {
		if f.Value != nil && f.At != nil && f.Unit != "" {
			charted++
		}
	}
	if charted == 0 {
		t.Fatalf("no figure carries its point's value, unit and date: %s", a)
	}
	if strings.Contains(string(a), "secret") {
		t.Fatalf("the review leaked into the published report: %s", a)
	}
}
