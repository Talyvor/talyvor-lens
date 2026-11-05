package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/talyvor/lens/internal/scan"
)

// scan_jets_handler.go — B52.30: KEEPING AN AIRCRAFT OUT OF SCAN'S COMPANY JETS.
//
//	POST /v1/admin/scan/jets/exclusions   {"registration": "N123AB", "reason": "erasure"}: the aircraft is never
//	                                      counted again, and every stored figure it was part of is deleted. Reasons:
//	                                      erasure (a data subject's request), single-user, ladd, pia.

type scanJetExcluder interface {
	ExcludeAircraft(ctx context.Context, registration, reason string) (int64, error)
}

func newScanJetExclusionHandler(store scanJetExcluder) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Registration string `json:"registration"`
			Reason       string `json:"reason"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096)).Decode(&body); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {\"registration\", \"reason\"}")
			return
		}
		n, err := store.ExcludeAircraft(req.Context(), body.Registration, body.Reason)
		switch {
		case errors.Is(err, scan.ErrExclusionReason):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"excluded": true, "figures_deleted": n})
		}
	})
}
