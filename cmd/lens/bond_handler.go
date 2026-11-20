package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/provenance"
)

// bondCreator / bondSettler are the H5 seams (*provenance.BondManager satisfies both).
type bondCreator interface {
	CreateBond(ctx context.Context, workspaceID, outputID string, amountUlens int64) (bondID string, created bool, err error)
}

type bondSettler interface {
	SettleBond(ctx context.Context, bondID string) (outcome string, err error)
}

// newBondCreateHandler serves POST /v1/bonds — the workspace stakes collateral on an output it produced.
// Authenticated; the bond is scoped to the caller's workspace (never another's). Locking happens via the
// integer-µLENS ledger; a bond on an output the caller does not own is refused (403). Registered ONLY when
// LENS_H5_BONDS_ENABLED.
func newBondCreateHandler(authn verdictAuthenticator, bc bondCreator) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ac, err := authn.Authenticate(req)
		if refuseAuthUnavailable(w, err) {
			return
		}
		if err != nil || ac == nil || ac.WorkspaceID == "" {
			writeJSONErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		var b struct {
			OutputID    string `json:"output_id"`
			AmountUlens int64  `json:"amount_ulens"`
		}
		if err := json.NewDecoder(req.Body).Decode(&b); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid body")
			return
		}
		if b.OutputID == "" || b.AmountUlens <= 0 {
			writeJSONErr(w, http.StatusBadRequest, "output_id and positive amount_ulens required")
			return
		}
		bondID, created, err := bc.CreateBond(req.Context(), ac.WorkspaceID, b.OutputID, b.AmountUlens)
		if errors.Is(err, provenance.ErrNotOwned) {
			writeJSONErr(w, http.StatusForbidden, "not the producer of this output")
			return
		}
		if err != nil {
			slog.Warn("bond create failed", slog.String("workspace", ac.WorkspaceID), slog.String("err", err.Error()))
			writeJSONErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"bond_id": bondID, "created": created})
	}
}

// newBondSettleHandler serves POST /v1/admin/bonds/{bond_id}/settle — an operator/cron finalizes a bond.
// SettleBond is deadline-guarded, CAS-safe and idempotent, so triggering it cannot cause a premature or
// double burn regardless of who calls it (admin-gated as defense in depth). Registered ONLY when
// LENS_H5_BONDS_ENABLED. (A background sweep over due bonds is the natural production trigger.)
func newBondSettleHandler(bs bondSettler) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		bondID := chi.URLParam(req, "bond_id")
		if bondID == "" {
			writeJSONErr(w, http.StatusBadRequest, "missing bond_id")
			return
		}
		outcome, err := bs.SettleBond(req.Context(), bondID)
		if err != nil {
			slog.Warn("bond settle failed", slog.String("bond_id", bondID), slog.String("err", err.Error()))
			writeJSONErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"bond_id": bondID, "outcome": outcome})
	}
}
