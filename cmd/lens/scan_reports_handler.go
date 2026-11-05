package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/scan"
)

// scan_reports_handler.go — B52.25: SCAN'S REVIEW QUEUE.
//
//	GET  /v1/admin/scan/reports?status=   the reports the writer drafted, newest first: pending (waiting for a person),
//	                                      refused (by the code rules or the second model, with why), approved (published)
//	                                      or rejected; every one when status is empty
//	POST /v1/admin/scan/reports/{id}/approve   a person publishes a pending report; requireStepUp names them
//	POST /v1/admin/scan/reports/{id}/reject    a person stops it ever being published

// scanReportQueue is what the queue reads and decides: *scan.Writer.
type scanReportQueue interface {
	Queue(ctx context.Context, status string) ([]scan.QueuedReport, error)
	Decide(ctx context.Context, id int64, approve bool, by, note string) (scan.QueuedReport, error)
}

func newScanReportsHandler(q scanReportQueue) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		status := req.URL.Query().Get("status")
		switch status {
		case "", scan.ReportPending, scan.ReportRefused, scan.ReportApproved, scan.ReportRejected:
		default:
			writeJSONErr(w, http.StatusBadRequest, "status is pending, refused, approved or rejected")
			return
		}
		reports, err := q.Queue(req.Context(), status)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"reports": reports})
	})
}

func newScanReportDecideHandler(q scanReportQueue, approve bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
		if err != nil {
			writeJSONErr(w, http.StatusNotFound, scan.ErrReportNotFound.Error())
			return
		}
		var in struct {
			Note string `json:"note"`
		}
		if req.ContentLength != 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
		}
		r, err := q.Decide(req.Context(), id, approve, strings.TrimSpace(req.Header.Get(moderatorOperatorHeader)), in.Note)
		switch {
		case errors.Is(err, scan.ErrReportNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, scan.ErrReportDecided):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, scan.ErrNoReviewer):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"report": r})
		}
	})
}

// GET /v1/public/scan/track-record — B52.26: every published report's observations and range, scored once the
// outcome is known: hits, misses and those still waiting. Public, like the reports it scores.
func newScanTrackRecordHandler(tr interface {
	TrackRecord(ctx context.Context) (scan.TrackRecord, error)
}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec, err := tr.TrackRecord(req.Context())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "the track record could not be read")
			return
		}
		writeJSONOK(w, http.StatusOK, rec)
	})
}

// scanPublishedReports is what the public library reads: *scan.Writer.
type scanPublishedReports interface {
	Published(ctx context.Context, family string) ([]scan.PublishedReport, error)
	PublishedReport(ctx context.Context, id int64) (scan.PublishedReport, error)
}

// GET /v1/public/scan/reports?family= — B52.33: every published (approved) report, newest first, for the app's
// report library; energy, oil, crops or filings only when family is set. Public, like the track record.
func newScanPublishedReportsHandler(p scanPublishedReports) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		family := req.URL.Query().Get("family")
		if _, ok := scan.ReportFamilies[family]; family != "" && !ok {
			writeJSONErr(w, http.StatusBadRequest, "family is energy, oil, crops or filings")
			return
		}
		reports, err := p.Published(req.Context(), family)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "the reports could not be read")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"reports": reports})
	})
}

// GET /v1/public/scan/reports/{id} — B52.33: one published report; 404 for any report not approved.
func newScanPublishedReportHandler(p scanPublishedReports) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
		if err != nil {
			writeJSONErr(w, http.StatusNotFound, scan.ErrReportNotFound.Error())
			return
		}
		r, err := p.PublishedReport(req.Context(), id)
		switch {
		case errors.Is(err, scan.ErrReportNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, "the report could not be read")
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"report": r})
		}
	})
}
