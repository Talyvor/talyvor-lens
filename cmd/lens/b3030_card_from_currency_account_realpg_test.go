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

// B30.30 — an agent card set to draw on the agent's USD account: a $25.00 authorisation holds $25.00 on the account,
// and its capture at $24.00 pays the merchant $24.00 and releases $1.00 back to it, on the money ledger; no credits move.
func TestB3030_ACardOnTheUSDAccountHolds25AndCaptureAt24Releases1(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b3030"
	b := newB468Payer(t, pool, ws, "USD", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(*economy.DualTokenStore, *partners.Registry) *money.Registry { return nil })
	agentID := b.acct.AgentID
	if _, err := b.store.SaveAgentCard(ctx, ws, economy.AgentCard{ID: "ic_b3030", AgentID: agentID, StripeCardholderID: "ich_b3030", Last4: "4242",
		ExpMonth: 12, ExpYear: 2030, Currency: "usd"}); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountAgentCardRoutes(r, b.store, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+ws+"/agents/"+agentID+"/card/draws-on", strings.NewReader(`{"draws_on":"account"}`))
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner",
		Scopes: []string{auth.ScopeKeys}}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var card economy.AgentCard
	_ = json.Unmarshal(w.Body.Bytes(), &card)
	if w.Code != http.StatusOK || card.DrawsOn != economy.CardDrawsOnAccount {
		t.Fatalf("set the card to draw on the account = %d %s, want 200 drawing on the account", w.Code, w.Body)
	}

	buy := func(evt string, cents int64) economy.CardDecision {
		t.Helper()
		d, err := b.store.AuthorizeAgentCard(ctx, economy.CardAuthorization{EventID: evt, AuthorizationID: "iauth_" + evt, CardID: "ic_b3030",
			AmountMinor: cents, Currency: "usd", MerchantName: "Paper Co", MerchantID: "m-paper", At: time.Now(), USDMicros: cents * 10_000})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	settle := func(evt, kind, status string, cents int64) economy.CardSettled {
		t.Helper()
		out, err := b.store.SettleAgentCard(ctx, economy.CardSettlement{EventID: evt, Kind: kind, AuthorizationID: "iauth_evt_b3030",
			TransactionID: "ipi_b3030", CardID: "ic_b3030", Status: status, AmountMinor: cents, Currency: "usd", At: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	credits := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1 AND type = 'agent_card'`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// More than the account holds: declined, nothing held.
	if d := buy("evt_b3030_big", 60000); d.Approved || !strings.Contains(d.Reason, "account holds") {
		t.Fatalf("a $600.00 purchase on a $500.00 account = %+v, want declined", d)
	}
	if got := b4610Postings(t, pool, ws); got != "" {
		t.Fatalf("a declined purchase posted %q", got)
	}

	if d := buy("evt_b3030", 2500); !d.Approved {
		t.Fatalf("a $25.00 purchase = %+v, want approved", d)
	}
	if got, want := b4610Postings(t, pool, ws), "card_hold agent -2500; card_hold hold 2500"; got != want {
		t.Fatalf("the authorisation posted %q, want %q", got, want)
	}
	b.balance(t, 47500)
	// The owner switches the card back to credits mid-purchase: an increment of it cannot be held on credits.
	if _, err := b.store.SetAgentCardDrawsOn(ctx, ws, agentID, economy.CardDrawsOnCredits); err != nil {
		t.Fatal(err)
	}
	if d, err := b.store.AuthorizeAgentCard(ctx, economy.CardAuthorization{EventID: "evt_b3030_incr", AuthorizationID: "iauth_evt_b3030",
		CardID: "ic_b3030", AmountMinor: 500, Currency: "usd", MerchantName: "Paper Co", MerchantID: "m-paper", At: time.Now(),
		USDMicros: 5_000_000}); err != nil || d.Approved || !strings.Contains(d.Reason, "drew on before") {
		t.Fatalf("an increment on credits of a purchase held on the account = %+v %v, want declined", d, err)
	}

	if out := settle("evt_b3030_capture", economy.CardCapture, "", 2400); !out.Recorded || out.AmountMinor != 100 {
		t.Fatalf("capture at $24.00 = %+v, want $1.00 released", out)
	}
	settle("evt_b3030_capture", economy.CardCapture, "", 2400) // Stripe sends it again: nothing more moves
	if out := settle("evt_b3030_closed", economy.CardRelease, "closed", 0); out.Recorded {
		t.Fatalf("the authorisation closing after its capture = %+v, want nothing left to release", out)
	}
	if got, want := b4610Postings(t, pool, ws), "card_hold agent -2500; card_hold hold 2500; "+
		"card_charge hold -2500; card_charge partner 2400; card_charge agent 100"; got != want {
		t.Fatalf("hold and capture posted %q, want %q", got, want)
	}
	b.balance(t, 47600)
	if n := credits(); n != 0 {
		t.Fatalf("a purchase on the account moved credits: %d agent_card rows", n)
	}
}
