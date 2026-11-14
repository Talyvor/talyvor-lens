package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B59.17 on real Postgres — an Instant transfer to an outside bank account (launch pack 4.2 (3)), through the money kind
// gate and the handler as main.go mounts them, read off the ledger. The agent names a saved payee at a bank account; at
// the Test FX partner's rates ($1.25 a pound, $1.10 a euro) and a 50 basis point fee, £100.00 is converted to 124.375
// USDC, moved, converted to €113.06 and paid into the German account by the payout provider: three legs. A beneficiary
// the transfer hooks' screening flags is refused before any leg moves; a slow rail's time is in the quote, in the
// approval the owner confirms and in the transfer; a failed payout reverses the move and the conversion and leaves the
// agent exactly as it was; and pounds into a UK account at a direct quote are paid straight, in one leg.
func TestB5917_InstantTransferToAnOutsideBankAccount(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const sender = "ws-b5917-sender"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, 'Sender Ltd', $1, 'business', 'hybrid')`,
		sender); err != nil {
		t.Fatal(err)
	}
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetScreener(screener)
	store.SetPayeeScreener(screener)
	store.SetFXMargin(func(context.Context, economy.FeeQuerier, string) (int64, string, error) { return 50, "team", nil })
	sandbox := money.NewSandbox(registry, "b5917")
	details := &partners.TestTransferHooks{Screening: partners.TestScreeningProvider{Addresses: map[string]string{"blocked bank gmbh": partners.AddressSanctioned}}}
	hooks := &failAfterLeg{TransferHooks: store.InstantTransferHooks(details), sandbox: sandbox}
	layer, err := money.NewRegistry(money.Config{Sandbox: sandbox, Limits: store.MoneyLimits(), Audit: store, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	store.SetMoneyLayer(layer)

	agent, err := store.CreateAgent(ctx, sender, "Payer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenCurrencyAccount(ctx, sender, "", "GBP", "test"); err != nil {
		t.Fatal(err)
	}
	from, err := store.OpenCurrencyAccount(ctx, sender, agent.ID, "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	var topUp string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = 'GBP'`, sender).Scan(&topUp); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: sender, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: topUp, AmountMinor: -1_000_00}, {AccountID: from.ID, AmountMinor: 1_000_00}}}); err != nil {
		t.Fatal(err)
	}
	on := true
	if _, err := store.SetAgentRules(ctx, sender, agent.ID, economy.AgentRules{MayInstantTransfer: &on}); err != nil {
		t.Fatal(err)
	}
	payee := func(p economy.OutsidePayee) economy.OutsidePayee {
		t.Helper()
		p, err := store.CreateOutsidePayee(ctx, sender, p)
		if err != nil {
			t.Fatalf("save %s: %v", p.Name, err)
		}
		// The owner confirms the payee with a passkey (B30.16's confirmation, tested there).
		if _, err := pool.Exec(ctx, `UPDATE money_payees SET confirmed_at = now(), confirmed_by = 'passkey' WHERE id = $1`, p.ID); err != nil {
			t.Fatal(err)
		}
		return p
	}
	german := payee(economy.OutsidePayee{Name: "Grace Hopper", Country: "DE", IBAN: "DE89370400440532013000", BIC: "COBADEFFXXX"})
	blocked := payee(economy.OutsidePayee{Name: "Blocked Bank GmbH", Country: "DE", IBAN: "DE89370400440532013000", BIC: "COBADEFFXXX"})
	american := payee(economy.OutsidePayee{Name: "Grace Hopper", Country: "US", RoutingNumber: "021000021", AccountNumber: "123456789"})
	british := payee(economy.OutsidePayee{Name: "Grace Hopper", Country: "GB", SortCode: "200000", AccountNumber: "55779911"})

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			owner := &auth.AuthContext{WorkspaceID: sender, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
			next.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), owner)))
		})
	})
	r.Group(func(g chi.Router) {
		g.Use(moneyKindGate(store))
		mountInstantTransferRoutes(g, store)
	})
	quote := func(receive, country string) economy.InstantQuote {
		t.Helper()
		raw, _ := json.Marshal(economy.InstantQuoteRequest{SendMinor: 100_00, SendCurrency: "GBP", ReceiveCurrency: receive, To: economy.InstantToBank, Country: country})
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+sender+"/money/instant-transfers/quote", strings.NewReader(string(raw)))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var q economy.InstantQuote
		if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil || rec.Code != http.StatusCreated {
			t.Fatalf("quote: %d %s", rec.Code, rec.Body.String())
		}
		return q
	}
	send := func(quoteID, payeeID, key string) (int, map[string]any, economy.InstantTransfer) {
		t.Helper()
		raw, _ := json.Marshal(economy.InstantSend{QuoteID: quoteID, ToPayeeID: payeeID})
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+sender+"/agents/"+agent.ID+"/money/instant-transfers", strings.NewReader(string(raw)))
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var fields map[string]any
		var it economy.InstantTransfer
		_ = json.Unmarshal(rec.Body.Bytes(), &fields)
		_ = json.Unmarshal(rec.Body.Bytes(), &it)
		return rec.Code, fields, it
	}
	// approve signs the approval the agent's last send filed, and answers the words the owner confirmed.
	approve := func() string {
		t.Helper()
		var id, memo string
		if err := pool.QueryRow(ctx, `SELECT id, memo FROM agent_approvals WHERE agent_id = $1 AND status = 'pending' ORDER BY created_at DESC LIMIT 1`,
			agent.ID).Scan(&id, &memo); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DecideAgentApproval(ctx, sender, id, true); err != nil {
			t.Fatal(err)
		}
		return memo
	}
	balance := func(account string) int64 {
		t.Helper()
		var b int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1`, account).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	paidOut := func(entry string) (minor int64, currency string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT p.amount_minor, a.currency FROM money_postings p JOIN money_accounts a ON a.id = p.account_id
			WHERE p.entry_id = $1 AND a.purpose = 'partner' AND a.name = 'Instant transfer payouts'`, entry).Scan(&minor, &currency); err != nil {
			t.Fatalf("the payout entry %s: %v", entry, err)
		}
		return minor, currency
	}
	moved := func() (entries, legs int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM money_entries WHERE kind LIKE 'instant_transfer_%'),
			(SELECT count(*) FROM instant_transfer_legs WHERE workspace_id = $1 AND method <> 'quote' AND method <> 'rail')`, sender).Scan(&entries, &legs); err != nil {
			t.Fatal(err)
		}
		return entries, legs
	}

	t.Run("a screened-out beneficiary is refused before any leg moves", func(t *testing.T) {
		entries, legs := moved()
		code, body, _ := send(quote("EUR", "DE").ID, blocked.ID, "flagged")
		if code != http.StatusForbidden || body["code"] != "screened" || body["field"] != "to_payee_id" {
			t.Fatalf("a send to a sanctioned beneficiary: %d %v, want 403 screened on to_payee_id", code, body)
		}
		if e, l := moved(); balance(from.ID) != 1_000_00 || e != entries || l != legs {
			t.Fatalf("the refused send moved money: balance %d, %d → %d entries, %d → %d legs", balance(from.ID), entries, e, legs, l)
		}
	})

	t.Run("a slow rail's time is in the quote, the approval the owner confirms, and the transfer", func(t *testing.T) {
		q := quote("USD", "US")
		if q.ArrivesInSeconds || q.ArrivesWithinSeconds != 24*60*60 {
			t.Fatalf("pounds to a US bank: %+v, want it arriving within same-day ACH's 24 hours", q)
		}
		if code, body, _ := send(q.ID, american.ID, "to-us-first"); code != http.StatusForbidden || !strings.Contains(body["error"].(string), "approv") {
			t.Fatalf("a first send to a new bank payee: %d %v, want it to wait for the owner's approval", code, body)
		}
		if memo := approve(); !strings.Contains(memo, "arrives within 24 hours") {
			t.Fatalf("the approval the owner confirms reads %q, want it to say it arrives within 24 hours", memo)
		}
		code, body, it := send(quote("USD", "US").ID, american.ID, "to-us")
		if code != http.StatusOK || it.Status != economy.InstantReceived || it.ArrivesInSeconds || it.ArrivesWithinSeconds != 24*60*60 {
			t.Fatalf("the transfer: %d %v, want it paid, arriving within 24 hours", code, body)
		}
	})

	var sent economy.InstantTransfer
	t.Run("to a bank account: converted, moved and paid out in three legs, once under its key", func(t *testing.T) {
		if code, _, _ := send(quote("EUR", "DE").ID, german.ID, "to-de-first"); code != http.StatusForbidden {
			t.Fatalf("a first send to a new bank payee: %d, want it to wait for the owner's approval", code)
		}
		approve()
		before := balance(from.ID)
		code, body, it := send(quote("EUR", "DE").ID, german.ID, "to-de")
		if code != http.StatusOK || it.Status != economy.InstantReceived || it.ToPayeeID != german.ID || it.ToAccountID != "" || it.ToAddress != "" ||
			it.SendMinor != 100_00 || it.FeeMinor != 50 || it.ReceiveMinor != 113_06 || it.ReceiveCurrency != "EUR" || it.ArrivesWithinSeconds != 10 {
			t.Fatalf("the transfer: %d %v, want £100.00 sent, a 50p fee and €113.06 paid into the bank within 10 seconds", code, body)
		}
		sent = it
		if len(it.Legs) != 3 || it.Legs[0].Leg != economy.InstantLegConvert || it.Legs[1].Leg != economy.InstantLegMove || it.Legs[2].Leg != economy.InstantLegPayout ||
			it.Legs[2].Sell.Minor != 124_375000 || it.Legs[2].Buy.Minor != 113_06 || it.Legs[2].Buy.Currency != "EUR" {
			t.Fatalf("the legs: %+v, want convert, move and a payout of 124.375 USDC as €113.06", it.Legs)
		}
		for _, l := range it.Legs {
			if !l.Done || l.EntryID == "" || l.PartnerRef == "" {
				t.Errorf("leg %s: %+v, want done with its entry and the provider's reference", l.ID, l)
			}
		}
		if it.Legs[2].ConversionRef == "" {
			t.Errorf("the payout leg carries no reference for its conversion out of USDC: %+v", it.Legs[2])
		}
		if balance(from.ID) != before-100_00 {
			t.Fatalf("the agent holds %d, want %d", balance(from.ID), before-100_00)
		}
		// On the ledger: the payout leg's entry pays €113.06 out through the payout partner.
		if minor, currency := paidOut(it.Legs[2].EntryID); minor != 113_06 || currency != "EUR" {
			t.Fatalf("the payout leg paid out %d %s, want 11306 EUR", minor, currency)
		}
		// The same key again answers the same transfer and moves nothing more.
		entries, _ := moved()
		if code, _, again := send(it.QuoteID, german.ID, "to-de"); code != http.StatusOK || again.ID != it.ID || again.Status != economy.InstantReceived {
			t.Fatalf("a repeat with the same key: %d %+v, want the first transfer", code, again)
		}
		if e, _ := moved(); e != entries || balance(from.ID) != before-100_00 {
			t.Fatalf("a repeat with the same key moved money")
		}
	})

	t.Run("a failed payout reverses the move and the conversion and leaves the agent whole", func(t *testing.T) {
		if sent.ID == "" {
			t.Fatal("the bank was never paid, so this send would wait for an approval")
		}
		defer hooks.disarm(t)
		if err := money.FailAt(sandbox, money.CapBankPayout, money.ErrRefused); err != nil {
			t.Fatal(err)
		}
		before := balance(from.ID)
		code, body, it := send(quote("EUR", "DE").ID, german.ID, "fail-payout")
		if code != http.StatusOK || it.Status != economy.InstantReversed || it.FailedLeg != economy.InstantLegPayout || len(it.ReversalEntries) != 2 {
			t.Fatalf("the transfer: %d %v, want it reversed at the payout leg, the move and the conversion back", code, body)
		}
		if balance(from.ID) != before {
			t.Fatalf("after a failed payout the agent has %d, was %d", balance(from.ID), before)
		}
		var kinds []string
		if err := pool.QueryRow(ctx, `SELECT array_agg(kind ORDER BY kind) FROM money_entries WHERE id = ANY($1)`, it.ReversalEntries).Scan(&kinds); err != nil ||
			strings.Join(kinds, ",") != "instant_transfer_convert_reversal,instant_transfer_move_reversal" {
			t.Fatalf("the reversal entries are %v (%v), want the move and the conversion reversed", kinds, err)
		}
	})

	t.Run("pounds into a UK bank account at a direct quote are paid straight, in one leg", func(t *testing.T) {
		q := quote("GBP", "GB")
		if code, _, _ := send(q.ID, british.ID, "to-gb-first"); code != http.StatusForbidden {
			t.Fatalf("a first send to a new bank payee: %d, want it to wait for the owner's approval", code)
		}
		approve()
		before := balance(from.ID)
		code, body, it := send(quote("GBP", "GB").ID, british.ID, "to-gb")
		if code != http.StatusOK || it.Status != economy.InstantReceived || it.FeeMinor != 0 || it.ReceiveMinor != 100_00 || it.ArrivesWithinSeconds != 2*60*60 {
			t.Fatalf("the transfer: %d %v, want £100.00 paid straight, no fee, within 2 hours", code, body)
		}
		if len(it.Legs) != 1 || it.Legs[0].Leg != economy.InstantLegPayout || !it.Legs[0].Done || it.Legs[0].PartnerRef == "" {
			t.Fatalf("the legs: %+v, want one payout", it.Legs)
		}
		if minor, currency := paidOut(it.Legs[0].EntryID); minor != 100_00 || currency != "GBP" || balance(from.ID) != before-100_00 {
			t.Fatalf("paid out %d %s and the agent holds %d, want 10000 GBP out and %d left", minor, currency, balance(from.ID), before-100_00)
		}
	})
}
