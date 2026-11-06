package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
	ctx := context.Background()
	const ws = "ws-b532"
	pool, b, _, call, charge := b532Cards(t, ws)

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
	charge(mine, "Unlabelled Casino", 1000, false, "merchant category is unknown") // a block cannot be dodged by leaving the category out

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

type b532Call func(method, path, body string) (int, economy.ReusableCard)
type b532Charge func(card economy.ReusableCard, merchant string, minor int64, wantOK bool, wantReason string)

// b532Cards is workspace ws with GBP 200.00 in the owner's account, the fake issuer serving cards, the reusable-card
// routes called as the owner, and a charge on a card at a merchant that must end as wanted.
func b532Cards(t *testing.T, ws string) (*pgxpool.Pool, b468Payer, *issuerfake.Issuer, b532Call, b532Charge) {
	t.Helper()
	pool := agentRoutesDB(t)
	ctx := context.Background()
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
	return pool, b, issuer, call, charge
}

// B54.3 — the same idempotency key asked again with other merchant categories, or another gambling block, is a
// different card, so it is refused rather than answered with the first card.
func TestB543_AKeyReusedWithOtherCategoriesIsRefused(t *testing.T) {
	_, _, _, call, _ := b532Cards(t, "ws-b543-key")
	const card = `"currency":"GBP","per_payment_limit_minor":5000,"daily_limit_minor":8000,"idempotency_key":"k"`
	if code, _ := call(http.MethodPost, "/v1/money/reusable-cards", `{`+card+`,"blocked_mccs":["5541"]}`); code != http.StatusCreated {
		t.Fatalf("issue = %d, want 201", code)
	}
	for _, other := range []string{`"blocked_mccs":["5542"]`, `"blocked_mccs":["5541"],"allowed_mccs":["5411"]`, `"blocked_mccs":["5541"],"blocks_gambling":true`} {
		if code, got := call(http.MethodPost, "/v1/money/reusable-cards", `{`+card+`,`+other+`}`); code != http.StatusConflict {
			t.Fatalf("the same key with %s = %d %+v, want 409 (the key was used for another card)", other, code, got)
		}
	}
	if code, _ := call(http.MethodPost, "/v1/money/reusable-cards", `{`+card+`,"blocked_mccs":[" 5541","5541"]}`); code != http.StatusCreated {
		t.Fatalf("the same card asked again = %d, want 201", code)
	}
}

// B54.3 — a new daily limit is the issuer's cap too: the issuer is asked to hold the new cap, and a card the issuer
// refuses to change keeps its old limit, with the issuer's reason.
func TestB543_ANewDailyLimitMovesTheIssuersCapOrNothing(t *testing.T) {
	ctx := context.Background()
	pool, b, issuer, call, charge := b532Cards(t, "ws-b543-cap")
	code, card := call(http.MethodPost, "/v1/money/reusable-cards", `{"currency":"GBP","per_payment_limit_minor":5000,"daily_limit_minor":8000,"idempotency_key":"cap"}`)
	if code != http.StatusCreated {
		t.Fatalf("issue = %d %+v", code, card)
	}
	var ref string
	if err := pool.QueryRow(ctx, `SELECT partner_ref FROM money_reusable_cards WHERE id = $1`, card.ID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	capOf := func() int64 { c, _ := issuer.Card(ref); return c.Cap.Minor }

	if code, got := call(http.MethodPatch, "/v1/money/reusable-cards/"+card.ID, `{"daily_limit_minor":12000}`); code != http.StatusOK || got.DailyLimitMinor != 12000 {
		t.Fatalf("raise the daily limit = %d %+v, want 12000", code, got)
	}
	if got := capOf(); got != 12000 {
		t.Fatalf("the issuer holds a cap of %d, want the new daily limit 12000", got)
	}
	charge(card, "Tesco", 5000, true, "")
	charge(card, "Tesco", 5000, true, "") // 10000 today: past the old 8000, within the new 12000

	if _, err := issuer.Close(ctx, "close-at-issuer", ref); err != nil {
		t.Fatal(err)
	}
	if code, got := call(http.MethodPatch, "/v1/money/reusable-cards/"+card.ID, `{"daily_limit_minor":20000}`); code == http.StatusOK {
		t.Fatalf("a daily limit the issuer refused = %d %+v, want an error", code, got)
	}
	cards, err := b.store.ReusableCards(ctx, "ws-b543-cap", "")
	if err != nil || len(cards) != 1 || cards[0].DailyLimitMinor != 12000 || capOf() != 12000 {
		t.Fatalf("after the issuer refused: %+v %v, issuer cap %d; want the old limit 12000 kept", cards, err, capOf())
	}
}
