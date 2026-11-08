package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B30.13 — accounts in pounds, euros and dollars for a company and each of its agents (economy/currency_accounts.go).
//
//	POST /v1/money/accounts   {currency, agent_id, funding} — the company's account in GBP, EUR or USD, or with
//	                          agent_id that agent's sub-account of it; funding is test (the default) or live
//	GET  /v1/money/accounts   ?agent_id= — the accounts and what each holds, test and live apart
//	GET  /v1/money/accounts/{id}/details — what a payer pays into it (B30.14): the company account's details, and an
//	                          agent account's payment reference; mode TEST for details the Test partner made up
//	POST /v1/money/accounts/{id}/test-money {amount_minor, idempotency_key} — B17.255: the owner or an admin puts
//	                          test money into a company account, a payment in from the Test partner
//	GET  /v1/money/accounts/{id}/statement?from=&to=&format=json|csv — B56.14: every posting to it in [from, to), with
//	                          the opening, in, out and closing balances
//
// B30.24 — the owner freezes, unfreezes and closes an account (economy/account_status.go):
//
//	POST /v1/money/accounts/{id}/freeze   — nothing goes out; money coming in still lands
//	POST /v1/money/accounts/{id}/unfreeze/challenge — the challenge an unfreeze is signed over, and the passkeys that may
//	POST /v1/money/accounts/{id}/unfreeze {"assertion": {credential_id, client_data_json, authenticator_data, signature}}
//	                          — needed once the workspace has a passkey
//	POST /v1/money/accounts/{id}/close/challenge {"payee_id"} — the challenge a close paying that payee is signed over
//	POST /v1/money/accounts/{id}/close    {"payee_id", "assertion"} — an agent's balance goes to the company's account;
//	                          the company's to the saved payee, signed once the workspace has a passkey; the account
//	                          then reads closed at zero
//
// The workspace is the key's. Its owner or an admin opens and reads any of its accounts; an agent's own key only the
// agent's, and opens them with no agent_id. Only the owner or an admin freezes, unfreezes or closes one. Mounted in
// the authed group.

type moneyAccountBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	OpenCurrencyAccount(ctx context.Context, workspaceID, agentID, currency, funding string) (economy.CurrencyAccount, error)
	CurrencyAccounts(ctx context.Context, workspaceID, agentID string) ([]economy.CurrencyAccount, error)
	CurrencyAccountDetails(ctx context.Context, workspaceID, agentID, accountID string) (economy.CurrencyAccountDetails, error)
	AddTestMoney(ctx context.Context, workspaceID, accountID string, amountMinor int64, key string) (economy.MoneyEntry, error)
	AccountStatement(ctx context.Context, workspaceID, agentID, accountID string, from, to time.Time) (economy.MoneyStatement, error)
	FreezeCurrencyAccount(ctx context.Context, workspaceID, accountID string) (economy.CurrencyAccount, error)
	UnfreezeChallenge(ctx context.Context, workspaceID, accountID string) (challenge string, credentialIDs []string, err error)
	UnfreezeCurrencyAccount(ctx context.Context, workspaceID, accountID string, a *passkey.Assertion) (economy.CurrencyAccount, error)
	CloseChallenge(ctx context.Context, workspaceID, accountID, payeeID string) (challenge string, credentialIDs []string, err error)
	CloseCurrencyAccount(ctx context.Context, workspaceID, accountID, payeeID string, a *passkey.Assertion) (economy.CurrencyAccount, error)
}

