package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B59.14 — the Instant transfer quote, locked for 30 seconds (economy/instant_transfer_quote.go).
//
//	POST /v1/workspaces/{wsID}/money/instant-transfers/quote {send_minor, send_currency, receive_currency, to, country}
//	                          — you send, the fee, the rate, they get and when it arrives, good until expires_at
//
// The workspace's owner or an admin asks it; a workspace that is not hybrid is refused 409 wrong_money_kind. Mounted in
// the authed group.
//
// B59.15 — Instant transfer to a Talyvor account: one id, three legs, all or nothing (economy/instant_transfer_send.go).
//
//	POST /v1/workspaces/{wsID}/agents/{agentID}/money/instant-transfers {quote_id, to_account_id, funding} with an
//	                          Idempotency-Key header (or idempotency_key) — sends from the agent's account at the quote,
//	                          once: received, or reversed with the agent's money back; the same key again answers the same
//	GET  /v1/workspaces/{wsID}/money/instant-transfers/{id}
//	                          — the receipt: where it stands, both conversions and the on-chain reference
//
// A send takes the agent's own key, the workspace's owner or an admin; an agent without "Can use Instant transfer" is
// refused 403 with field may_instant_transfer, and one that needs the owner's approval files it and is refused 403.

type instantTransferBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	QuoteInstantTransfer(ctx context.Context, workspaceID string, in economy.InstantQuoteRequest) (economy.InstantQuote, error)
	SendInstantTransfer(ctx context.Context, workspaceID, agentID string, in economy.InstantSend) (economy.InstantTransfer, error)
	InstantTransfer(ctx context.Context, workspaceID, id string) (economy.InstantTransfer, error)
}

func mountInstantTransferRoutes(r chi.Router, bank instantTransferBank) {
	r.Post("/v1/workspaces/{wsID}/money/instant-transfers/quote", func(w http.ResponseWriter, req *http.Request) {
		if _, owner := storedanswers.OwnerOrAdmin(req.Context()); !owner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may quote an Instant transfer")
			return
		}
		var in economy.InstantQuoteRequest
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"send_minor", "send_currency", "receive_currency", "to", "country"}`)
			return
		}
		q, err := bank.QuoteInstantTransfer(req.Context(), chi.URLParam(req, "wsID"), in)
		switch {
		case err == nil:
			writeJSONOK(w, http.StatusCreated, q)
		case writeMoneyKindRefusal(w, err):
		case errors.Is(err, economy.ErrInstantTransfer), errors.Is(err, money.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		default:
			writePaymentErr(w, err)
		}
	})
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/money/instant-transfers", func(w http.ResponseWriter, req *http.Request) {
		agentID := chi.URLParam(req, "agentID")
		if _, owner := storedanswers.OwnerOrAdmin(req.Context()); !owner {
			actx := auth.GetAuthContext(req.Context())
			keyAgent := ""
			if actx != nil && actx.APIKeyID != "" {
				keyAgent, _, _ = bank.AgentOfKey(req.Context(), actx.APIKeyID)
			}
			if keyAgent != agentID {
				writeJSONErr(w, http.StatusForbidden, "only the agent's own key, the workspace's owner or an admin may send its money")
				return
			}
		}
		var in economy.InstantSend
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.QuoteID == "" || in.ToAccountID == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"quote_id", "to_account_id", "funding"}: POST …/money/instant-transfers/quote first`)
			return
		}
		if key := req.Header.Get("Idempotency-Key"); key != "" {
			in.IdempotencyKey = key
		}
		t, err := bank.SendInstantTransfer(req.Context(), chi.URLParam(req, "wsID"), agentID, in)
		var rule *economy.InstantRuleRefusal
		switch {
		case err == nil:
			writeJSONOK(w, http.StatusOK, t)
		case errors.As(err, &rule):
			writeJSONOK(w, http.StatusForbidden, map[string]any{"error": api.Refusal(rule.Error()), "code": "agent_rule", "field": rule.Field})
		case writeMoneyKindRefusal(w, err):
		case errors.Is(err, economy.ErrInstantTransfer), errors.Is(err, money.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		default:
			writePaymentErr(w, err)
		}
	})
	r.Get("/v1/workspaces/{wsID}/money/instant-transfers/{id}", func(w http.ResponseWriter, req *http.Request) {
		t, err := bank.InstantTransfer(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "id"))
		if err == nil {
			if _, owner := storedanswers.OwnerOrAdmin(req.Context()); !owner {
				actx := auth.GetAuthContext(req.Context())
				keyAgent := ""
				if actx != nil && actx.APIKeyID != "" {
					keyAgent, _, _ = bank.AgentOfKey(req.Context(), actx.APIKeyID)
				}
				if keyAgent != t.AgentID {
					writeJSONErr(w, http.StatusForbidden, "only the agent that sent it, the workspace's owner or an admin may read an Instant transfer")
					return
				}
			}
		}
		if err != nil {
			writePaymentErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, t)
	})
}
