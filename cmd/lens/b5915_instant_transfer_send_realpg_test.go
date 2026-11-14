package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// failAfterLeg is the Sandbox's transfer hooks, kept in Postgres, with a switch: once the leg named by after is
// recorded as done, every later call of the leg fail names fails — so a test fails a transfer's second or third leg.
type failAfterLeg struct {
	partners.TransferHooks
	sandbox *money.Adapter

	mu         sync.Mutex
	after, key string // the leg (its capability) and the key suffix that, done, arms the failure
	fail       money.Capability
}

func (h *failAfterLeg) arm(afterLeg, keySuffix string, fail money.Capability) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.after, h.key, h.fail = afterLeg, keySuffix, fail
}

func (h *failAfterLeg) disarm(t *testing.T) {
	t.Helper()
	h.arm("", "", "")
	for _, c := range []money.Capability{money.CapConvert, money.CapOnchain, money.CapBankPayout} {
		if err := money.FailAt(h.sandbox, c, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *failAfterLeg) RecordLeg(ctx context.Context, l partners.LegRecord) error {
	err := h.TransferHooks.RecordLeg(ctx, l)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.after != "" && l.Leg == h.after && strings.HasSuffix(l.Key, h.key) && l.Outcome == "ok" {
		_ = money.FailAt(h.sandbox, h.fail, money.ErrRefused)
	}
	return err
}

// B59.15 on real Postgres — an Instant transfer to a Talyvor account (launch pack 4.1, 4.2 (1), 4.3), through the money
// kind gate and the handler as main.go mounts them, read off the ledger. At the Test FX partner's rates ($1.25 a pound,
// $1.10 a euro) and a 50 basis point fee, £100.00 sends £99.50 as 124.375 USDC, which lands as €113.06 in a hybrid
// account or as the USDC in a stablecoin one: one id, three legs in ledger order that add up to the quote. A repeat with
// the same key moves money once; a failed second leg and a failed third leg each leave the agent's balance exactly as
// it was; an agent without "Can use Instant transfer" is refused with that field; and a fiat or stablecoin workspace
// cannot reach the route.
func TestB5915_InstantTransferToATalyvorAccount(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const sender = "ws-b5915-sender"
	for ws, kind := range map[string]string{sender: "hybrid", "ws-b5915-hybrid": "hybrid", "ws-b5915-coin": "stablecoin",
		"ws-b5915-fiat": "fiat"} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, $2, $1, 'business', $3)`,
			ws, strings.TrimPrefix(ws, "ws-b5915-")+" Ltd", kind); err != nil {
			t.Fatal(err)
		}
	}
	registry := partners.NewRegistry(nil)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetScreener(screening.NewScreener(pool, registry))
	store.SetFXMargin(func(context.Context, economy.FeeQuerier, string) (int64, string, error) { return 50, "team", nil })
	sandbox := money.NewSandbox(registry, "b5915")
	hooks := &failAfterLeg{TransferHooks: store.InstantTransferHooks(&partners.TestTransferHooks{}), sandbox: sandbox}
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
	euros, err := store.OpenCurrencyAccount(ctx, "ws-b5915-hybrid", "", "EUR", "test")
	if err != nil {
		t.Fatal(err)
	}
	usdc, err := store.OpenCurrencyAccount(ctx, "ws-b5915-coin", "", "USDC", "test")
	if err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			owner := &auth.AuthContext{WorkspaceID: req.Header.Get("X-Test-Workspace"), AuthMethod: auth.MethodJWT, UserID: "owner",
				Scopes: []string{auth.ScopeKeys}}
			next.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), owner)))
		})
	})
	r.Group(func(g chi.Router) {
		g.Use(moneyKindGate(store))
		mountInstantTransferRoutes(g, store)
	})
	call := func(ws, method, path, key string, body any) (int, map[string]any, economy.InstantTransfer) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
		req.Header.Set("X-Test-Workspace", ws)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var fields map[string]any
		var it economy.InstantTransfer
		_ = json.Unmarshal(rec.Body.Bytes(), &fields)
		_ = json.Unmarshal(rec.Body.Bytes(), &it)
		return rec.Code, fields, it
	}
	quote := func(receive string) economy.InstantQuote {
		t.Helper()
		in := economy.InstantQuoteRequest{SendMinor: 100_00, SendCurrency: "GBP", ReceiveCurrency: receive, To: economy.InstantToAccount}
		raw, _ := json.Marshal(in)
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+sender+"/money/instant-transfers/quote", strings.NewReader(string(raw)))
		req.Header.Set("X-Test-Workspace", sender)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var q economy.InstantQuote
		if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil || rec.Code != http.StatusCreated {
			t.Fatalf("quote: %d %s", rec.Code, rec.Body.String())
		}
		return q
	}
	sendPath := "/v1/workspaces/" + sender + "/agents/" + agent.ID + "/money/instant-transfers"
	send := func(q economy.InstantQuote, to, key string) (int, map[string]any, economy.InstantTransfer) {
		t.Helper()
		return call(sender, http.MethodPost, sendPath, key, economy.InstantSend{QuoteID: q.ID, ToAccountID: to})
	}
	balance := func(account string) int64 {
		t.Helper()
		var b int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1`, account).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	entries := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_entries WHERE kind LIKE 'instant_transfer_%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// approved sends to an account the agent has never paid: the first send files the owner's approval and moves
	// nothing; once the owner approves, a send at a new quote goes through.
	approved := func(receive, to, key string) economy.InstantTransfer {
		t.Helper()
		before, moved := balance(from.ID), entries()
		code, body, _ := send(quote(receive), to, key+"-first")
		if code != http.StatusForbidden || !strings.Contains(body["error"].(string), "approv") {
			t.Fatalf("a first send to a new account: %d %v, want it to wait for the owner's approval", code, body)
		}
		if balance(from.ID) != before || entries() != moved {
			t.Fatalf("a send waiting for approval moved money: %d → %d, %d → %d entries", before, balance(from.ID), moved, entries())
		}
		var approval string
		if err := pool.QueryRow(ctx, `SELECT id FROM agent_approvals WHERE agent_id = $1 AND status = 'pending' ORDER BY created_at DESC LIMIT 1`,
			agent.ID).Scan(&approval); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DecideAgentApproval(ctx, sender, approval, true); err != nil {
			t.Fatal(err)
		}
		code, body, it := send(quote(receive), to, key)
		if code != http.StatusOK {
			t.Fatalf("the approved send: %d %v", code, body)
		}
		return it
	}

	t.Run("a fiat or stablecoin workspace cannot reach the route", func(t *testing.T) {
		for _, ws := range []string{"ws-b5915-fiat", "ws-b5915-coin"} {
			code, body, _ := call(ws, http.MethodPost, "/v1/workspaces/"+ws+"/agents/any/money/instant-transfers", "k", economy.InstantSend{QuoteID: "itq_x", ToAccountID: euros.ID})
			if code != http.StatusConflict || body["code"] != "wrong_money_kind" {
				t.Errorf("%s: %d %v, want 409 wrong_money_kind", ws, code, body)
			}
			_, err := store.SendInstantTransfer(ctx, ws, "any", economy.InstantSend{QuoteID: "itq_x", ToAccountID: euros.ID, IdempotencyKey: "k"})
			var refusal *economy.MoneyKindRefusal
			if !errors.As(err, &refusal) || refusal.Field != "instant_transfer" {
				t.Errorf("%s past the route: %v, want a refusal on instant_transfer", ws, err)
			}
		}
	})

	t.Run("an agent without the rule is refused with a field", func(t *testing.T) {
		code, body, _ := send(quote("EUR"), euros.ID, "no-rule")
		if code != http.StatusForbidden || body["code"] != "agent_rule" || body["field"] != economy.FieldMayInstantTransfer {
			t.Fatalf("an agent without Can use Instant transfer: %d %v, want 403 agent_rule on may_instant_transfer", code, body)
		}
		if balance(from.ID) != 1_000_00 || entries() != 0 {
			t.Fatalf("the refused send moved money")
		}
	})

	on := true
	if _, err := store.SetAgentRules(ctx, sender, agent.ID, economy.AgentRules{MayInstantTransfer: &on}); err != nil {
		t.Fatal(err)
	}

	t.Run("to a hybrid account: converted on arrival, once under its key", func(t *testing.T) {
		it := approved("EUR", euros.ID, "to-euros")
		if it.Status != economy.InstantReceived || it.SendMinor != 100_00 || it.FeeMinor != 50 || it.ReceiveMinor != 113_06 || it.ReceiveCurrency != "EUR" {
			t.Fatalf("the transfer: %+v, want £100.00 sent, a 50p fee and €113.06 received", it)
		}
		if balance(from.ID) != 900_00 || balance(euros.ID) != 113_06 {
			t.Fatalf("balances: the agent %d, the beneficiary %d; want 90000 and 11306", balance(from.ID), balance(euros.ID))
		}
		// One id, three legs in ledger order, each its own money entry, adding up to the quote.
		rows, err := pool.Query(ctx, `SELECT e.id, e.kind, p.account_id, p.currency, p.amount_minor FROM money_entries e
			JOIN money_postings p ON p.entry_id = e.id WHERE e.idempotency_key LIKE $1 || ':%' ORDER BY e.created_at, e.id, p.line`, it.ID)
		if err != nil {
			t.Fatal(err)
		}
		var kinds []string
		moved := map[string]int64{}
		for rows.Next() {
			var id, kind, account, currency string
			var amount int64
			if err := rows.Scan(&id, &kind, &account, &currency, &amount); err != nil {
				t.Fatal(err)
			}
			if len(kinds) == 0 || kinds[len(kinds)-1] != kind {
				kinds = append(kinds, kind)
			}
			moved[account+" "+currency] += amount
		}
		rows.Close()
		if strings.Join(kinds, ",") != "instant_transfer_convert,instant_transfer_move,instant_transfer_credit" {
			t.Errorf("the legs in ledger order: %v", kinds)
		}
		if moved[from.ID+" GBP"] != -100_00 || moved[euros.ID+" EUR"] != 113_06 {
			t.Errorf("the legs moved %v; want £100.00 out of the agent and €113.06 into the beneficiary", moved)
		}
		if len(it.Legs) != 3 || it.Legs[0].ID != it.ID+":convert" || it.Legs[1].ID != it.ID+":move" || it.Legs[2].ID != it.ID+":credit" ||
			it.Legs[0].Buy.Minor != 124_375000 || it.Legs[1].Sell.Minor != 124_375000 || it.Legs[2].Buy.Minor != 113_06 {
			t.Errorf("the legs: %+v", it.Legs)
		}
		for _, l := range it.Legs {
			if !l.Done || l.EntryID == "" || l.PartnerRef == "" {
				t.Errorf("leg %s: %+v, want done with its entry and the provider's reference", l.ID, l)
			}
		}
		// The same key again answers the same transfer and moves nothing more.
		n := entries()
		if code, body, _ := send(quote("EUR"), euros.ID, "to-euros"); code != http.StatusConflict {
			t.Fatalf("the same key for another send: %d %v, want 409", code, body)
		}
		var q economy.InstantQuote
		q.ID = it.QuoteID
		code, _, again := send(q, euros.ID, "to-euros")
		if code != http.StatusOK || again.ID != it.ID || again.Status != economy.InstantReceived {
			t.Fatalf("a repeat with the same key: %d %+v, want the first transfer", code, again)
		}
		if entries() != n || balance(from.ID) != 900_00 || balance(euros.ID) != 113_06 {
			t.Fatalf("a repeat with the same key moved money")
		}
		// The receipt.
		code, body, receipt := call(sender, http.MethodGet, "/v1/workspaces/"+sender+"/money/instant-transfers/"+it.ID, "", nil)
		if code != http.StatusOK || receipt.Legs[1].PartnerRef == "" || receipt.Legs[0].PartnerRef == "" || receipt.Legs[2].PartnerRef == "" {
			t.Errorf("the receipt: %d %v, want both conversions and the on-chain reference", code, body)
		}
	})

	t.Run("to a stablecoin account: the USDC is kept", func(t *testing.T) {
		it := approved("USDC", usdc.ID, "to-coin")
		if it.Status != economy.InstantReceived || it.ReceiveMinor != 124_375000 || balance(usdc.ID) != 124_375000 || balance(from.ID) != 800_00 {
			t.Fatalf("the transfer %+v; the stablecoin account holds %d, the agent %d", it, balance(usdc.ID), balance(from.ID))
		}
	})

	t.Run("a failed second leg leaves the agent's balance exactly as before", func(t *testing.T) {
		defer hooks.disarm(t)
		hooks.arm(string(money.CapConvert), ":convert", money.CapOnchain)
		before, beneficiary := balance(from.ID), balance(euros.ID)
		code, body, it := send(quote("EUR"), euros.ID, "fail-move")
		if code != http.StatusOK || it.Status != economy.InstantReversed || it.FailedLeg != economy.InstantLegMove || len(it.ReversalEntries) != 1 {
			t.Fatalf("the transfer: %d %v, want it reversed at the move leg", code, body)
		}
		if balance(from.ID) != before || balance(euros.ID) != beneficiary {
			t.Fatalf("after a failed second leg the agent has %d (was %d) and the beneficiary %d (was %d)", balance(from.ID), before, balance(euros.ID), beneficiary)
		}
	})

	t.Run("a failed third leg leaves the agent's balance exactly as before", func(t *testing.T) {
		defer hooks.disarm(t)
		hooks.arm(string(money.CapOnchain), ":move", money.CapConvert)
		before, beneficiary := balance(from.ID), balance(euros.ID)
		code, body, it := send(quote("EUR"), euros.ID, "fail-credit")
		if code != http.StatusOK || it.Status != economy.InstantReversed || it.FailedLeg != economy.InstantLegCredit || len(it.ReversalEntries) != 2 {
			t.Fatalf("the transfer: %d %v, want it reversed at the credit leg, both earlier legs back", code, body)
		}
		if balance(from.ID) != before || balance(euros.ID) != beneficiary {
			t.Fatalf("after a failed third leg the agent has %d (was %d) and the beneficiary %d (was %d)", balance(from.ID), before, balance(euros.ID), beneficiary)
		}
		// Sent again with the same key, a reversed transfer stays reversed and moves nothing.
		n := entries()
		var q economy.InstantQuote
		q.ID = it.QuoteID
		if code, _, again := send(q, euros.ID, "fail-credit"); code != http.StatusOK || again.ID != it.ID || again.Status != economy.InstantReversed || entries() != n {
			t.Fatalf("a repeat of a reversed transfer: %d %+v", code, again)
		}
	})
}
