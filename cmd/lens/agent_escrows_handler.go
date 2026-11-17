package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B22.6 — escrow: money held until the deal is done (economy/agent_escrows.go).
//
//	POST /v1/workspaces/{wsID}/agents/{agentID}/escrows      {to, amount_ulxc, release_at, memo} — pay into escrow for
//	                                                         any agent; released to it on confirmation or at release_at
//	GET  /v1/workspaces/{wsID}/escrows                       what its agents paid into escrow and are owed from it
//	GET  /v1/workspaces/{wsID}/escrows/{escrowID}
//	POST /v1/workspaces/{wsID}/escrows/{escrowID}/confirm    the payer's side: delivered — release it to the payee
//	POST /v1/workspaces/{wsID}/escrows/{escrowID}/dispute    {reason} — the payer's side, before the deadline: hold it
//	                                                         for the operator to decide
//
// Paying in, confirming and disputing take the paying agent's own key, the workspace's owner or an admin.
// Deadlines release on the agent schedules' tick; the operator decides a dispute with `lens escrows`.
// Mounted in the authed group.

type agentEscrowBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	PayIntoEscrow(ctx context.Context, workspaceID, payerAgentID, payeeAddress string, amount int64, memo string, releaseAt time.Time) (economy.Escrow, error)
	ConfirmEscrow(ctx context.Context, workspaceID, escrowID string) (economy.Escrow, error)
	DisputeEscrow(ctx context.Context, workspaceID, escrowID, reason string) (economy.Escrow, error)
	GetEscrow(ctx context.Context, workspaceID, escrowID string) (economy.Escrow, error)
	ListEscrows(ctx context.Context, workspaceID string) ([]economy.Escrow, error)
}

func mountAgentEscrowRoutes(r chi.Router, bank agentEscrowBank) {
	// payerOrOwner lets the paying agent's own key, the workspace's owner or an admin act for it.
	payerOrOwner := func(w http.ResponseWriter, req *http.Request, agentID string) bool {
		if _, owner := storedanswers.OwnerOrAdmin(req.Context()); owner {
			return true
		}
		if actx := auth.GetAuthContext(req.Context()); actx != nil && actx.APIKeyID != "" {
			if keyAgent, _, _ := bank.AgentOfKey(req.Context(), actx.APIKeyID); keyAgent == agentID {
				return true
			}
		}
		writeJSONErr(w, http.StatusForbidden, "only the paying agent's own key, the workspace's owner or an admin may move its escrow")
		return false
	}
	r.With(withTask).Post("/v1/workspaces/{wsID}/agents/{agentID}/escrows", func(w http.ResponseWriter, req *http.Request) {
		agentID := chi.URLParam(req, "agentID")
		if !payerOrOwner(w, req, agentID) {
			return
		}
		var in struct {
			To         string    `json:"to"`
			AmountULXC int64     `json:"amount_ulxc"`
			ReleaseAt  time.Time `json:"release_at"`
			Memo       string    `json:"memo"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.To == "" || in.ReleaseAt.IsZero() {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"to": "<wallet id or @handle>", "amount_ulxc": …, "release_at": "<RFC 3339 time>", "memo": "<optional>"}`)
			return
		}
		e, err := bank.PayIntoEscrow(req.Context(), chi.URLParam(req, "wsID"), agentID, in.To, in.AmountULXC, in.Memo, in.ReleaseAt)
		if err != nil {
			writeEscrowErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, e)
	})
	r.Get("/v1/workspaces/{wsID}/escrows", func(w http.ResponseWriter, req *http.Request) {
		list, err := bank.ListEscrows(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"escrows": list})
	})
	r.Get("/v1/workspaces/{wsID}/escrows/{escrowID}", func(w http.ResponseWriter, req *http.Request) {
		e, err := bank.GetEscrow(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "escrowID"))
		if err != nil {
			writeEscrowErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, e)
	})
	// payerAct runs act for the escrow's paying side, once the caller may act for its paying agent.
	payerAct := func(act func(req *http.Request, wsID, escrowID string) (economy.Escrow, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			wsID, escrowID := chi.URLParam(req, "wsID"), chi.URLParam(req, "escrowID")
			e, err := bank.GetEscrow(req.Context(), wsID, escrowID)
			if err == nil && e.PayerWorkspaceID != wsID {
				err = economy.ErrEscrowNotFound // the payee's side cannot confirm or dispute
			}
			if err != nil {
				writeEscrowErr(w, err)
				return
			}
			if !payerOrOwner(w, req, e.PayerAgentID) {
				return
			}
			if e, err = act(req, wsID, escrowID); err != nil {
				writeEscrowErr(w, err)
				return
			}
			writeJSONOK(w, http.StatusOK, e)
		}
	}
	r.Post("/v1/workspaces/{wsID}/escrows/{escrowID}/confirm", payerAct(func(req *http.Request, wsID, escrowID string) (economy.Escrow, error) {
		return bank.ConfirmEscrow(req.Context(), wsID, escrowID)
	}))
	r.Post("/v1/workspaces/{wsID}/escrows/{escrowID}/dispute", payerAct(func(req *http.Request, wsID, escrowID string) (economy.Escrow, error) {
		var in struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in) // a missing reason is refused by the store, naming what it needs
		return bank.DisputeEscrow(req.Context(), wsID, escrowID, in.Reason)
	}))
}

func writeEscrowErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrEscrowNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, economy.ErrEscrowTerms):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	default:
		writeTransferErr(w, err)
	}
}
