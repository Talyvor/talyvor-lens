package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/poolroyalty"
)

// adjudicateAuthenticator is the minimal auth surface the handler needs —
// *auth.Manager satisfies it (Authenticate). Kept as an interface so the
// handler is testable with a fake (httptest), mirroring how ApproveRate
// authenticates via authManager.Authenticate.
type adjudicateAuthenticator interface {
	Authenticate(*http.Request) (*auth.AuthContext, error)
}

// adjudicator is the gate's write surface — *poolroyalty.AdjudicationWriter
// satisfies it. The handler calls Adjudicate, which writes the audit record
// BEFORE the revoke (record-before-burn) — so the only production path to a
// held-burn runs through this admin-gated endpoint with an operator-chosen
// subset.
type adjudicator interface {
	Adjudicate(ctx context.Context, d poolroyalty.AdjudicationDecision) (string, poolroyalty.RevokeReport, error)
}

// adjudicateRequest is the operator's POST body. revoke_request_ids is the
// REQUIRED operator-chosen subset; the endpoint never re-runs the detector or
// auto-selects — the human selection is the structural gate.
type adjudicateRequest struct {
	FlagType            string   `json:"flag_type"`
	ResolutionLabel     string   `json:"resolution_label"`
	CandidateRequestIDs []string `json:"candidate_request_ids"`
	RevokeRequestIDs    []string `json:"revoke_request_ids"`
}

// newAdjudicateHandler builds the admin-gated adjudication endpoint. Mirrors the
// ApproveRate precedent: Authenticate → IsAdmin else 403; decided_by =
// AuthContext.UserID or 'global_key'; call the writer; return id + report.
func newAdjudicateHandler(am adjudicateAuthenticator, adj adjudicator) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		actx, err := am.Authenticate(req)
		if refuseAuthUnavailable(rw, err) {
			return
		}
		if err != nil || actx == nil || !actx.IsAdmin {
			writeJSONErr(rw, http.StatusForbidden, "admin credentials required")
			return
		}
		var body adjudicateRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSONErr(rw, http.StatusBadRequest, "invalid request body")
			return
		}
		if len(body.RevokeRequestIDs) == 0 {
			writeJSONErr(rw, http.StatusBadRequest, "revoke_request_ids must be a non-empty operator-chosen subset")
			return
		}
		decidedBy := actx.UserID
		if decidedBy == "" {
			decidedBy = "global_key"
		}
		id, report, err := adj.Adjudicate(req.Context(), poolroyalty.AdjudicationDecision{
			FlagType:            body.FlagType,
			ResolutionLabel:     body.ResolutionLabel,
			CandidateRequestIDs: body.CandidateRequestIDs,
			RevokeRequestIDs:    body.RevokeRequestIDs,
			DecidedBy:           decidedBy,
		})
		if err != nil {
			writeJSONErr(rw, http.StatusInternalServerError, err.Error())
			return
		}
		// Audit observability for a money-affecting admin action — and the
		// runtime record of the known decided_by identity gap (global_key when
		// no per-person UserID). Makes the "logged" identity limitation true at
		// runtime, not just durable on the row.
		slog.Info("pool-royalty adjudication",
			slog.String("adjudication_id", id),
			slog.String("decided_by", decidedBy),
			slog.String("flag_type", body.FlagType),
			slog.String("resolution_label", body.ResolutionLabel),
			slog.Int("reviewed", len(body.CandidateRequestIDs)),
			slog.Int("revoked_requested", len(body.RevokeRequestIDs)),
			slog.Int("revoked", report.Totals[poolroyalty.OutcomeRevoked]),
		)
		writeJSONOK(rw, http.StatusOK, map[string]any{
			"adjudication_id": id,
			"report":          report,
		})
	}
}
