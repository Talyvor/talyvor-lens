package main

import (
	"context"
	"net/http"

	"github.com/talyvor/lens/internal/scan"
)

// scan_models_handler.go — B52.24: SCAN'S MODELS, AS THE OPERATOR SEES THEM.
//
//	GET /v1/admin/scan/models   every modelled series' latest backtest — the model written down, outlooks checked, how
//	                            often it called the side of the normal, its error against the normal's alone, how often
//	                            its 80% range held, passed or failed and why — with, when it passed, the latest point
//	                            against its normal and its outlook; and the series Scan keeps without a model, and why

// scanModelReports is what the page reads: *scan.Models.
type scanModelReports interface {
	Reports(ctx context.Context) ([]scan.Report, error)
}

func newScanModelsHandler(models scanModelReports) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		reports, err := models.Reports(req.Context())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"backtests": reports, "not_modelled": scan.Unmodelled})
	})
}
