package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/issuerfake"
	"github.com/talyvor/lens/internal/partners"
)

// B61.43 — an agent's rules hold the merchant categories it may pay in and those it may never pay in, judged on a
// card payment by the merchant's category before any money moves. An agent allowed only software has a charge at a
// freight carrier refused, and the ledger gains no row for it; a charge at a software merchant goes through. The rules
// read back as they were saved, and a category blocked once refuses its merchants too.
func TestB6143_AnAgentsRulesHoldMerchantsAndCategories(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b6143"
	var issuer *issuerfake.Issuer
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b6143"), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
				Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool),
				Switches: money.SwitchSet{{Capability: money.CapCards, Partner: issuerfake.Name}: true}})
			if err != nil {
				t.Fatal(err)
			}
			issuer = issuerfake.New(store.AuthorizeCardCharge)
			if err := layer.Register(issuer.Adapter()); err != nil {
				t.Fatal(err)
			}
			return layer
		})
	agent := b.acct.AgentID
	card, err := b.store.IssueReusableCard(ctx, ws, economy.ReusableCard{AgentID: agent, Currency: "GBP", PerPaymentLimitMinor: 10000,
		DailyLimitMinor: 40000, IdempotencyKey: "agent-card"})
	if err != nil || card.Status != economy.ReusableCardActive {
		t.Fatalf("issue the agent's card = %+v %v, want active", card, err)
	}
	var ref string
	if err := pool.QueryRow(ctx, `SELECT partner_ref FROM money_reusable_cards WHERE id = $1`, card.ID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	charge := func(merchant string, wantOK bool, wantReason string) {
		t.Helper()
		ok, reason, err := issuer.Charge(ctx, ref, merchant, money.Amount{Minor: 1000, Asset: "GBP"})
		if err != nil || ok != wantOK || !strings.Contains(reason, wantReason) {
			t.Fatalf("%s = %v %q %v, want %v %q", merchant, ok, reason, err, wantOK, wantReason)
		}
	}

	saved, err := b.store.SetAgentRules(ctx, ws, agent, economy.AgentRules{Timezone: "UTC", AllowedMerchantCategories: []string{"software"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.store.GetAgentRules(ctx, ws, agent)
	if err != nil || !slices.Equal(got.AllowedMerchantCategories, []string{"software"}) || got.BlockedMerchantCategories == nil ||
		len(got.BlockedMerchantCategories) != 0 || !slices.Equal(saved.AllowedMerchantCategories, got.AllowedMerchantCategories) {
		t.Fatalf("rules read back = %+v %v, want software allowed and nothing blocked", got, err)
	}
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), `"allowed_merchant_categories":["software"]`) || !strings.Contains(string(raw), `"blocked_merchant_categories":[]`) {
		t.Fatalf("GET rules answers %s, want allowed_merchant_categories and blocked_merchant_categories", raw)
	}

	charge("DHL Freight", false, "outside the agent's merchants and categories")
	charge("OpenAI", true, "") // computer software, 5734

	// A save that leaves the categories out keeps them: a client that predates them cannot clear them.
	if kept, err := b.store.SetAgentRules(ctx, ws, agent, economy.AgentRules{Timezone: "UTC"}); err != nil ||
		!slices.Equal(kept.AllowedMerchantCategories, []string{"software"}) {
		t.Fatalf("a save without the categories = %+v %v, want software still allowed", kept, err)
	}
	if _, err := b.store.SetAgentRules(ctx, ws, agent, economy.AgentRules{Timezone: "UTC", AllowedMerchantCategories: []string{},
		BlockedMerchantCategories: []string{"freight"}}); err != nil {
		t.Fatal(err)
	}
	charge("DHL Freight", false, "outside the agent's merchants and categories")
	charge("Tesco", true, "") // groceries, blocked by nothing

	b.balance(t, 50000-1000-1000)
	if got, want := b4610Postings(t, pool, ws), "card_charge agent -1000; card_charge partner 1000; card_charge agent -1000; card_charge partner 1000"; got != want {
		t.Fatalf("the card posted %q, want %q: nothing for the refused freight charges", got, want)
	}
	var refused int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_card_authorizations WHERE card_id = $1 AND NOT approved AND mcc = '4214'`, card.ID).
		Scan(&refused); err != nil || refused != 2 {
		t.Fatalf("refused freight authorisations = %d %v, want 2 recorded declined", refused, err)
	}
}

// B61.43 — a one-time card the owner approved for an agent pays only in the agent's merchant categories too: its charge
// at a freight carrier, when the agent may pay only in software, is refused and spends nothing from the card's hold.
func TestB6143_AOneTimeCardFollowsTheAgentsMerchantCategories(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b6143-once"
	var issuer *issuerfake.Issuer
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b6143_once"), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
				Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool),
				Switches: money.SwitchSet{{Capability: money.CapCards, Partner: issuerfake.Name}: true}})
			if err != nil {
				t.Fatal(err)
			}
			issuer = issuerfake.New(store.AuthorizeCardCharge)
			if err := layer.Register(issuer.Adapter()); err != nil {
				t.Fatal(err)
			}
			return layer
		})
	if _, err := b.store.SetAgentRules(ctx, ws, b.acct.AgentID, economy.AgentRules{Timezone: "UTC", AllowedMerchantCategories: []string{"software"}}); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountMoneyCardRoutes(r, b.store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, economy.CardBatch) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(auth.WithAuthContext(ctx, owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out economy.CardBatch
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	code, batch := call(http.MethodPost, "/v1/money/cards", `{"agent_id":"`+b.acct.AgentID+`","purpose":"Ship the servers","currency":"GBP",
		"idempotency_key":"ship","cards":[{"merchant":"DHL Freight","cap_minor":5000}]}`)
	if code == http.StatusCreated && batch.Status == economy.CardBatchAwaitingApproval {
		code, batch = call(http.MethodPost, "/v1/money/cards/"+batch.ID+"/approve", "")
	}
	if code != http.StatusOK || batch.Status != economy.CardBatchIssued || len(batch.Cards) != 1 {
		t.Fatalf("ask for and approve a card at DHL Freight = %d %+v, want it issued", code, batch)
	}
	ok, reason, err := issuer.Charge(ctx, issuerRef(t, pool, batch.Cards[0].ID), "DHL Freight", money.Amount{Minor: 4000, Asset: "GBP"})
	if err != nil || ok || !strings.Contains(reason, "outside the agent's merchants and categories") {
		t.Fatalf("the freight charge = %v %q %v, want it refused as outside the agent's merchants and categories", ok, reason, err)
	}
	if got, want := b4610Postings(t, pool, ws), "card_hold agent -5000; card_hold hold 5000"; got != want {
		t.Fatalf("the card posted %q, want %q: the hold, and nothing for the refused charge", got, want)
	}
}
