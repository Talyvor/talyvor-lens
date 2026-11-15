package main

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/attribution"
	"github.com/talyvor/lens/internal/outputverify"
)

// attributionReader is the read seam for the output-attribution read-back (*outputverify.AttributionReader
// satisfies it).
type attributionReader interface {
	GetByOutput(ctx context.Context, workspaceID, outputID string) ([]outputverify.AttributionRecord, error)
	ListByWorkspace(ctx context.Context, workspaceID string, limit int) ([]outputverify.AttributionRecord, error)
}

// newAttributionByOutputHandler serves GET /v1/outputs/{output_id}/attribution — the attributions of one
// output the caller OWNS (a single output may carry a pr AND a spec). Owner-scoped: a foreign output — or an
// owned output with no attribution — yields zero rows → 404 (never 403), so there is no cross-tenant oracle.
func newAttributionByOutputHandler(authn verdictAuthenticator, reader attributionReader) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ac, err := authn.Authenticate(req)
		if refuseAuthUnavailable(w, err) {
			return
		}
		if err != nil || ac == nil || ac.WorkspaceID == "" {
			writeJSONErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		outputID := chi.URLParam(req, "output_id")
		if outputID == "" {
			writeJSONErr(w, http.StatusBadRequest, "missing output_id")
			return
		}
		recs, err := reader.GetByOutput(req.Context(), ac.WorkspaceID, outputID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(recs) == 0 {
			writeJSONErr(w, http.StatusNotFound, "no attribution for this output")
			return
		}
		writeJSONOK(w, http.StatusOK, recs)
	}
}

// newAttributionListHandler serves GET /v1/attributions — the caller's OWN attributions (self-scoped via the
// authenticated WorkspaceID). Always a JSON array.
func newAttributionListHandler(authn verdictAuthenticator, reader attributionReader) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ac, err := authn.Authenticate(req)
		if refuseAuthUnavailable(w, err) {
			return
		}
		if err != nil || ac == nil || ac.WorkspaceID == "" {
			writeJSONErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
		recs, err := reader.ListByWorkspace(req.Context(), ac.WorkspaceID, limit)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if recs == nil {
			recs = []outputverify.AttributionRecord{}
		}
		writeJSONOK(w, http.StatusOK, recs)
	}
}

// newBranchStatsHandler serves GET /v1/workspaces/{wsID}/attribution/branches/{branch}. chi matches on the
// escaped path, so a branch with a slash ("feature/x", sent as feature%2Fx) arrives escaped and is unescaped
// here — read as sent, it named no branch and counted nothing.
func newBranchStatsHandler(get func(ctx context.Context, workspaceID, branch string, since time.Time) (*attribution.BranchStats, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		branch, err := url.PathUnescape(chi.URLParam(req, "branch"))
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid branch: "+err.Error())
			return
		}
		stats, err := get(req.Context(), chi.URLParam(req, "wsID"), branch, parseSinceParam(req.URL.Query().Get("since")))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, stats)
	}
}
