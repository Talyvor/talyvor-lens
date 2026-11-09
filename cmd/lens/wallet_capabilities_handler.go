package main

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B22.1 — every wallet capability carries its class, and real money obeys it (economy/wallet_capabilities.go).
//
//	GET /v1/wallets/capabilities   each capability, its class (GREEN, AMBER, RED) and whether it takes real
//	                               money now — GREEN always, AMBER and RED only while the operator's clearance
//	                               is in force (`lens wallet-clearances`); and label, live or sandbox for the
//	                               caller's workspace (B56.7), on each capability that moves money
//
// Mounted in the authed group, for the screens; any credential reads it.

type walletCapabilityReader interface {
	WalletCapabilities(ctx context.Context) ([]economy.CapabilityStatus, error)
	WorkspaceLabels(ctx context.Context, workspaceID string) (map[string]string, error)
}

// labelledCapability is a capability with its label for the caller's workspace.
type labelledCapability struct {
	economy.CapabilityStatus
	Label string `json:"label,omitempty"`
}

func mountWalletCapabilityRoutes(r chi.Router, store walletCapabilityReader) {
	r.Get("/v1/wallets/capabilities", func(w http.ResponseWriter, req *http.Request) {
		caps, err := store.WalletCapabilities(req.Context())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		ws, _ := auth.WorkspaceIdentity(req.Context())
		labels, err := store.WorkspaceLabels(req.Context(), ws)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := make([]labelledCapability, 0, len(caps))
		for _, c := range caps {
			out = append(out, labelledCapability{CapabilityStatus: c, Label: labels[c.Key]})
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"capabilities": out})
	})
}
