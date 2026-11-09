package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/mcp"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/workspace"
)

// B20.2 — USE A LISTING, PAY PER USE, AND THE SELLER EARNS (internal/market/use.go).
//
//	POST /v1/workspaces/{wsID}/marketplace/listings/{id}/use   {version, model, input, variables, max_price_usd_micros}
//	POST /v1/workspaces/{wsID}/marketplace/listings/{id}/licences  {offer_id, version} + Idempotency-Key   B32.19: buy, rent or subscribe
//	GET  /v1/workspaces/{wsID}/marketplace/licences            the licences the workspace holds or held
//	POST /v1/workspaces/{wsID}/marketplace/licences/{id}/cancel  B32.20: stop renewing; it runs to its ends_at
//	GET  /v1/workspaces/{wsID}/marketplace/earnings            the seller's payable, in holdback and available (µUSD)
//	GET  /v1/workspaces/{wsID}/marketplace/journal             the seller's holdback and available on the journal, and whether they reconcile (B32.17)
//	GET  /v1/workspaces/{wsID}/marketplace/bill?month=2026-09  the buyer's billed uses in a month
//	GET  /v1/workspaces/{wsID}/marketplace/bill?invoice=in_…   B28.140: the uses one invoice carried ("upcoming": the period in progress)
//	GET  /v1/workspaces/{wsID}/marketplace/invoices            B28.140: the bill's invoices, newest first, the period in progress first
//
// A use runs the listing through Lens's own proxy with the caller's credential, so the models it calls are
// billed to the buyer as usual; a paid listing's price then goes on the buyer's monthly marketplace bill.
// An agent's key may use a listing within its spending rules (403 when they refuse, naming the approval
// one needs). A use that would cost more than its max_price_usd_micros answers 409, runs nothing and is charged
// nothing (B32.23).
//
// A listing whose per_use offer gives trial uses runs each buyer's first ones as trials (internal/market/trials.go,
// B32.21): free, never on the bill, and answered with trial: true and what the use would have cost.
//
// A licence (internal/market/licences.go) is bought once, on the bill, and covers the uses after it: each is charged
// "licensed" and runs the version the licence pins. A licence sent again with its Idempotency-Key answers 200 with
// the licence that key bought, and buys nothing.

type marketAgents interface {
	market.AgentJudge
	market.Capabilities
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
}

// marketCaller is who in wsID is calling: its agent, when an agent's key of wsID asked, else the person — the user,
// or the key or session they called with — a personal or enterprise licence counts.
func marketCaller(req *http.Request, agents marketAgents, wsID string) (agentID, person string, err error) {
	actx := auth.GetAuthContext(req.Context())
	if actx == nil {
		return "", "", nil
	}
	if actx.APIKeyID != "" && agents != nil {
		a, ws, err := agents.AgentOfKey(req.Context(), actx.APIKeyID)
		switch {
		case err == nil && ws == wsID:
			return a, "", nil
		case err != nil && !errors.Is(err, economy.ErrAgentNotFound):
			return "", "", err
		}
	}
	switch {
	case actx.UserID != "":
		return "", "user:" + actx.UserID, nil
	case actx.APIKeyID != "":
		return "", "key:" + actx.APIKeyID, nil
	case actx.SessionKeyID != "":
		return "", "session:" + actx.SessionKeyID, nil
	}
	return "", "", nil
}

