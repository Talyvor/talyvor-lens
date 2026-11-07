package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/scan"
)

// B52.40 — A SCAN REPORT REHEARSED ON A TEST WORKSPACE, FOR THE NIGHTLY.
//
// Scan's writer, review queue and scorer otherwise run only as Lens's hourly jobs on live feeds, behind the admin
// routes. For a TEST (synthetic) workspace only, with the synthetic key, the nightly drives one report through them on
// Lens's recorded data, kept for that workspace alone (internal/scan/rehearsal.go) and never under /v1/public/scan/*:
//
//	POST /v1/synthetic/workspaces/{wsID}/scan/reports                {"series","plant"} → 201 {"report"}: the real
//	                                                                    writer on the recorded weeks, plant appended to
//	                                                                    "why it may matter"; pending, or refused
//	POST /v1/synthetic/workspaces/{wsID}/scan/reports/{id}/approve    {"note"} → {"report"}: the test reviewer's
//	                                                                    approval; 409 unless pending
//	POST /v1/synthetic/workspaces/{wsID}/scan/reports/{id}/outcome    {"value"} → {"scored"}: the planted outcome,
//	                                                                    scored; 409 unless approved and not yet scored
//	GET  /v1/synthetic/workspaces/{wsID}/scan/track-record            the workspace's approved rehearsed reports

// syntheticScan is *scan.Writer.
type syntheticScan interface {
	Rehearse(ctx context.Context, ws, series, plant string) (scan.QueuedReport, error)
	ApproveRehearsal(ctx context.Context, ws string, id int64, note string) (scan.QueuedReport, error)
	PlantRehearsalOutcome(ctx context.Context, ws string, id int64, value float64) (int, error)
	RehearsalTrackRecord(ctx context.Context, ws string) (scan.TrackRecord, error)
}

func (d syntheticDueDeps) scanRehearse(w http.ResponseWriter, r *http.Request, ws string) (int, string) {
	var in struct {
		Series string `json:"series"`
		Plant  string `json:"plant"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return 0, "error: bad request"
	}
	q, err := d.scan.Rehearse(r.Context(), ws, in.Series, in.Plant)
	switch {
	case errors.Is(err, scan.ErrNoRecordedData):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
		return 0, "refused: no recorded data"
	case errors.Is(err, scan.ErrNoConflicts):
		writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
		return 0, "refused: no conflicts statement"
	case errors.Is(err, scan.ErrModelRefused):
		writeJSONErr(w, http.StatusConflict, err.Error())
		return 0, "refused: model failed"
	case err != nil:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	writeJSONOK(w, http.StatusCreated, map[string]any{"report": q})
	return 1, "ok"
}

func (d syntheticDueDeps) scanApprove(w http.ResponseWriter, r *http.Request, ws string) (int, string) {
	id, ok := rehearsalID(w, r)
	if !ok {
		return 0, "refused: no such report"
	}
	var in struct {
		Note string `json:"note"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return 0, "error: bad request"
		}
	}
	q, err := d.scan.ApproveRehearsal(r.Context(), ws, id, in.Note)
	if n, outcome, done := rehearsalErr(w, err); done {
		return n, outcome
	}
	writeJSONOK(w, http.StatusOK, map[string]any{"report": q})
	return 1, "ok"
}

func (d syntheticDueDeps) scanOutcome(w http.ResponseWriter, r *http.Request, ws string) (int, string) {
	id, ok := rehearsalID(w, r)
	if !ok {
		return 0, "refused: no such report"
	}
	var in struct {
		Value *float64 `json:"value"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil || in.Value == nil {
		writeJSONErr(w, http.StatusBadRequest, "the body is {\"value\": the outcome, in the report's unit}")
		return 0, "error: bad request"
	}
	n, err := d.scan.PlantRehearsalOutcome(r.Context(), ws, id, *in.Value)
	if n, outcome, done := rehearsalErr(w, err); done {
		return n, outcome
	}
	writeJSONOK(w, http.StatusOK, map[string]any{"scored": n})
	return 1, "ok"
}

func (d syntheticDueDeps) scanTrackRecord(w http.ResponseWriter, r *http.Request, ws string) (int, string) {
	rec, err := d.scan.RehearsalTrackRecord(r.Context(), ws)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	writeJSONOK(w, http.StatusOK, rec)
	return 1, "ok"
}

func rehearsalID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeJSONErr(w, http.StatusNotFound, scan.ErrReportNotFound.Error())
		return 0, false
	}
	return id, true
}

// rehearsalErr answers a review or outcome's error; done is false when there was none.
func rehearsalErr(w http.ResponseWriter, err error) (int, string, bool) {
	switch {
	case err == nil:
		return 0, "", false
	case errors.Is(err, scan.ErrReportNotFound):
		writeJSONErr(w, http.StatusNotFound, "this test workspace has no rehearsed report with that id")
		return 0, "refused: no such report", true
	case errors.Is(err, scan.ErrReportDecided):
		writeJSONErr(w, http.StatusConflict, "the rehearsed report is not in the state this step needs: approve a pending one, score an approved one once")
		return 0, "refused: wrong state", true
	}
	writeJSONErr(w, http.StatusInternalServerError, err.Error())
	return 0, "error: " + err.Error(), true
}
