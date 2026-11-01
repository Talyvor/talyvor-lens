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
	"github.com/talyvor/lens/internal/money/issuerfake"
	"github.com/talyvor/lens/internal/partners"
)

// B53.2 (and B28.322) — against the fake issuer, through the routes the app uses: the owner issues their own reusable
// card with a limit per payment, a daily limit and a blocked category. It pays again and again; a charge over either
// limit, in the blocked category or, once gambling is blocked in one tap, at a betting shop is declined by Lens and
// writes no posting; a frozen card declines everything and unfrozen pays again. An agent's reusable card spends from
// the agent's own account.
func TestB532_TheOwnersReusableCardPaysWithinItsLimitsAndCategoriesAndFreezes(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b532"
	var issuer *issuerfake.Issuer
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b532"), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
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
	var company, partner string
	if err := pool.QueryRow(ctx, `SELECT (SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'company' AND currency = 'GBP'),
		(SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = 'GBP')`, ws).Scan(&company, &partner); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "owner-top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partner, AmountMinor: -20000}, {AccountID: company, AmountMinor: 20000}}}); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountReusableCardRoutes(r, b.store, true)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, economy.ReusableCard) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out economy.ReusableCard
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	charge := func(card economy.ReusableCard, merchant string, minor int64, wantOK bool, wantReason string) {
		t.Helper()
		var ref string
		if err := pool.QueryRow(ctx, `SELECT partner_ref FROM money_reusable_cards WHERE id = $1`, card.ID).Scan(&ref); err != nil {
			t.Fatal(err)
		}
		ok, reason, err := issuer.Charge(ctx, ref, merchant, money.Amount{Minor: minor, Asset: "GBP"})
		if err != nil || ok != wantOK || !strings.Contains(reason, wantReason) {
			t.Fatalf("%d at %s = %v %q %v, want %v %q", minor, merchant, ok, reason, err, wantOK, wantReason)
		}
	}

	for range 2 { // asked twice under one key: one card
		code, card := call(http.MethodPost, "/v1/money/reusable-cards", `{"currency":"GBP","per_payment_limit_minor":5000,"daily_limit_minor":8000,
			"blocked_mccs":["5541"],"idempotency_key":"my-card"}`)
		if code != http.StatusCreated || card.Status != economy.ReusableCardActive || card.Partner != issuerfake.Name || card.AgentID != "" || card.BlocksGambling {
			t.Fatalf("issue the owner's card = %d %+v, want an active card on the fake issuer", code, card)
		}
	}
	cards, err := b.store.ReusableCards(ctx, ws, "")
	if err != nil || len(cards) != 1 {
		t.Fatalf("cards = %+v %v, want one", cards, err)
	}
	mine := cards[0]

	charge(mine, "Tesco", 3000, true, "")
	charge(mine, "Tesco", 3000, true, "") // reusable: it pays again
	charge(mine, "Tesco", 6000, false, "limit of GBP 50.00 a payment")
	charge(mine, "Pret A Manger", 2500, false, "daily limit of GBP 80.00") // 6000 spent today
	charge(mine, "Shell", 1000, false, "blocked in merchant category 5541")

	code, got := call(http.MethodPatch, "/v1/money/reusable-cards/"+mine.ID, `{"blocks_gambling":true}`)
	if code != http.StatusOK || !got.BlocksGambling || !strings.Contains(strings.Join(got.BlockedMCCs, ","), "5541") {
		t.Fatalf("block gambling = %d %+v, want gambling blocked beside 5541", code, got)
	}
	charge(mine, "William Hill", 1000, false, "blocked from gambling")

	if code, got = call(http.MethodPatch, "/v1/money/reusable-cards/"+mine.ID, `{"frozen":true}`); code != http.StatusOK || got.Status != economy.ReusableCardFrozen {
		t.Fatalf("freeze = %d %+v, want frozen", code, got)
	}
	charge(mine, "Tesco", 500, false, "frozen")
	if code, got = call(http.MethodPatch, "/v1/money/reusable-cards/"+mine.ID, `{"frozen":false}`); code != http.StatusOK || got.Status != economy.ReusableCardActive {
		t.Fatalf("unfreeze = %d %+v, want active", code, got)
	}
	charge(mine, "Tesco", 500, true, "")

	// An agent's reusable card spends from the agent's own account.
	code, theirs := call(http.MethodPost, "/v1/money/reusable-cards", `{"agent_id":"`+b.acct.AgentID+`","currency":"GBP","per_payment_limit_minor":2000,
		"daily_limit_minor":2000,"allowed_mccs":["5734"],"idempotency_key":"agent-card"}`)
	if code != http.StatusCreated || theirs.Status != economy.ReusableCardActive || theirs.AgentID != b.acct.AgentID {
		t.Fatalf("issue the agent's card = %d %+v, want an active card for the agent", code, theirs)
	}
	charge(theirs, "Tesco", 100, false, "does not pay in merchant category 5411")
	charge(theirs, "OpenAI", 1500, true, "")
	b.balance(t, 48500)

	// Only approved charges posted: what each cost, from the card's account to the partner.
	if got, want := b4610Postings(t, pool, ws), "card_charge company -3000; card_charge partner 3000; card_charge company -3000; card_charge partner 3000; "+
		"card_charge company -500; card_charge partner 500; card_charge agent -1500; card_charge partner 1500"; got != want {
		t.Fatalf("the cards posted %q, want %q", got, want)
	}
}