func mountMoneyAccountRoutes(r chi.Router, bank moneyAccountBank) {
	// caller is the key's workspace and the agent asked for: an agent's own key may ask only for itself.
	caller := func(w http.ResponseWriter, req *http.Request, asked string) (ws, agentID string, ok bool) {
		ws, _ = auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusBadRequest, "money accounts are a workspace's: use a workspace's key")
			return "", "", false
		}
		if _, owner := storedanswers.OwnerOrAdmin(req.Context()); owner {
			return ws, asked, true
		}
		keyAgent := ""
		if actx := auth.GetAuthContext(req.Context()); actx != nil && actx.APIKeyID != "" {
			keyAgent, _, _ = bank.AgentOfKey(req.Context(), actx.APIKeyID)
		}
		if keyAgent == "" || (asked != "" && asked != keyAgent) {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner, an admin or the agent's own key may use its money accounts")
			return "", "", false
		}
		return ws, keyAgent, true
	}
	r.Post("/v1/money/accounts", func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Currency string `json:"currency"`
			AgentID  string `json:"agent_id"`
			Funding  string `json:"funding"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"currency": "GBP|EUR|USD", "agent_id": "<optional>", "funding": "test|live"}`)
			return
		}
		ws, agentID, ok := caller(w, req, in.AgentID)
		if !ok {
			return
		}
		a, err := bank.OpenCurrencyAccount(req.Context(), ws, agentID, in.Currency, in.Funding)
		switch {
		case errors.Is(err, economy.ErrCurrencyAccount):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrAgentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrCurrencyAccountExists), errors.Is(err, economy.ErrCompanyAccountNeeded):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, economy.ErrCapabilityNotCleared):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case errors.Is(err, economy.ErrAccountNotOpened):
			writeJSONErr(w, http.StatusBadGateway, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusCreated, a)
		}
	})
	r.Get("/v1/money/accounts", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req, req.URL.Query().Get("agent_id"))
		if !ok {
			return
		}
		accounts, err := bank.CurrencyAccounts(req.Context(), ws, agentID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"accounts": accounts})
	})
	r.Post("/v1/money/accounts/{id}/test-money", func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		if _, owner := storedanswers.OwnerOrAdmin(req.Context()); ws == "" || !owner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may add money here")
			return
		}
		var in struct {
			AmountMinor    int64  `json:"amount_minor"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"amount_minor": <positive>, "idempotency_key": "<key>"}`)
			return
		}
		e, err := bank.AddTestMoney(req.Context(), ws, chi.URLParam(req, "id"), in.AmountMinor, in.IdempotencyKey)
		switch {
		case errors.Is(err, economy.ErrTestMoney):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writePaymentErr(w, err)
		default:
			writeJSONOK(w, http.StatusCreated, e)
		}
	})
	r.Get("/v1/money/accounts/{id}/statement", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req, "")
		if !ok {
			return
		}
		from, to, asCSV, err := statementPeriod(req, time.Now())
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		st, err := bank.AccountStatement(req.Context(), ws, agentID, chi.URLParam(req, "id"), from, to)
		switch {
		case errors.Is(err, economy.ErrMoneyAccountNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		case !asCSV:
			writeJSONOK(w, http.StatusOK, st)
		default:
			writeMoneyStatement(w, st)
		}
	})
	r.Get("/v1/money/accounts/{id}/details", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req, "")
		if !ok {
			return
		}
		d, err := bank.CurrencyAccountDetails(req.Context(), ws, agentID, chi.URLParam(req, "id"))
		switch {
		case errors.Is(err, economy.ErrMoneyAccountNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrMoneyAccountNotOpen):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, economy.ErrCapabilityNotCleared):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case errors.Is(err, economy.ErrNoAccountDetails):
			writeJSONErr(w, http.StatusBadGateway, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, d)
		}
	})

	// B30.24: the owner's, with the account's new state answered.
	owner := func(w http.ResponseWriter, req *http.Request) (string, bool) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusBadRequest, "money accounts are a workspace's: use a workspace's key")
			return "", false
		}
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may freeze, unfreeze or close its accounts")
			return "", false
		}
		return ws, true
	}
	answer := func(w http.ResponseWriter, a economy.CurrencyAccount, err error) {
		switch {
		case errors.Is(err, economy.ErrAccountStatus), errors.Is(err, economy.ErrAccountNotEmpty):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, economy.ErrPasskeyRequired), errors.Is(err, passkey.ErrInvalid):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case err != nil: // not found, or a close's payment to the payee
			writePaymentErr(w, err)
		default:
			writeJSONOK(w, http.StatusOK, a)
		}
	}
	r.Post("/v1/money/accounts/{id}/freeze", func(w http.ResponseWriter, req *http.Request) {
		if ws, ok := owner(w, req); ok {
			a, err := bank.FreezeCurrencyAccount(req.Context(), ws, chi.URLParam(req, "id"))
			answer(w, a, err)
		}
	})
	r.Post("/v1/money/accounts/{id}/unfreeze/challenge", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		c, creds, err := bank.UnfreezeChallenge(req.Context(), ws, chi.URLParam(req, "id"))
		if err != nil {
			answer(w, economy.CurrencyAccount{}, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c, "allow_credentials": creds, "user_verification": "required"})
	})
	r.Post("/v1/money/accounts/{id}/unfreeze", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		var in struct {
			Assertion *passkey.Assertion `json:"assertion"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		a, err := bank.UnfreezeCurrencyAccount(req.Context(), ws, chi.URLParam(req, "id"), in.Assertion)
		answer(w, a, err)
	})
	type closeIn struct {
		PayeeID   string             `json:"payee_id"`
		Assertion *passkey.Assertion `json:"assertion"`
	}
	r.Post("/v1/money/accounts/{id}/close/challenge", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		var in closeIn
		_ = json.NewDecoder(req.Body).Decode(&in)
		c, creds, err := bank.CloseChallenge(req.Context(), ws, chi.URLParam(req, "id"), in.PayeeID)
		if err != nil {
			answer(w, economy.CurrencyAccount{}, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c, "allow_credentials": creds, "user_verification": "required"})
	})
	r.Post("/v1/money/accounts/{id}/close", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		var in closeIn
		_ = json.NewDecoder(req.Body).Decode(&in)
		a, err := bank.CloseCurrencyAccount(req.Context(), ws, chi.URLParam(req, "id"), in.PayeeID, in.Assertion)
		answer(w, a, err)
	})
}

// writeMoneyStatement answers with a currency account's statement as CSV: a header, the opening balance, every line,
// then the closing balance, in the currency's minor units.
func writeMoneyStatement(w http.ResponseWriter, st economy.MoneyStatement) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="account-statement-%s-%s-%s.csv"`,
		st.Currency, st.From.Format(time.DateOnly), st.To.Format(time.DateOnly)))
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	i64 := func(v int64) string { return strconv.FormatInt(v, 10) }
	at := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
	_ = cw.Write([]string{"entry_id", "line", "at", "kind", "amount_minor", "currency", "funding", "counterparty", "memo", "partner_ref", "balance_after_minor"})
	_ = cw.Write([]string{"", "", at(st.From), "opening", "", st.Currency, "", "", "", "", i64(st.OpeningMinor)})
	for _, l := range st.Lines {
		_ = cw.Write([]string{l.EntryID, strconv.Itoa(l.Line), at(l.At), l.Kind, i64(l.AmountMinor), st.Currency, l.Funding,
			csvText(l.Counterparty), csvText(l.Memo), csvText(l.PartnerRef), i64(l.BalanceAfterMinor)})
	}
	_ = cw.Write([]string{"", "", at(st.To), "closing", "", st.Currency, "", "", "", "", i64(st.ClosingMinor)})
	cw.Flush()
}
