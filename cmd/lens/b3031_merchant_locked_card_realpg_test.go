package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
)

// B30.31 — a card locked to one merchant: locked by the owner to the merchant of its first purchase, a second
// merchant's authorisation is declined naming the lock, on the card's statement, and holds nothing on the account;
// locked by the owner to a merchant id they set, only that merchant is approved.
func TestB3031_ALockedCardDeclinesASecondMerchantNamingTheLock(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b3031"
	b := newB468Payer(t, pool, ws, "USD", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(*economy.DualTokenStore, *partners.Registry) *money.Registry { return nil })
	agentID := b.acct.AgentID
	if _, err := b.store.SaveAgentCard(ctx, ws, economy.AgentCard{ID: "ic_b3031", AgentID: agentID, StripeCardholderID: "ich_b3031", Last4: "4242",
		ExpMonth: 12, ExpYear: 2030, Currency: "usd"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.SetAgentCardDrawsOn(ctx, ws, agentID, economy.CardDrawsOnAccount); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountAgentCardRoutes(r, b.store, nil)
	lock := func(body string) economy.AgentCard {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+ws+"/agents/"+agentID+"/card/merchant-lock", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner",
			Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var card economy.AgentCard
		_ = json.Unmarshal(w.Body.Bytes(), &card)
		if w.Code != http.StatusOK {
			t.Fatalf("lock the card with %s = %d %s, want 200", body, w.Code, w.Body)
		}
		return card
	}
	buy := func(evt, merchantID, merchantName string) economy.CardDecision {
		t.Helper()
		d, err := b.store.AuthorizeAgentCard(ctx, economy.CardAuthorization{EventID: evt, AuthorizationID: "iauth_" + evt, CardID: "ic_b3031",
			AmountMinor: 1000, Currency: "usd", MerchantName: merchantName, MerchantID: merchantID, At: time.Now(), USDMicros: 10_000_000})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	if card := lock(`{"lock":"first"}`); card.MerchantLock != economy.CardMerchantLockFirst || card.LockedMerchantID != "" {
		t.Fatalf("locked to its first merchant = %+v, want lock first with no merchant yet", card)
	}
	if d := buy("evt_b3031_nobody", "", "Unknown"); d.Approved || !strings.Contains(d.Reason, "locked to one merchant") {
		t.Fatalf("a purchase with no merchant id on a card locking to its first merchant = %+v, want declined", d)
	}
	if d := buy("evt_b3031_paper", "m-paper", "Paper Co"); !d.Approved {
		t.Fatalf("the first purchase, at Paper Co = %+v, want approved", d)
	}
	if card, err := b.store.GetAgentCard(ctx, ws, agentID); err != nil || card.LockedMerchantID != "m-paper" {
		t.Fatalf("after its first purchase the card = %+v %v, want locked to m-paper", card, err)
	}
	d := buy("evt_b3031_ink", "m-ink", "Ink Ltd")
	if d.Approved || !strings.Contains(d.Reason, "locked to one merchant") || !strings.Contains(d.Reason, "m-paper") {
		t.Fatalf("a second merchant's purchase = %+v, want declined naming the lock to m-paper", d)
	}
	if got, want := b4610Postings(t, pool, ws), "card_hold agent -1000; card_hold hold 1000"; got != want {
		t.Fatalf("the declined purchase posted: %q, want only the first purchase's hold %q", got, want)
	}
	auths, err := b.store.ListAgentCardAuthorizations(ctx, ws, agentID, 10)
	if err != nil || len(auths) != 3 || auths[0].MerchantName != "Ink Ltd" || auths[0].Approved || auths[0].Reason != d.Reason {
		t.Fatalf("the card's statement = %+v %v, want Ink Ltd's purchase declined with %q", auths, err, d.Reason)
	}
	if d := buy("evt_b3031_paper2", "m-paper", "Paper Co"); !d.Approved {
		t.Fatalf("a second purchase at Paper Co = %+v, want approved", d)
	}

	// The owner sets the merchant themselves: only that merchant is approved now.
	if card := lock(`{"lock":"merchant","merchant_id":"m-ink"}`); card.LockedMerchantID != "m-ink" {
		t.Fatalf("locked to m-ink = %+v", card)
	}
	if d := buy("evt_b3031_paper3", "m-paper", "Paper Co"); d.Approved || !strings.Contains(d.Reason, "m-ink") {
		t.Fatalf("Paper Co on a card locked to m-ink = %+v, want declined naming m-ink", d)
	}
	if d := buy("evt_b3031_ink2", "m-ink", "Ink Ltd"); !d.Approved {
		t.Fatalf("Ink Ltd on a card locked to m-ink = %+v, want approved", d)
	}
	if card := lock(`{"lock":"off"}`); card.MerchantLock != economy.CardMerchantLockOff || card.LockedMerchantID != "" {
		t.Fatalf("unlocked = %+v", card)
	}
	if d := buy("evt_b3031_paper4", "m-paper", "Paper Co"); !d.Approved {
		t.Fatalf("Paper Co on an unlocked card = %+v, want approved", d)
	}
}
