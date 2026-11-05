package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/scan"
)

type fakeScanQueue struct{ by string }

func (f *fakeScanQueue) Queue(context.Context, string) ([]scan.QueuedReport, error) {
	return []scan.QueuedReport{{ID: 7, Status: scan.ReportPending}}, nil
}

func (f *fakeScanQueue) Decide(_ context.Context, id int64, approve bool, by, _ string) (scan.QueuedReport, error) {
	f.by = by
	if id != 7 {
		return scan.QueuedReport{}, scan.ErrReportNotFound
	}
	return scan.QueuedReport{ID: 7, Status: scan.ReportApproved, ReviewedBy: by}, nil
}

// The queue lists the reports and the approval names the operator who took it.
func TestScanReviewQueue(t *testing.T) {
	q := &fakeScanQueue{}
	r := chi.NewRouter()
	r.Get("/q", newScanReportsHandler(q).ServeHTTP)
	r.Post("/q/{id}/approve", newScanReportDecideHandler(q, true).ServeHTTP)
	do := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set(moderatorOperatorHeader, "nicolai")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	if rec := do(http.MethodGet, "/q?status=pending"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":7`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do(http.MethodGet, "/q?status=bogus"); rec.Code != http.StatusBadRequest {
		t.Fatalf("%d", rec.Code)
	}
	if rec := do(http.MethodPost, "/q/7/approve"); rec.Code != http.StatusOK || q.by != "nicolai" {
		t.Fatalf("%d %s by %q", rec.Code, rec.Body, q.by)
	}
	if rec := do(http.MethodPost, "/q/8/approve"); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
}
