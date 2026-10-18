package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/ratelimit"
)

// rate_limits_handler.go — B39.9: the operator raises (or lowers) one workspace's request limits. Every other
// workspace keeps ratelimit.DefaultRules().

func rateLimitsBody(r ratelimit.RateRule, custom bool) map[string]any {
	return map[string]any{
		"workspace_id":        r.WorkspaceID,
		"custom":              custom,
		"requests_per_second": r.RequestsPerSecond,
		"requests_per_minute": r.RequestsPerMinute,
		"requests_per_hour":   r.RequestsPerHour,
	}
}

// defaultRuleFor is the limits a workspace with no row of its own is held to.
func defaultRuleFor(wsID string) ratelimit.RateRule {
	r := ratelimit.DefaultRules()[0]
	r.WorkspaceID = wsID
	return r
}

// newRateLimitsGetHandler — GET /v1/admin/workspaces/{wsID}/rate-limits: the limits the workspace is held to.
func newRateLimitsGetHandler(s *ratelimit.WorkspaceLimits) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		r, ok, err := s.Get(req.Context(), wsID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			r = defaultRuleFor(wsID)
		}
		writeJSONOK(w, http.StatusOK, rateLimitsBody(r, ok))
	})
}

// newRateLimitsPutHandler — PUT /v1/admin/workspaces/{wsID}/rate-limits {"requests_per_second",
// "requests_per_minute", "requests_per_hour"}: sets the workspace's own limits; an omitted one keeps its default.
func newRateLimitsPutHandler(s *ratelimit.WorkspaceLimits) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			PerSecond *int `json:"requests_per_second"`
			PerMinute *int `json:"requests_per_minute"`
			PerHour   *int `json:"requests_per_hour"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, 8<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		r := defaultRuleFor(chi.URLParam(req, "wsID"))
		for _, f := range []struct {
			in  *int
			out *int
		}{{in.PerSecond, &r.RequestsPerSecond}, {in.PerMinute, &r.RequestsPerMinute}, {in.PerHour, &r.RequestsPerHour}} {
			if f.in != nil {
				*f.out = *f.in
			}
		}
		if r.RequestsPerSecond > 1<<31-1 || r.RequestsPerMinute > 1<<31-1 || r.RequestsPerHour > 1<<31-1 {
			writeJSONErr(w, http.StatusBadRequest, "a limit is too large")
			return
		}
		if err := s.Set(req.Context(), r); err != nil {
			if errors.Is(err, ratelimit.ErrInvalidLimits) {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, rateLimitsBody(r, true))
	})
}

// newRateLimitsDeleteHandler — DELETE /v1/admin/workspaces/{wsID}/rate-limits: back on the default limits.
func newRateLimitsDeleteHandler(s *ratelimit.WorkspaceLimits) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		if err := s.Delete(req.Context(), wsID); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, rateLimitsBody(defaultRuleFor(wsID), false))
	})
}