func mountMarketUseRoutes(r chi.Router, store *market.Store, lens http.Handler, meter market.Meter, agents marketAgents) {
	r.Post("/v1/workspaces/{wsID}/marketplace/listings/{listingID}/use", func(w http.ResponseWriter, req *http.Request) {
		var in market.UseRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 256<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"version", "model", "input", "variables"}: `+err.Error())
			return
		}
		wsID := chi.URLParam(req, "wsID")
		agentID, person, err := marketCaller(req, agents, wsID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		in.Person = person
		// B25.6: a test workspace's paid uses go on its Stripe test-mode bill.
		m, byKind := meter, stripeByKind{}
		if k, ok := meter.(stripeByKind); ok {
			m, byKind = k.meterFor(wsID), k
		}
		deps := market.UseDeps{Runner: proxyRunner{lens: lens, from: req}, Meter: m, Agents: agents}
		u, err := store.Use(req.Context(), deps, wsID, agentID, chi.URLParam(req, "listingID"), in)
		switch {
		case err != nil:
			writeMarketUseErr(w, err, byKind, wsID)
		default:
			if u.MeterError != "" {
				slog.Warn("market: a use ran but is not yet on the buyer's bill; the next pass bills it",
					"use", u.ID, "workspace", wsID, "err", u.MeterError)
			}
			writeJSONOK(w, http.StatusOK, u)
		}
	})
	r.Post("/v1/workspaces/{wsID}/marketplace/listings/{listingID}/licences", func(w http.ResponseWriter, req *http.Request) {
		var in market.LicenceRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"offer_id", "version"}: `+err.Error())
			return
		}
		wsID := chi.URLParam(req, "wsID")
		agentID, person, err := marketCaller(req, agents, wsID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		in.Person = person
		m, byKind := meter, stripeByKind{}
		if k, ok := meter.(stripeByKind); ok {
			m, byKind = k.meterFor(wsID), k
		}
		deps := market.LicenceDeps{Meter: m, Agents: agents, Capabilities: agents}
		lic, again, err := store.License(req.Context(), deps, wsID, agentID, chi.URLParam(req, "listingID"), req.Header.Get("Idempotency-Key"), in)
		var need *economy.ApprovalNeededError
		switch {
		case errors.As(err, &need):
			writeJSONOK(w, http.StatusForbidden, map[string]any{"error": err.Error(), "approval_id": need.ApprovalID})
		case errors.Is(err, economy.ErrAgentRule), errors.Is(err, workspace.ErrMoneyWall), errors.Is(err, economy.ErrCapabilityNotCleared),
			errors.Is(err, market.ErrNotSoldHere):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case errors.Is(err, market.ErrNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, market.ErrTakenDown):
			writeJSONErr(w, http.StatusGone, err.Error())
		case errors.Is(err, market.ErrKeyReused):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, market.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, market.ErrNoBill):
			if byKind.isTest != nil {
				if _, why := byKind.billFor(wsID); why != nil {
					writeJSONErr(w, http.StatusForbidden, why.Error())
					return
				}
			}
			writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		case again:
			writeJSONOK(w, http.StatusOK, lic)
		default:
			if lic.MeterError != "" {
				slog.Warn("market: a licence was bought but is not yet on the buyer's bill; the next pass bills it",
					"licence", lic.ID, "use", lic.UseID, "workspace", wsID, "err", lic.MeterError)
			}
			writeJSONOK(w, http.StatusCreated, lic)
		}
	})
	r.Get("/v1/workspaces/{wsID}/marketplace/licences", func(w http.ResponseWriter, req *http.Request) {
		list, err := store.Licences(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if list == nil {
			list = []market.Licence{}
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"licences": list})
	})
	r.Post("/v1/workspaces/{wsID}/marketplace/licences/{licenceID}/cancel", func(w http.ResponseWriter, req *http.Request) {
		lic, err := store.CancelLicence(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "licenceID"))
		switch {
		case errors.Is(err, market.ErrNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, lic)
		}
	})
	r.Get("/v1/workspaces/{wsID}/marketplace/earnings", func(w http.ResponseWriter, req *http.Request) {
		e, err := store.SellerEarnings(req.Context(), chi.URLParam(req, "wsID"), time.Now())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, e)
	})
	r.Get("/v1/workspaces/{wsID}/marketplace/journal", func(w http.ResponseWriter, req *http.Request) {
		j, err := store.JournalCheck(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// What the seller is owed on the journal (available below zero: they owe it back), what of the holdback is
		// already due for release, and whether it all agrees with their earnings and payouts. Why it does not is
		// the operator's to read, with `lens market journal-check`.
		writeJSONOK(w, http.StatusOK, map[string]any{
			"holdback_usd_micros":        j.JournalHoldbackUSDMicros,
			"available_usd_micros":       j.JournalAvailableUSDMicros,
			"due_for_release_usd_micros": j.PendingReleaseUSDMicros,
			"reconciled":                 j.OK(),
		})
	})
	r.Get("/v1/workspaces/{wsID}/marketplace/invoices", func(w http.ResponseWriter, req *http.Request) {
		// Stripe's PDF of an invoice prints the buyer's name and address, as a receipt does: the owner's or an admin's.
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may read its invoices")
			return
		}
		list, err := store.Invoices(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"invoices": list})
	})
	r.Get("/v1/workspaces/{wsID}/marketplace/bill", func(w http.ResponseWriter, req *http.Request) {
		if id := req.URL.Query().Get("invoice"); id != "" { // B28.140: the uses one invoice carried
			b, err := store.InvoiceBill(req.Context(), chi.URLParam(req, "wsID"), id)
			switch {
			case errors.Is(err, market.ErrNoInvoice):
				writeJSONErr(w, http.StatusNotFound, err.Error())
			case err != nil:
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
			default:
				if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
					b.Invoice.InvoicePDF = "" // its PDF is the owner's, as the invoices list is
				}
				writeJSONOK(w, http.StatusOK, b)
			}
			return
		}
		month := time.Now().UTC()
		if m := req.URL.Query().Get("month"); m != "" {
			t, err := time.Parse("2006-01", m)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, "month must be YYYY-MM")
				return
			}
			month = t
		}
		b, err := store.MonthBill(req.Context(), chi.URLParam(req, "wsID"), month)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, b)
	})
}

