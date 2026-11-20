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

// B59.16 on real Postgres — an Instant transfer to an outside wallet (launch pack 4.2 (2)), through the money kind gate
// and the handler as main.go mounts them, read off the ledger. The agent names a saved payee at a USDC address; at the
// Test FX partner's $1.25 a pound and a 50 basis point fee, £100.00 goes to the address as 124.375 USDC, in two legs
// (convert, move) with the originator's and the beneficiary's details attached to the move. An address the transfer
// hooks' screening flags is refused with a field before any leg moves, and a failed on-chain leg reverses the
// conversion and leaves the agent exactly as it was.
func TestB5916_InstantTransferToAnOutsideWallet(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const sender = "ws-b5916-sender"
	const wallet = "0x00000000000000000000000000000000000b5916"
	const flagged = "0x00000000000000000000000000000000000bad16"
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
	sandbox := money.NewSandbox(registry, "b5916")
	details := &partners.TestTransferHooks{Screening: partners.TestScreeningProvider{Addresses: map[string]string{flagged: partners.AddressSanctioned}}}
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
	payee := func(name, address string) economy.OutsidePayee {
		t.Helper()
		p, err := store.CreateOutsidePayee(ctx, sender, economy.OutsidePayee{Name: name, Country: "DE", Network: "base", Address: address})
		if err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
		// The owner approves the address with a passkey (B30.16's confirmation, tested there).
		if _, err := pool.Exec(ctx, `UPDATE money_payees SET confirmed_at = now(), confirmed_by = 'passkey' WHERE id = $1`, p.ID); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good, bad := payee("Wallet GmbH", wallet), payee("Other GmbH", flagged)

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
	quote := func() economy.InstantQuote {
		t.Helper()
		raw, _ := json.Marshal(economy.InstantQuoteRequest{SendMinor: 100_00, SendCurrency: "GBP", ReceiveCurrency: "USDC", To: economy.InstantToWallet})
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
	balance := func(account string) int64 {
		t.Helper()
		var b int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1`, account).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	moved := func() (entries, legs int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM money_entries WHERE kind LIKE 'instant_transfer_%'),
			(SELECT count(*) FROM instant_transfer_legs WHERE workspace_id = $1 AND method <> 'quote')`, sender).Scan(&entries, &legs); err != nil {
			t.Fatal(err)
		}
		return entries, legs
	}

	t.Run("a screened-out address is refused before any leg moves", func(t *testing.T) {
		entries, legs := moved()
		code, body, _ := send(quote().ID, bad.ID, "flagged")
		if code != http.StatusForbidden || body["code"] != "screened" || body["field"] != "to_payee_id" {
			t.Fatalf("a send to a sanctioned address: %d %v, want 403 screened on to_payee_id", code, body)
		}
		var transfers int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM instant_transfers WHERE workspace_id = $1`, sender).Scan(&transfers); err != nil {
			t.Fatal(err)
		}
		if e, l := moved(); balance(from.ID) != 1_000_00 || e != entries || l != legs || transfers != 0 {
			t.Fatalf("the refused send moved money: balance %d, %d → %d entries, %d → %d legs, %d transfers", balance(from.ID), entries, e, legs, l, transfers)
		}
	})

	var sent economy.InstantTransfer
	t.Run("to an outside wallet: the USDC goes to the address with both sides' details, once under its key", func(t *testing.T) {
		// A payee the agent has never sent an Instant transfer to waits for the owner's approval.
		if code, body, _ := send(quote().ID, good.ID, "to-wallet-first"); code != http.StatusForbidden || !strings.Contains(body["error"].(string), "approv") {
			t.Fatalf("a first send to a new wallet: %d %v, want it to wait for the owner's approval", code, body)
		}
		var approval string
		if err := pool.QueryRow(ctx, `SELECT id FROM agent_approvals WHERE agent_id = $1 AND status = 'pending' ORDER BY created_at DESC LIMIT 1`,
			agent.ID).Scan(&approval); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DecideAgentApproval(ctx, sender, approval, true); err != nil {
			t.Fatal(err)
		}
		code, body, it := send(quote().ID, good.ID, "to-wallet")
		if code != http.StatusOK || it.Status != economy.InstantReceived || it.ToAddress != wallet || it.ToNetwork != "base" || it.ToAccountID != "" ||
			it.SendMinor != 100_00 || it.FeeMinor != 50 || it.ReceiveMinor != 124_375000 || it.ReceiveCurrency != "USDC" {
			t.Fatalf("the transfer: %d %v, want £100.00 sent, a 50p fee and 124.375 USDC received at the address", code, body)
		}
		sent = it
		if len(it.Legs) != 2 || it.Legs[0].ID != it.ID+":convert" || it.Legs[1].ID != it.ID+":move" || it.Legs[1].Buy.Minor != 124_375000 {
			t.Fatalf("the legs: %+v, want convert and move", it.Legs)
		}
		for _, l := range it.Legs {
			if !l.Done || l.EntryID == "" || l.PartnerRef == "" {
				t.Errorf("leg %s: %+v, want done with its entry and the provider's reference", l.ID, l)
			}
		}
		if balance(from.ID) != 900_00 {
			t.Fatalf("the agent holds %d, want 90000", balance(from.ID))
		}
		// On the ledger: £100.00 out of the agent, and the 124.375 USDC out of Talyvor's hold to the chain.
		var chain int64
		if err := pool.QueryRow(ctx, `SELECT p.amount_minor FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
			WHERE e.id = $1 AND p.amount_minor > 0`, it.Legs[1].EntryID).Scan(&chain); err != nil || chain != 124_375000 {
			t.Fatalf("the move leg's entry put %d on chain (%v), want 124375000", chain, err)
		}
		// The originator's and the beneficiary's details went with the move, to the address.
		var carried *partners.TravelRuleTransfer
		for _, d := range details.Details() {
			if d.ID == it.ID+":move" {
				carried = &d
			}
		}
		if carried == nil || carried.Originator.Name != "Sender Ltd" || carried.Beneficiary.Name != "Wallet GmbH" || carried.Beneficiary.Country != "DE" ||
			carried.Beneficiary.Address != wallet || carried.AmountMinor != 124_375000 {
			t.Fatalf("the details on chain: %+v, want Sender Ltd to Wallet GmbH (DE) at %s for 124.375 USDC", carried, wallet)
		}
		// The same key again answers the same transfer and moves nothing more.
		entries, _ := moved()
		if code, _, again := send(it.QuoteID, good.ID, "to-wallet"); code != http.StatusOK || again.ID != it.ID || again.Status != economy.InstantReceived {
			t.Fatalf("a repeat with the same key: %d %+v, want the first transfer", code, again)
		}
		if e, _ := moved(); e != entries || balance(from.ID) != 900_00 {
			t.Fatalf("a repeat with the same key moved money")
		}
	})

	t.Run("a failed on-chain leg reverses the conversion and leaves the agent whole", func(t *testing.T) {
		if sent.ID == "" {
			t.Fatal("the wallet was never paid, so this send would wait for an approval")
		}
		defer hooks.disarm(t)
		if err := money.FailAt(sandbox, money.CapOnchain, money.ErrRefused); err != nil {
			t.Fatal(err)
		}
		before := balance(from.ID)
		code, body, it := send(quote().ID, good.ID, "fail-move")
		if code != http.StatusOK || it.Status != economy.InstantReversed || it.FailedLeg != economy.InstantLegMove || len(it.ReversalEntries) != 1 {
			t.Fatalf("the transfer: %d %v, want it reversed at the move leg, the conversion back", code, body)
		}
		if balance(from.ID) != before {
			t.Fatalf("after a failed on-chain leg the agent has %d, was %d", balance(from.ID), before)
		}
		var kind string
		if err := pool.QueryRow(ctx, `SELECT kind FROM money_entries WHERE id = $1`, it.ReversalEntries[0]).Scan(&kind); err != nil || kind != "instant_transfer_convert_reversal" {
			t.Fatalf("the reversal entry is %q (%v), want the conversion reversed", kind, err)
		}
	})
}
