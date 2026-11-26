package main

import (
	"cmp"
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
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B30.13 — accounts in pounds, euros and dollars for a company and each of its agents (economy/currency_accounts.go).
//
//	POST /v1/money/accounts   {currency, agent_id, funding} — the company's account in GBP, EUR, USD or USDC, or with
//	                          agent_id that agent's sub-account of it; funding is test (the default) or live
//	GET  /v1/money/accounts   ?agent_id= — the accounts and what each holds, test and live apart
//	GET  /v1/money/accounts/{id}/details — what a payer pays into it (B30.14): the company account's details, and an
//	                          agent account's payment reference; mode TEST for details the Test partner made up
//	POST /v1/money/accounts/{id}/test-money {amount_minor, idempotency_key} — B17.255: the owner or an admin puts
//	                          test money into a company account, a payment in from the Test partner
//	POST /v1/money/accounts/{id}/funding {direction, amount_minor, idempotency_key, funding} — B61.66: the owner or an
//	                          admin moves money now between the company's account and this agent account in its
//	                          currency: direction to_agent gives the agent money, to_company takes it back
//	GET  /v1/money/accounts/{id}/statement?from=&to=&format=json|csv|pdf — B56.14: every posting to it in [from, to),
//	                          with the opening, in, out and closing balances; ?month=YYYY-MM is that calendar month, so
//	                          far if it is this one (B30.23), and the PDF carries the account's details once a partner
//	                          has given real ones
//	GET  /v1/money/overview   ?agent_id= — B30.28: the cash view: totals per currency for the company and each agent,
//	                          money in and out this month, the next 30 days of standing orders and mandates, the runway
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
	MoveAgentMoney(ctx context.Context, workspaceID, agentAccountID, direction string, amountMinor int64, funding, key, by string) (economy.AgentFunding, error)
	AccountStatement(ctx context.Context, workspaceID, agentID, accountID string, from, to time.Time) (economy.MoneyStatement, error)
	FreezeCurrencyAccount(ctx context.Context, workspaceID, accountID string) (economy.CurrencyAccount, error)
	UnfreezeChallenge(ctx context.Context, workspaceID, accountID string) (challenge string, credentialIDs []string, err error)
	UnfreezeCurrencyAccount(ctx context.Context, workspaceID, accountID string, a *passkey.Assertion) (economy.CurrencyAccount, error)
	CloseChallenge(ctx context.Context, workspaceID, accountID, payeeID string) (challenge string, credentialIDs []string, err error)
	CloseCurrencyAccount(ctx context.Context, workspaceID, accountID, payeeID string, a *passkey.Assertion) (economy.CurrencyAccount, error)
	MoneyOverview(ctx context.Context, workspaceID, agentID string, now time.Time) (economy.MoneyOverview, error)
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
		case writeMoneyKindRefusal(w, err): // B59.42
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
	r.Get("/v1/money/overview", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req, req.URL.Query().Get("agent_id"))
		if !ok {
			return
		}
		o, err := bank.MoneyOverview(req.Context(), ws, agentID, time.Now())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, o)
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
	r.Post("/v1/money/accounts/{id}/funding", func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		by, owner := storedanswers.OwnerOrRole(req.Context(), moneyRoles...)
		if ws == "" || !owner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner, an admin or a person whose role allows it may move money between the company and an agent")
			return
		}
		var in struct {
			Direction      string `json:"direction"`
			AmountMinor    int64  `json:"amount_minor"`
			IdempotencyKey string `json:"idempotency_key"`
			Funding        string `json:"funding"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"direction": "to_agent|to_company", "amount_minor": <positive>, "idempotency_key": "<key>", "funding": "test|live"}`)
			return
		}
		f, err := bank.MoveAgentMoney(req.Context(), ws, chi.URLParam(req, "id"), in.Direction, in.AmountMinor, in.Funding, in.IdempotencyKey, by)
		switch {
		case errors.Is(err, economy.ErrAgentFunding):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrAgentFundingShort):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case err != nil:
			writePaymentErr(w, err)
		default:
			writeJSONOK(w, http.StatusCreated, f)
		}
	})
	r.Get("/v1/money/accounts/{id}/statement", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req, "")
		if !ok {
			return
		}
		now := time.Now()
		from, to, format, err := statementPeriod(req, now)
		if err == nil && format != "json" && format != "csv" && format != "pdf" && format != "xlsx" {
			err = errors.New("format must be json, csv, pdf or xlsx")
		}
		if m := req.URL.Query().Get("month"); err == nil && m != "" {
			from, to, err = statementMonth(m, req.URL.Query().Has("from") || req.URL.Query().Has("to"), now)
		}
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
		case format == "json":
			writeJSONOK(w, http.StatusOK, st)
		case format == "pdf" || format == "xlsx":
			// The details a payer pays into, only once a partner has given real ones: an account it cannot give them for
			// (the Test partner's, a closed one) is still stated, without them.
			d, derr := bank.CurrencyAccountDetails(req.Context(), ws, agentID, st.AccountID)
			if derr != nil || d.Mode != economy.DetailsLive {
				d = economy.CurrencyAccountDetails{}
			}
			who := statementWhoFor(req.Context(), bank, ws, economy.CapabilityCurrencyAccounts)
			if format == "xlsx" {
				writeStatementXLSX(w, statementFileName(who.Agents[st.AgentID], st.Currency, st.From, st.To), moneyStatementDoc(st, d, who, now))
				return
			}
			w.Header().Set("Content-Type", "application/pdf")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="account-statement-%s-%s-%s.pdf"`,
				st.Currency, st.From.Format(time.DateOnly), st.To.Format(time.DateOnly)))
			_, _ = w.Write(moneyStatementPDF(st, d, who, now)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- application/pdf rendered by Lens
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

// statementMonth is ?month=YYYY-MM as a period: the calendar month in UTC, up to now if it is this one. It is not
// asked with ?from= or ?to=.
func statementMonth(month string, withPeriod bool, now time.Time) (from, to time.Time, err error) {
	if withPeriod {
		return from, to, errors.New("ask for a month or a period from and to, not both")
	}
	m, err := time.Parse("2006-01", month)
	if err != nil {
		return from, to, errors.New("month must be YYYY-MM")
	}
	from, to = m.UTC(), m.UTC().AddDate(0, 1, 0)
	if now = now.UTC().Truncate(time.Microsecond); to.After(now) {
		to = now
	}
	if !from.Before(to) {
		return from, to, errors.New("that month has not begun")
	}
	return from, to, nil
}

// moneyStatementPDF is a currency account's period as the branded statement (B30.23, B61.131): the account and, once
// a partner has given them, its details; its opening, in, out and closing; then each line with who it was with, what it
// was, its reference, where it stands when it is not complete, what the agent said, and the balance after it.
func moneyStatementPDF(st economy.MoneyStatement, d economy.CurrencyAccountDetails, who economy.StatementWho, now time.Time) []byte {
	return moneyStatementDoc(st, d, who, now).PDF()
}

// moneyStatementDoc is the statement document the PDF and the Excel sheet are made from (B61.132).
func moneyStatementDoc(st economy.MoneyStatement, d economy.CurrencyAccountDetails, who economy.StatementWho, now time.Time) market.BrandedStatement {
	sec := brandedSection(who, st.Currency, st.From, st.To, now)
	sec.Source = economy.BalanceName(st.Currency)
	if sec.Holder = cmp.Or(who.Holder, d.Holder); sec.Holder == "" && st.AgentID == "" {
		sec.Holder = st.Name // a company account is named for its holder when it opens
	}
	sec.WhoName = sec.Source
	for _, l := range [][2]string{{"Sort code", d.SortCode}, {"Account number", d.AccountNumber}, {"IBAN", d.IBAN}, {"BIC", d.BIC},
		{"Routing number", d.RoutingNumber}, {"Payment reference", d.PaymentReference}} {
		if l[1] != "" {
			sec.WhoLines = append(sec.WhoLines, l[0]+" "+l[1])
		}
	}
	if st.AgentID != "" {
		brandedAgent(&sec, who, st.AgentID, st.Currency)
	}
	sec.Opening, sec.In, sec.Out, sec.Closing = st.OpeningMinor, st.InMinor, st.OutMinor, st.ClosingMinor
	for _, l := range st.Lines {
		sec.Rows = append(sec.Rows, brandedRow(l.At, l.Kind, brandedWith(l.Counterparty, who), []string{l.Memo, brandedRef(l.PartnerRef)}, l.State, l.Task,
			l.AmountMinor, l.BalanceAfterMinor))
	}
	return market.BrandedStatement{Sandbox: who.Sandbox, Sections: []market.BrandedSection{sec}}
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
	_ = cw.Write([]string{"entry_id", "line", "at", "kind", "amount_minor", "currency", "funding", "counterparty", "memo", "partner_ref", "balance_after_minor", "task", "state"})
	_ = cw.Write([]string{"", "", at(st.From), "opening", "", st.Currency, "", "", "", "", i64(st.OpeningMinor), "", ""})
	for _, l := range st.Lines {
		_ = cw.Write([]string{l.EntryID, strconv.Itoa(l.Line), at(l.At), l.Kind, i64(l.AmountMinor), st.Currency, l.Funding,
			csvText(l.Counterparty), csvText(l.Memo), csvText(l.PartnerRef), i64(l.BalanceAfterMinor),
			csvText(economy.TaskShown(l.Task)), l.State}) // B62.2: what the agent said, never as a fact
	}
	_ = cw.Write([]string{"", "", at(st.To), "closing", "", st.Currency, "", "", "", "", i64(st.ClosingMinor), "", ""})
	cw.Flush()
}
