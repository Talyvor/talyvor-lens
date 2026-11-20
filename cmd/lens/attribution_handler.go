package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/outputverify"
)

// attributionRecorder is the ownership-bound write seam (*outputverify.AttributionWriter satisfies it).
type attributionRecorder interface {
	RecordAttributionIfOwned(ctx context.Context, a outputverify.Attribution) (owned, recorded, conflict bool, err error)
}

type attributionBody struct {
	TargetKind string `json:"target_kind"`
	TargetRef  string `json:"target_ref"`
}

// newAttributionHandler serves POST /v1/outputs/{output_id}/attribution — the PRODUCING workspace
// attributes an output IT OWNS to a PR or spec. Authenticated (no workspace → 401). OWNERSHIP-BOUND:
// records ONLY if the caller owns output_id (present in k4_output_verdicts with the caller's
// workspace_id) — a workspace can never attribute another workspace's output (→ 403). Append-only,
// first-wins per (output_id, workspace_id, target_kind): an identical re-post is an idempotent no-op
// (200 recorded:false); a DIFFERENT target_ref for the same kind is refused (409). target_ref is an
// OPAQUE free string — never parsed, never dereferenced (no GitHub call). Attribution ≠ settlement:
// the handler and its store touch NO ledger/mint/held/supply row and NO amount.
func newAttributionHandler(authn verdictAuthenticator, rec attributionRecorder) http.HandlerFunc {
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
		var b attributionBody
		if err := json.NewDecoder(req.Body).Decode(&b); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid body")
			return
		}
		switch b.TargetKind {
		case outputverify.AttrKindPR, outputverify.AttrKindSpec:
		default:
			writeJSONErr(w, http.StatusBadRequest, "invalid target_kind")
			return
		}
		if b.TargetRef == "" {
			writeJSONErr(w, http.StatusBadRequest, "missing target_ref")
			return
		}
		owned, recorded, conflict, err := rec.RecordAttributionIfOwned(req.Context(), outputverify.Attribution{
			OutputID: outputID, WorkspaceID: ac.WorkspaceID, TargetKind: b.TargetKind, TargetRef: b.TargetRef,
		})
		if err != nil {
			// Never echo the internal error — a DB/schema detail could leak. Log server-side, return generic.
			slog.Warn("attribution record failed", slog.String("workspace", ac.WorkspaceID), slog.String("err", err.Error()))
			writeJSONErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !owned {
			writeJSONErr(w, http.StatusForbidden, "not the producer of this output")
			return
		}
		if conflict {
			// D1 append-only: a different target_ref is already recorded for this kind — first wins.
			writeJSONErr(w, http.StatusConflict, "already attributed for this kind")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"recorded": recorded, "output_id": outputID, "target_kind": b.TargetKind})
	}
}
