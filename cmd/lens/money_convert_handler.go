package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B56.23 — an agent converts between its currency accounts at its plan's FX margin (economy/money_conversions.go).
//
//	POST /v1/workspaces/{wsID}/agents/{agentID}/money/convert/quote {sell_currency, buy_currency, sell_minor, funding}
//	                          — the reference rate, the margin, the rate and what it buys, good until expires_at
//	POST /v1/workspaces/{wsID}/agents/{agentID}/money/convert       {quote_id} with an Idempotency-Key header (or
//	                          idempotency_key) — converts at the quote, once: the same key again answers the same
//
// Each takes the agent's own key, the workspace's owner or an admin. Mounted in the authed group.

type moneyConvertBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	QuoteConversion(ctx context.Context, workspaceID, agentID string, in economy.Conversion) (economy.Conversion, error)
	Convert(ctx context.Context, workspaceID, agentID, quoteID, key string) (economy.Conversion, error)
}

func mountMoneyConvertRoutes(r chi.Router, bank moneyConvertBank) {
	// agentOrOwner lets the agent's own key, the workspace's owner or an admin convert the agent's money.
	agentOrOwner := func(next func(w http.ResponseWriter, req *http.Request, ws, agentID string)) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			agentID := chi.URLParam(req, "agentID")
			if _, owner := storedanswers.OwnerOrAdmin(req.Context()); !owner {
				actx := auth.GetAuthContext(req.Context())
				keyAgent := ""
				if actx != nil && actx.APIKeyID != "" {
					keyAgent, _, _ = bank.AgentOfKey(req.Context(), actx.APIKeyID)
				}
				if keyAgent != agentID {
					writeJSONErr(w, http.StatusForbidden, "only the agent's own key, the workspace's owner or an admin may convert its money")
					return
				}
			}
			next(w, req, chi.URLParam(req, "wsID"), agentID)
		}
	}
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/money/convert/quote", agentOrOwner(func(w http.ResponseWriter, req *http.Request, ws, agentID string) {
		var in economy.Conversion
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"sell_currency", "buy_currency", "sell_minor", "funding"}`)
			return
		}
		q, err := bank.QuoteConversion(req.Context(), ws, agentID, economy.Conversion{SellCurrency: in.SellCurrency,
			BuyCurrency: in.BuyCurrency, SellMinor: in.SellMinor, Funding: in.Funding})
		if err != nil {
			writeConvertErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, q)
	}))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/money/convert", agentOrOwner(func(w http.ResponseWriter, req *http.Request, ws, agentID string) {
		var in struct {
			QuoteID        string `json:"quote_id"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.QuoteID == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"quote_id"}: POST …/money/convert/quote first`)
			return
		}
		key := req.Header.Get("Idempotency-Key")
		if key == "" {
			key = in.IdempotencyKey
		}
		c, err := bank.Convert(req.Context(), ws, agentID, in.QuoteID, key)
		if err != nil {
			writeConvertErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, c)
	}))
}

func writeConvertErr(w http.ResponseWriter, err error) {
	if errors.Is(err, economy.ErrConversion) {
		writeJSONErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writePaymentErr(w, err)
}
