package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B30.20 — invoices and payment links (economy/money_invoices.go).
//
//	POST /v1/money/invoices            {account_id, customer_name, customer_email, customer_address, customer_vat_number,
//	                                   seller_vat_number, lines: [{description, quantity, unit_amount_minor, vat_rate_bps}],
//	                                   due_date, remind_days_before, memo, send} — an agent's account issues an invoice
//	GET  /v1/money/invoices            ?agent_id= — the invoices issued, with their payments and reminders
//	POST /v1/money/invoices/{id}/send  a draft is sent: its pay link opens
//	POST /v1/money/invoices/{id}/void  a draft or sent one is voided
//	POST /v1/pay/{token}/agent         {agent_id, funding} — a Talyvor agent pays it from its account in the currency
//
// The workspace is the key's. Its owner or an admin issues and reads any of its agents' invoices; an agent's own key
// only its own, and may name its currency instead of account_id. Mounted in the authed group. The pay link itself —
// the public page and its card button — is mountInvoicePayRoutes, outside it:
//
//	GET  /v1/pay/{token}       the invoice, what is due and how to pay it: a card, or a transfer to these details quoting
//	                           this reference — "Preview — test money only"
//	POST /v1/pay/{token}/card  a Stripe test-mode Checkout Session for what is due: {url}

type moneyInvoiceBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	CreateInvoice(ctx context.Context, workspaceID, agentID string, r economy.InvoiceRequest) (economy.Invoice, error)
	Invoices(ctx context.Context, workspaceID, agentID string) ([]economy.Invoice, error)
	SendInvoice(ctx context.Context, workspaceID, agentID, id string) (economy.Invoice, error)
	VoidInvoice(ctx context.Context, workspaceID, agentID, id string) (economy.Invoice, error)
	PayInvoiceByAgent(ctx context.Context, token, workspaceID, agentID, funding string) (economy.PaidInvoice, error)
}

