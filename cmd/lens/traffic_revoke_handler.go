package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/mining"
)

// traffic_revoke_handler.go — the admin manual-clawback endpoint for
// traffic_mint_holds (the cache/compute/embedding node mints). The generic
// poolroyalty adjudicate handler can't serve this table (composite key +
// workspace_id), so this is the parallel surface over mining.TrafficRevoker. It
// keeps the SAME record-before-revoke discipline: the audit row (shared
// held_mint_adjudications, 0091) is written BEFORE the burn, then completed with
// the RevokeReport — so a production revoke can never happen without a preceding
// audit record.

// trafficRevokeSurface is the revoke seam — *mining.TrafficRevoker satisfies it.
type trafficRevokeSurface interface {
	RevokeTrafficHolds(ctx context.Context, keys []mining.TrafficHoldKey) (mining.TrafficRevokeReport, error)
}

// trafficRevokeDB is the minimal audit-write seam (*pgxpool.Pool satisfies it).
type trafficRevokeDB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type trafficRevokeKey struct {
	RequestID   string `json:"request_id"`
	WorkspaceID string `json:"workspace_id"`
	MintType    string `json:"mint_type"`
}

// trafficRevokeRequest is the operator's POST body. revoke_keys is the REQUIRED
// operator-chosen subset (composite keys); the endpoint never auto-selects.
type trafficRevokeRequest struct {
	ResolutionLabel string             `json:"resolution_label"`
	RevokeKeys      []trafficRevokeKey `json:"revoke_keys"`
}

const trafficAuditInsertSQL = `INSERT INTO held_mint_adjudications
    (flag_type, resolution_label, candidate_request_ids, revoked_request_ids, decided_by)
VALUES ('manual:traffic_mint_holds', $1, $2, $2, $3)
RETURNING id`

const trafficAuditCompleteSQL = `UPDATE held_mint_adjudications SET outcome = $2 WHERE id = $1`

// newTrafficRevokeHandler builds the admin-gated traffic clawback endpoint.
// Mirrors newAdjudicateHandler + AdjudicationWriter.Adjudicate: Authenticate →
// IsAdmin else 403; record the decision FIRST; revoke exactly the chosen keys;
// complete the record with the report.
func newTrafficRevokeHandler(am adjudicateAuthenticator, db trafficRevokeDB, rev trafficRevokeSurface) http.HandlerFunc {
	return func(rw http.ResponseWriter, req *http.Request) {
		actx, err := am.Authenticate(req)
		if refuseAuthUnavailable(rw, err) {
			return
		}
		if err != nil || actx == nil || !actx.IsAdmin {
			writeJSONErr(rw, http.StatusForbidden, "admin credentials required")
			return
		}
		var body trafficRevokeRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSONErr(rw, http.StatusBadRequest, "invalid request body")
			return
		}
		if len(body.RevokeKeys) == 0 {
			writeJSONErr(rw, http.StatusBadRequest, "revoke_keys must be a non-empty operator-chosen subset")
			return
		}
		keys := make([]mining.TrafficHoldKey, 0, len(body.RevokeKeys))
		ids := make([]string, 0, len(body.RevokeKeys))
		for _, k := range body.RevokeKeys {
			if k.RequestID == "" || k.WorkspaceID == "" || k.MintType == "" {
				writeJSONErr(rw, http.StatusBadRequest, "each revoke_key needs request_id, workspace_id and mint_type")
				return
			}
			keys = append(keys, mining.TrafficHoldKey{RequestID: k.RequestID, WorkspaceID: k.WorkspaceID, MintType: k.MintType})
			ids = append(ids, k.RequestID+"|"+k.WorkspaceID+"|"+k.MintType)
		}
		decidedBy := actx.UserID
		if decidedBy == "" {
			decidedBy = "global_key"
		}

		// record-before-revoke: the audit row lands BEFORE any burn.
		var id string
		if err := db.QueryRow(req.Context(), trafficAuditInsertSQL, body.ResolutionLabel, ids, decidedBy).Scan(&id); err != nil {
			writeJSONErr(rw, http.StatusInternalServerError, "record adjudication: "+err.Error())
			return
		}
		report, err := rev.RevokeTrafficHolds(req.Context(), keys)
		if err != nil {
			writeJSONErr(rw, http.StatusInternalServerError, err.Error())
			return
		}
		// complete the record with the outcome (best-effort; the claim-row status is
		// the authoritative money truth if this UPDATE is lost).
		if outcome, mErr := json.Marshal(report); mErr == nil {
			if _, uErr := db.Exec(req.Context(), trafficAuditCompleteSQL, id, outcome); uErr != nil {
				slog.Warn("traffic revoke: audit completion failed (claim status is authoritative)", slog.String("id", id), slog.String("err", uErr.Error()))
			}
		}
		slog.Info("traffic mint clawback (admin)", slog.String("adjudication_id", id), slog.String("decided_by", decidedBy), slog.Int("keys", len(keys)))
		writeJSONOK(rw, http.StatusOK, map[string]any{"adjudication_id": id, "outcomes": report.Outcomes, "totals": report.Totals})
	}
}
