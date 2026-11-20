package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/outputverify"
)

// mechanicalRecorder is the ownership-bound write seam (*outputverify.MechanicalWriter satisfies it).
type mechanicalRecorder interface {
	RecordMechanicalIfOwned(ctx context.Context, r outputverify.MechanicalReport) (owned, recorded bool, err error)
}

type mechanicalReportBody struct {
	Verdict  string `json:"verdict"`
	ExitCode int    `json:"exit_code"`
	Tool     string `json:"tool"`
	Reason   string `json:"reason"`
}

// newMechanicalVerdictHandler serves POST /v1/output-verdicts/{output_id}/mechanical — the PRODUCING
// workspace self-reports a mechanical verdict (compiled/compile_failed/tests_passed/tests_failed) for an
// output it produced. Authenticated; a caller with no workspace → 401. OWNERSHIP-BOUND: the write records
// ONLY if the caller owns output_id (it appears in k4_output_verdicts with the caller's workspace_id) — a
// workspace can never report on another workspace's output (→ 403). Append-only: first report wins.
//
// TRUST MODEL (stored as verdict_source='self_reported'): a self-reported FAILURE is credible against
// interest (usable as H5 slash evidence); a self-reported PASS proves nothing and must never release a bond.
func newMechanicalVerdictHandler(authn verdictAuthenticator, rec mechanicalRecorder) http.HandlerFunc {
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
		var b mechanicalReportBody
		if err := json.NewDecoder(req.Body).Decode(&b); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid body")
			return
		}
		switch b.Verdict {
		case outputverify.MechCompiled, outputverify.MechCompileFailed, outputverify.MechTestsPassed, outputverify.MechTestsFailed:
		default:
			writeJSONErr(w, http.StatusBadRequest, "invalid verdict")
			return
		}
		owned, recorded, err := rec.RecordMechanicalIfOwned(req.Context(), outputverify.MechanicalReport{
			OutputID: outputID, WorkspaceID: ac.WorkspaceID,
			Verdict: b.Verdict, ExitCode: b.ExitCode, Tool: b.Tool, Reason: b.Reason,
		})
		if err != nil {
			// Never echo the internal error to the caller — a DB/schema detail could leak. Log server-side,
			// return a generic message.
			slog.Warn("mechanical verdict record failed", slog.String("workspace", ac.WorkspaceID), slog.String("err", err.Error()))
			writeJSONErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !owned {
			// Not the producer (or the K4 ownership row isn't present) — refuse.
			writeJSONErr(w, http.StatusForbidden, "not the producer of this output")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"recorded": recorded, "output_id": outputID})
	}
}