func mountInvoiceRoutes(r chi.Router, bank moneyInvoiceBank) {
	// caller is the key's workspace and the agent asked for: an agent's own key may ask only for itself.
	caller := func(w http.ResponseWriter, req *http.Request, asked string) (ws, agentID string, ok bool) {
		ws, _ = auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusBadRequest, "invoices are a workspace's: use a workspace's key")
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
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner, an admin or the agent's own key may use its invoices")
			return "", "", false
		}
		return ws, keyAgent, true
	}
	r.Post("/v1/money/invoices", func(w http.ResponseWriter, req *http.Request) {
		var in economy.InvoiceRequest
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"account_id", "customer_name", "customer_email", "lines": [{"description", "quantity", "unit_amount_minor", "vat_rate_bps"}], "due_date": "YYYY-MM-DD", "send"}`)
			return
		}
		ws, agentID, ok := caller(w, req, "")
		if !ok {
			return
		}
		inv, err := bank.CreateInvoice(req.Context(), ws, agentID, in)
		if err != nil {
			writeInvoiceErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, inv)
	})
	r.Get("/v1/money/invoices", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req, req.URL.Query().Get("agent_id"))
		if !ok {
			return
		}
		invoices, err := bank.Invoices(req.Context(), ws, agentID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"invoices": invoices})
	})
	move := func(do func(ctx context.Context, workspaceID, agentID, id string) (economy.Invoice, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			ws, agentID, ok := caller(w, req, "")
			if !ok {
				return
			}
			inv, err := do(req.Context(), ws, agentID, chi.URLParam(req, "id"))
			if err != nil {
				writeInvoiceErr(w, err)
				return
			}
			writeJSONOK(w, http.StatusOK, inv)
		}
	}
	r.Post("/v1/money/invoices/{id}/send", move(bank.SendInvoice))
	r.Post("/v1/money/invoices/{id}/void", move(bank.VoidInvoice))
	r.Post("/v1/pay/{token}/agent", func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			AgentID string `json:"agent_id"`
			Funding string `json:"funding"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"agent_id": "<the paying agent; an agent's own key names none>", "funding": "test|live"}`)
			return
		}
		ws, agentID, ok := caller(w, req, in.AgentID)
		if !ok {
			return
		}
		if agentID == "" {
			writeJSONErr(w, http.StatusBadRequest, "agent_id names the agent that pays")
			return
		}
		paid, err := bank.PayInvoiceByAgent(req.Context(), chi.URLParam(req, "token"), ws, agentID, in.Funding)
		if err != nil {
			writeInvoiceErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, paid)
	})
}

type invoicePayPages interface {
	InvoiceByToken(ctx context.Context, token string) (economy.Invoice, error)
	CurrencyAccountDetails(ctx context.Context, workspaceID, agentID, accountID string) (economy.CurrencyAccountDetails, error)
}

// invoiceCardCheckout opens a Stripe test-mode Checkout Session for an invoice: *billing.LiveStripe on a test-mode key.
type invoiceCardCheckout interface {
	CreateInvoiceCheckoutSession(ctx context.Context, p billing.InvoiceCheckout) (string, error)
}

// mountInvoicePayRoutes mounts the pay link's public routes on r; cards is nil when no Stripe test-mode key is set,
// and the page then offers no card. B50.13: cards is always Stripe test mode, so only a test (synthetic, isTest)
// workspace's invoice is paid by card; a real one's page offers no card and its card route is refused.
func mountInvoicePayRoutes(r chi.Router, pages invoicePayPages, cards invoiceCardCheckout, isTest func(wsID string) bool) {
	payable := func(inv economy.Invoice) bool {
		return inv.Status == economy.InvoiceSent || inv.Status == economy.InvoiceOverdue
	}
	r.Get("/v1/pay/{token}", func(w http.ResponseWriter, req *http.Request) {
		inv, err := pages.InvoiceByToken(req.Context(), chi.URLParam(req, "token"))
		if err != nil {
			writeInvoiceErr(w, err)
			return
		}
		page := map[string]any{"card": cards != nil && payable(inv) && isTest(inv.WorkspaceID)}
		if payable(inv) {
			// A transfer quotes the agent's payment reference, which routes it to the agent, and the invoice's.
			// The Test partner's details reach no bank, so a page on them offers no transfer (B49.1).
			if d, err := pages.CurrencyAccountDetails(req.Context(), inv.WorkspaceID, inv.AgentID, inv.AccountID); err == nil && d.Mode == economy.DetailsLive {
				page["transfer"] = map[string]any{"details": d.AccountDetails, "mode": d.Mode, "reference": d.PaymentReference + " " + inv.Reference}
			} else {
				slog.Info("invoice pay page: no account details to pay by transfer", "invoice", inv.ID, "err", err)
			}
		}
		// The payer sees the invoice, not the issuer's ids or its ledger.
		inv.WorkspaceID, inv.AgentID, inv.AccountID, inv.PayToken, inv.Payments = "", "", "", "", nil
		page["invoice"] = inv
		writeJSONOK(w, http.StatusOK, page)
	})
	r.Post("/v1/pay/{token}/card", func(w http.ResponseWriter, req *http.Request) {
		if cards == nil {
			writeJSONErr(w, http.StatusServiceUnavailable, "card payments need a Stripe test-mode key, and none is set")
			return
		}
		inv, err := pages.InvoiceByToken(req.Context(), chi.URLParam(req, "token"))
		if err != nil {
			writeInvoiceErr(w, err)
			return
		}
		if !isTest(inv.WorkspaceID) {
			writeJSONErr(w, http.StatusForbidden, "card payments are not switched on")
			return
		}
		if !payable(inv) || inv.DueMinor <= 0 {
			writeJSONErr(w, http.StatusConflict, "the invoice is "+inv.Status+": nothing is due on it")
			return
		}
		checkout, err := cards.CreateInvoiceCheckoutSession(req.Context(), billing.InvoiceCheckout{InvoiceID: inv.ID, Number: inv.Number,
			Issuer: inv.Issuer, Currency: inv.Currency, AmountMinor: inv.DueMinor, ReturnURL: inv.PayURL})
		if err != nil {
			writeJSONErr(w, http.StatusBadGateway, "Stripe did not open a card payment: "+err.Error())
			return
		}
		writeJSONOK(w, http.StatusCreated, map[string]string{"url": checkout})
	})
}

func writeInvoiceErr(w http.ResponseWriter, err error) {
	var refusal *economy.CapabilityRefusal
	switch {
	case errors.Is(err, economy.ErrInvoice):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrInvoiceNotFound), errors.Is(err, economy.ErrMoneyAccountNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.As(err, &refusal), errors.Is(err, economy.ErrAgentRule), errors.Is(err, economy.ErrApprovalRequired):
		writeJSONErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, economy.ErrInvoiceState), errors.Is(err, economy.ErrInvoiceShort), errors.Is(err, economy.ErrInvoicePayerPaused),
		errors.Is(err, economy.ErrMoneyAccountNotOpen), errors.Is(err, economy.ErrIdempotencyKeyReused):
		writeJSONErr(w, http.StatusConflict, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}

// originOf is the scheme and host of a URL of the web app's — where an invoice's pay link points — or "".
func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// sweepInvoices matches transfers to invoices and sends their reminders, each minute.
func sweepInvoices(ctx context.Context, store interface {
	SweepInvoices(ctx context.Context, now time.Time) ([]economy.InvoiceReminder, error)
}) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if sent, err := store.SweepInvoices(ctx, time.Now()); err != nil {
			slog.Warn("invoices: matching transfers and sending reminders", "err", err)
		} else if len(sent) > 0 {
			slog.Info("invoices: reminders sent", "reminders", len(sent))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
