package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/scan"
)

// B52.40 — the nightly rehearses a Scan report on a test workspace with the synthetic key: the real writer on Lens's
// recorded distillate weeks drafts a pending report with its sources and whole box; the same with a planted "Buy" line is
// refused naming the word and can never be approved; the clean one is on the workspace's track record only once the
// test reviewer approves it, and an outcome inside its range scores a hit, once. None of it reaches the review queue,
// the public library or the public track record, and the recorded weeks leave no point or backtest behind. A workspace
// that is not synthetic is refused 403 and nothing is written.
func TestB5240_AScanReportRehearsedOnATestWorkspaceNeverReachesThePublicLibrary(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const key, ws, real = "b5240-synthetic-key", "ws-b5240-test", "ws-b5240-real"
	for id, synthetic := range map[string]bool{ws: true, real: false} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, $2)`, id, synthetic); err != nil {
			t.Fatal(err)
		}
	}
	writer := scan.NewWriter(scan.NewStore(pool), nil, "", "", "Talyvor holds no position in the oil traded.")
	r := chi.NewRouter()
	mountSyntheticDueRoutes(r, key, syntheticDeps{audit: pool, due: syntheticDueDeps{db: pool, scan: writer}})
	call := func(method, path string, body any, out any) int {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			if err := json.NewEncoder(&buf).Encode(body); err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest(method, path, &buf)
		req.Header.Set(syntheticKeyHeader, key)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code >= 300 {
			t.Logf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		if out != nil && rec.Code < 300 {
			if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
				t.Fatalf("%s %s: %v (%s)", method, path, err, rec.Body.String())
			}
		}
		return rec.Code
	}
	base := "/v1/synthetic/workspaces/" + ws + "/scan"
	type answer struct {
		Report scan.QueuedReport `json:"report"`
	}
	track := func() scan.TrackRecord {
		t.Helper()
		var tr scan.TrackRecord
		if code := call(http.MethodGet, base+"/track-record", nil, &tr); code != http.StatusOK {
			t.Fatalf("track record: %d", code)
		}
		return tr
	}

	var clean answer
	if code := call(http.MethodPost, base+"/reports", map[string]string{"series": scan.RehearsalSeries}, &clean); code != http.StatusCreated {
		t.Fatalf("rehearse: %d", code)
	}
	q := clean.Report
	if q.Status != scan.ReportPending || len(scan.Check(q.Draft)) != 0 || len(q.Draft.Sources) == 0 || len(q.Draft.Box.Data) == 0 ||
		q.Draft.Box.Conflicts != "Talyvor holds no position in the oil traded." || q.Draft.Box.Backtest == "" {
		t.Fatalf("the clean draft: %+v", q)
	}

	var planted answer
	if code := call(http.MethodPost, base+"/reports", map[string]string{"series": scan.RehearsalSeries, "plant": "Buy Cushing crude now."},
		&planted); code != http.StatusCreated {
		t.Fatalf("rehearse planted: %d", code)
	}
	if planted.Report.Status != scan.ReportRefused || !strings.Contains(planted.Report.Refusal, "advice or trading wording: Buy") {
		t.Fatalf("the planted draft: %+v", planted.Report)
	}
	if code := call(http.MethodPost, fmt.Sprintf("%s/reports/%d/approve", base, planted.Report.ID), map[string]string{}, nil); code != http.StatusConflict {
		t.Fatalf("a refused draft approved: %d", code)
	}
	if tr := track(); len(tr.Records) != 0 {
		t.Fatalf("on the track record before approval: %+v", tr)
	}

	var approved answer
	if code := call(http.MethodPost, fmt.Sprintf("%s/reports/%d/approve", base, q.ID), map[string]string{"note": "checked"}, &approved); code != http.StatusOK ||
		approved.Report.Status != scan.ReportApproved || approved.Report.ReviewedBy != scan.RehearsalReviewer {
		t.Fatalf("approve: %d %+v", code, approved.Report)
	}
	tr := track()
	if len(tr.Records) != 1 || tr.Records[0].ReportID != q.ID || tr.Records[0].Hit != nil || tr.Waiting != 1 {
		t.Fatalf("after approval: %+v", tr)
	}
	e := tr.Records[0]
	mid := (e.OutlookLow + e.OutlookHigh) / 2
	var scored struct {
		Scored int `json:"scored"`
	}
	if code := call(http.MethodPost, fmt.Sprintf("%s/reports/%d/outcome", base, q.ID), map[string]float64{"value": mid}, &scored); code != http.StatusOK || scored.Scored != 1 {
		t.Fatalf("outcome: %d %+v", code, scored)
	}
	if tr := track(); tr.Hits != 1 || tr.Records[0].Hit == nil || !*tr.Records[0].Hit || *tr.Records[0].Outcome != mid {
		t.Fatalf("after the outcome: %+v", tr)
	}
	if code := call(http.MethodPost, fmt.Sprintf("%s/reports/%d/outcome", base, q.ID), map[string]float64{"value": e.OutlookHigh + 1e6}, nil); code != http.StatusConflict {
		t.Fatalf("scored twice: %d", code)
	}

	// Nothing public, and the recorded weeks were rolled back.
	if pub, err := writer.Published(ctx, ""); err != nil || len(pub) != 0 {
		t.Fatalf("public library: %+v, %v", pub, err)
	}
	if pub, err := writer.TrackRecord(ctx); err != nil || len(pub.Records) != 0 {
		t.Fatalf("public track record: %+v, %v", pub, err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM scan_points WHERE starts_with(series, $1)) +
		(SELECT count(*) FROM scan_backtests) + (SELECT count(*) FROM scan_reports)`, scan.RehearsalSeries).Scan(&left); err != nil || left != 0 {
		t.Fatalf("rows left in Scan's own tables: %d, %v", left, err)
	}

	before := 0
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM scan_rehearsals`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if code := call(http.MethodPost, "/v1/synthetic/workspaces/"+real+"/scan/reports", map[string]string{"series": scan.RehearsalSeries}, nil); code != http.StatusForbidden {
		t.Fatalf("a real workspace rehearsed: %d", code)
	}
	if code := call(http.MethodPost, fmt.Sprintf("/v1/synthetic/workspaces/%s/scan/reports/%d/approve", real, q.ID), map[string]string{}, nil); code != http.StatusForbidden {
		t.Fatalf("a real workspace approved: %d", code)
	}
	after := 0
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM scan_rehearsals`).Scan(&after); err != nil || after != before {
		t.Fatalf("a refused call wrote: %d → %d, %v", before, after, err)
	}
}
