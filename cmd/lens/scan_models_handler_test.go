package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/lens/internal/scan"
)

type fakeScanModels []scan.Report

func (f fakeScanModels) Reports(context.Context) ([]scan.Report, error) { return f, nil }

func TestScanModelsPageShowsEveryBacktest(t *testing.T) {
	h := newScanModelsHandler(fakeScanModels{
		{Series: "eia.stocks.cushing-crude", Passed: true, Verdict: "passed: …", Finding: &scan.Finding{Z: 2.1}},
		{Series: "entsog.flow.x", Verdict: "failed: it called the side of the normal 48% of the time, no better than a coin"},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/scan/models", nil))
	var body struct {
		Backtests   []scan.Report                  `json:"backtests"`
		NotModelled []struct{ Prefix, Why string } `json:"not_modelled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(body.Backtests) != 2 || body.Backtests[0].Finding.Z != 2.1 || body.Backtests[1].Passed ||
		len(body.NotModelled) != len(scan.Unmodelled) {
		t.Fatalf("%+v", body)
	}
}
