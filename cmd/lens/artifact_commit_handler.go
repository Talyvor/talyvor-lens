package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/outputverify"
)

// artifactCommitter is the opt-in commit seam (*outputverify.ArtifactCommitter satisfies it).
type artifactCommitter interface {
	Commit(ctx context.Context, outputID, callerWorkspaceID, outputPath string, contextManifest []outputverify.ManifestEntry) (string, bool, error)
}

type artifactCommitBody struct {
	OutputPath      string                       `json:"output_path"`
	ContextManifest []outputverify.ManifestEntry `json:"context_manifest"`
}

// newArtifactCommitHandler serves POST /v1/outputs/{output_id}/artifact — the PRODUCING workspace opts in to
// commit the manifest hash of the buildable module it relies on, bound to the output. Authenticated; no
// workspace → 401. OWNERSHIP-BOUND: only the producer may commit (→ 403 otherwise). The committed output slot
// is folded from the output's stored output_content_sha256 (the canonical served content — the bytes a real
// buildable tree can carry) — a caller-supplied output hash is ignored — so the commitment binds what was
// served. An output with no content binding can never commit (→ 409, permanent). Append-once. Registered
// only when LENS_H5_ARTIFACT_ENABLED.
func newArtifactCommitHandler(authn verdictAuthenticator, c artifactCommitter) http.HandlerFunc {
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
		var b artifactCommitBody
		if err := json.NewDecoder(req.Body).Decode(&b); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid body")
			return
		}
		artifactSHA, committed, err := c.Commit(req.Context(), outputID, ac.WorkspaceID, b.OutputPath, b.ContextManifest)
		if err != nil {
			switch {
			case errors.Is(err, outputverify.ErrNoOutputPath):
				writeJSONErr(w, http.StatusBadRequest, "output_path required")
			case errors.Is(err, outputverify.ErrOutputNotFound):
				writeJSONErr(w, http.StatusNotFound, "output not found")
			case errors.Is(err, outputverify.ErrNotOutputOwner):
				writeJSONErr(w, http.StatusForbidden, "not the producer of this output")
			case errors.Is(err, outputverify.ErrNoContentBinding):
				// Permanent for this output: no canonical content was captured (pre-0098 row, extraction
				// failure, or a true stream) — nothing a buildable tree could ever match. Do not retry.
				writeJSONErr(w, http.StatusConflict, "output has no content binding; artifact commitment impossible")
			default:
				slog.Warn("artifact commit failed", slog.String("workspace", ac.WorkspaceID), slog.String("err", err.Error()))
				writeJSONErr(w, http.StatusInternalServerError, "internal error")
			}
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"artifact_sha256": artifactSHA, "committed": committed, "output_id": outputID})
	}
}
