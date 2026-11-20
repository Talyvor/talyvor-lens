package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B59.51 — the owner's Wallets: withdraw to your own bank, and convert between the two sides (economy/money_wallets.go).
//
//	POST /v1/money/accounts/{id}/withdraw/challenge {payee_id, amount_minor, funding, idempotency_key} — the challenge
//	                          withdrawing that amount from the company account to the owner's own bank is signed over:
//	                          {challenge, allow_credentials}
//	POST /v1/money/accounts/{id}/withdraw {the same, "assertion"}, or the key as the Idempotency-Key header — pays it out
//	                          to a payee saved with own, in the account's currency, as a payment with the reference
//	                          Withdrawal (201); once the workspace has a passkey or device key, only signed (403)
//	POST /v1/money/convert/quote {sell_currency, buy_currency, sell_minor, funding} — prices converting between the
//	                          company's accounts on the two sides, a currency for USDC or USDC for a currency, good
//	                          until expires_at (201); outside a hybrid workspace, 409 wrong_money_kind
//	POST /v1/money/convert/challenge {quote_id, idempotency_key} — the challenge converting at it is signed over
//	POST /v1/money/convert    {quote_id, idempotency_key, "assertion"} — converts at the quote, once: the same key again
//	                          answers the same conversion
//
// Only the workspace's owner or an admin uses these; an agent's key is refused. Mounted in the authed group.

type moneyWalletBank interface {
	WithdrawalChallenge(ctx context.Context, workspaceID string, in economy.Withdrawal) (string, []string, error)
	WithdrawToOwnBank(ctx context.Context, workspaceID string, in economy.Withdrawal, a *passkey.Assertion) (economy.OutsidePayment, error)
	QuoteSideConversion(ctx context.Context, workspaceID string, in economy.Conversion) (economy.Conversion, error)
	SideConversionChallenge(ctx context.Context, workspaceID, quoteID, key string) (string, []string, error)
	ConvertSides(ctx context.Context, workspaceID, quoteID, key string, a *passkey.Assertion) (economy.Conversion, error)
}

// walletOwner is the key's workspace when its owner or an admin calls; otherwise it writes the refusal.
func walletOwner(w http.ResponseWriter, req *http.Request) (string, bool) {
	ws, _ := auth.WorkspaceIdentity(req.Context())
	if ws == "" {
		writeJSONErr(w, http.StatusBadRequest, "wallets are a workspace's: use a workspace's key")
		return "", false
	}
	if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
		writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may withdraw or convert its money")
		return "", false
	}
	return ws, true
}

func mountMoneyWalletRoutes(r chi.Router, bank moneyWalletBank) {
	withdrawal := func(w http.ResponseWriter, req *http.Request) (ws string, in economy.Withdrawal, a *passkey.Assertion, ok bool) {
		if ws, ok = walletOwner(w, req); !ok {
			return
		}
		var body struct {
			economy.Withdrawal
			Assertion *passkey.Assertion `json:"assertion"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"payee_id", "amount_minor", "idempotency_key"}`)
			return ws, in, nil, false
		}
		in = economy.Withdrawal{AccountID: chi.URLParam(req, "id"), PayeeID: body.PayeeID, AmountMinor: body.AmountMinor,
			Funding: body.Funding, IdempotencyKey: body.IdempotencyKey}
		if k := req.Header.Get("Idempotency-Key"); k != "" {
			in.IdempotencyKey = k
		}
		return ws, in, body.Assertion, true
	}
	r.Post("/v1/money/accounts/{id}/withdraw/challenge", func(w http.ResponseWriter, req *http.Request) {
		ws, in, _, ok := withdrawal(w, req)
		if !ok {
			return
		}
		c, creds, err := bank.WithdrawalChallenge(req.Context(), ws, in)
		if err != nil {
			writeWalletErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c, "allow_credentials": creds, "user_verification": "required"})
	})
	r.Post("/v1/money/accounts/{id}/withdraw", func(w http.ResponseWriter, req *http.Request) {
		ws, in, a, ok := withdrawal(w, req)
		if !ok {
			return
		}
		p, err := bank.WithdrawToOwnBank(req.Context(), ws, in, a)
		if err != nil {
			writeWalletErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, p)
	})

	r.Post("/v1/money/convert/quote", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := walletOwner(w, req)
		if !ok {
			return
		}
		var in economy.Conversion
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"sell_currency", "buy_currency", "sell_minor", "funding"}`)
			return
		}
		q, err := bank.QuoteSideConversion(req.Context(), ws, economy.Conversion{SellCurrency: in.SellCurrency, BuyCurrency: in.BuyCurrency,
			SellMinor: in.SellMinor, Funding: in.Funding})
		if err != nil {
			writeWalletErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, q)
	})
	conversion := func(w http.ResponseWriter, req *http.Request) (ws, quoteID, key string, a *passkey.Assertion, ok bool) {
		if ws, ok = walletOwner(w, req); !ok {
			return
		}
		var in struct {
			QuoteID        string             `json:"quote_id"`
			IdempotencyKey string             `json:"idempotency_key"`
			Assertion      *passkey.Assertion `json:"assertion"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.QuoteID == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"quote_id", "idempotency_key"}: POST /v1/money/convert/quote first`)
			return ws, "", "", nil, false
		}
		key = req.Header.Get("Idempotency-Key")
		if key == "" {
			key = in.IdempotencyKey
		}
		return ws, in.QuoteID, key, in.Assertion, true
	}
	r.Post("/v1/money/convert/challenge", func(w http.ResponseWriter, req *http.Request) {
		ws, quoteID, key, _, ok := conversion(w, req)
		if !ok {
			return
		}
		c, creds, err := bank.SideConversionChallenge(req.Context(), ws, quoteID, key)
		if err != nil {
			writeWalletErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c, "allow_credentials": creds, "user_verification": "required"})
	})
	r.Post("/v1/money/convert", func(w http.ResponseWriter, req *http.Request) {
		ws, quoteID, key, a, ok := conversion(w, req)
		if !ok {
			return
		}
		c, err := bank.ConvertSides(req.Context(), ws, quoteID, key, a)
		if err != nil {
			writeWalletErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, c)
	})
}

func writeWalletErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrWithdrawal):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, economy.ErrConversionUnsigned):
		writeJSONErr(w, http.StatusForbidden, err.Error())
	default:
		writeConvertErr(w, err)
	}
}