// writeMarketUseErr answers a use Store.Use refused or could not run.
func writeMarketUseErr(w http.ResponseWriter, err error, byKind stripeByKind, wsID string) {
	var need *economy.ApprovalNeededError
	var ran *runError
	switch {
	case errors.As(err, &need):
		writeJSONOK(w, http.StatusForbidden, map[string]any{"error": err.Error(), "approval_id": need.ApprovalID})
	case errors.Is(err, economy.ErrAgentRule), errors.Is(err, workspace.ErrMoneyWall), errors.Is(err, market.ErrNotSoldHere):
		writeJSONErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, market.ErrNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, market.ErrTakenDown):
		writeJSONErr(w, http.StatusGone, err.Error())
	case errors.Is(err, market.ErrNotSoldPerUse), errors.Is(err, market.ErrOverMaxPrice):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, market.ErrInvalid), errors.Is(err, market.ErrNoModel):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, market.ErrNotRunnable):
		writeJSONErr(w, http.StatusNotImplemented, err.Error())
	case errors.Is(err, market.ErrNoBill):
		if byKind.isTest != nil {
			if _, why := byKind.billFor(wsID); why != nil {
				writeJSONErr(w, http.StatusForbidden, why.Error())
				return
			}
		}
		writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
	case errors.As(err, &ran):
		// The buyer's own limits pass through (no credit, rate limited, a model their agent may not use). A
		// 401 cannot be the buyer's credential — it just passed the same authentication — so it is the
		// provider's, and a bad gateway.
		status := http.StatusBadGateway
		if ran.status >= 400 && ran.status < 500 && ran.status != http.StatusUnauthorized {
			status = ran.status
		}
		writeJSONErr(w, status, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}

// refundTakenDownMarketUses finishes what a takedown could not (B20.4): it refunds a use that was running
// when its listing came down, and retries a buyer's credit Stripe did not accept. refunder nil: no
// marketplace bill, so nothing was ever metered and there is nothing to credit.
func refundTakenDownMarketUses(ctx context.Context, store *market.Store, refunder market.Refunder) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if refunder == nil {
				continue
			}
			refunded, credited, err := store.RefundTakenDown(ctx, refunder)
			if err != nil {
				slog.Warn("market: refunding taken-down listings", "refunded", refunded, "credited", credited, "err", err)
			} else if refunded+credited > 0 {
				slog.Info("market: refunded taken-down listings", "refunded", refunded, "credited", credited)
			}
		}
	}
}

// meterPendingMarketUses bills, every few minutes, the uses whose meter event did not reach Stripe when
// they were used.
func meterPendingMarketUses(ctx context.Context, store *market.Store, meter market.Meter) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := store.MeterPending(ctx, meter, time.Minute); err != nil {
				slog.Warn("market: billing pending uses", "billed", n, "err", err)
			} else if n > 0 {
				slog.Info("market: billed pending uses", "billed", n)
			}
		}
	}
}

// releaseMarketHoldbacks moves, every few minutes, each earning past its 14-day holdback to its seller's available
// balance on the marketplace journal (B32.17), unless a hold keeps it in escrow.
func releaseMarketHoldbacks(ctx context.Context, store *market.Store) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := store.ReleaseDue(ctx, time.Now()); err != nil {
				slog.Warn("market: releasing earnings past their holdback", "released", n, "err", err)
			} else if n > 0 {
				slog.Info("market: released earnings past their holdback", "released", n)
			}
		}
	}
}

// mcpMarketDeps gives the MCP market tools (B32.23) what the routes above give a use and a licence: the workspace's
// bill, the agent's judge, and a runner that calls the models with the credential the tool call came in with.
type mcpMarketDeps struct {
	lens   http.Handler
	wallet http.Handler // B41.178: the in-process proxy a connector caller's model calls go through (roomWalletProxy)
	meter  market.Meter
	agents marketAgents
}

func (d mcpMarketDeps) meterFor(wsID string) market.Meter {
	if k, ok := d.meter.(stripeByKind); ok {
		return k.meterFor(wsID)
	}
	return d.meter
}

func (d mcpMarketDeps) UseDeps(ctx context.Context, wsID string) market.UseDeps {
	run := proxyRunner{lens: d.lens, from: mcp.CallerRequest(ctx)}
	if as, ok := ctx.Value(connectorCallerKey{}).(*auth.AuthContext); ok { // B41.178: as the agent's key behind the token
		run = proxyRunner{lens: d.wallet, from: run.from, as: as}
	}
	return market.UseDeps{Runner: run, Meter: d.meterFor(wsID), Agents: d.agents}
}

func (d mcpMarketDeps) LicenceDeps(_ context.Context, wsID string) market.LicenceDeps {
	return market.LicenceDeps{Meter: d.meterFor(wsID), Agents: d.agents, Capabilities: d.agents}
}

// proxyRunner runs a listing's model calls through Lens's own proxy routes as the caller — the same
// credential, so the same authentication, rules, cache and billing as the caller's own requests.
type proxyRunner struct {
	lens http.Handler
	from *http.Request
	// as, when set, is the credential the calls are made with instead of the caller's: a room wallet's key, whose
	// plaintext nobody holds (B32.33). lens is then roomWalletProxy, which takes the identity from the request's context.
	as *auth.AuthContext
}

// runError is a model call the proxy did not answer with a 2xx.
type runError struct {
	status int
	body   string
}

func (e *runError) Error() string {
	return fmt.Sprintf("market: running the listing: Lens answered %d: %s", e.status, e.body)
}

func (p proxyRunner) Run(ctx context.Context, model string, messages []market.Message) (string, error) {
	var path string
	var body []byte
	anthropic := strings.HasPrefix(model, "claude")
	if anthropic {
		system, turns := "", []market.Message{}
		for _, m := range messages {
			if m.Role == "system" {
				system = m.Content
			} else {
				turns = append(turns, m)
			}
		}
		req := map[string]any{"model": model, "max_tokens": 4096, "messages": turns}
		if system != "" {
			req["system"] = system
		}
		body, _ = json.Marshal(req)
		path = "/v1/proxy/anthropic/v1/messages"
	} else {
		body, _ = json.Marshal(map[string]any{"model": model, "messages": messages})
		path = "/v1/proxy/openai/v1/chat/completions"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.RemoteAddr = p.from.RemoteAddr
	req.Header.Set("Content-Type", "application/json")
	if p.as != nil {
		req = req.WithContext(auth.WithAuthContext(ctx, p.as))
	} else {
		for _, h := range []string{"Authorization", "X-Talyvor-Key", "X-Api-Key"} {
			if v := p.from.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
	}
	req.Header.Set("X-Talyvor-Feature", "marketplace")
	rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
	p.lens.ServeHTTP(rec, req)
	if rec.status/100 != 2 {
		return "", &runError{status: rec.status, body: strings.TrimSpace(truncate(rec.buf.String(), 500))}
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.buf.Bytes(), &out); err != nil {
		return "", &runError{status: http.StatusBadGateway, body: "an unreadable answer: " + truncate(rec.buf.String(), 200)}
	}
	if !anthropic && len(out.Choices) > 0 {
		return out.Choices[0].Message.Content, nil
	}
	var text strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	return text.String(), nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// bufferedResponse keeps an in-process response.
type bufferedResponse struct {
	header http.Header
	status int
	buf    bytes.Buffer
	wrote  bool
}

func (b *bufferedResponse) Header() http.Header { return b.header }
func (b *bufferedResponse) WriteHeader(code int) {
	if !b.wrote {
		b.status, b.wrote = code, true
	}
}
func (b *bufferedResponse) Write(p []byte) (int, error) {
	b.wrote = true
	return b.buf.Write(p)
}
