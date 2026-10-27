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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/issuerfake"
	"github.com/talyvor/lens/internal/partners"
)

// b4610Postings is every card posting in ws, in order: "kind purpose amount".
func b4610Postings(t *testing.T, pool *pgxpool.Pool, ws string) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT e.kind, a.purpose, p.amount_minor FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
		JOIN money_accounts a ON a.id = p.account_id WHERE e.workspace_id = $1 AND e.kind LIKE 'card_%' ORDER BY e.created_at, p.line`, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, purpose string
		var amount int64
		_ = rows.Scan(&kind, &purpose, &amount)
		out = append(out, kind+" "+purpose+" "+jsonOf(amount))
	}
	return strings.Join(out, "; ")
}

// B46.10 — against the fake issuer: one approval issues a batch of three cards for "book a trip", each locked to its
// merchant and capped; a charge at the locked merchant up to the cap is approved once and closes the card; a second
// charge, another merchant, more than the cap or a paused agent is declined; a card unused for 24 hours is closed and its cap given back.
func TestB4610_OneApprovalIssuesThreeLockedCardsAndEachPaysItsMerchantOnceUpToItsCap(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b4610"
	var issuer *issuerfake.Issuer
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b4610"), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
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

	// The agent's own approval amount is below every card's cap: the owner's one approval of the batch is what issues them.
	if _, err := b.store.SetAgentRules(ctx, ws, b.acct.AgentID, economy.AgentRules{ApprovalAboveULXC: 1}); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountMoneyCardRoutes(r, b.store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, economy.CardBatch) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out economy.CardBatch
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	code, batch := call(http.MethodPost, "/v1/money/cards", `{"agent_id":"`+b.acct.AgentID+`","purpose":"Book a trip to Lisbon","currency":"GBP",
		"idempotency_key":"trip","cards":[{"merchant":"British Airways","cap_minor":20000},{"merchant":"Marriott Lisbon","cap_minor":15000},
		{"merchant":"Hertz","cap_minor":5000}]}`)
	if code != http.StatusCreated || batch.Status != economy.CardBatchAwaitingApproval || len(batch.Cards) != 3 || batch.TotalMinor != 40000 {
		t.Fatalf("ask for three cards = %d %+v, want a batch of three capped at £400.00 awaiting approval", code, batch)
	}
	if got := b4610Postings(t, pool, ws); got != "" {
		t.Fatalf("a batch awaiting approval posted %q, want nothing", got)
	}
	b.balance(t, 50000)

	for range 2 { // approved twice: issued once
		if code, batch = call(http.MethodPost, "/v1/money/cards/"+batch.ID+"/approve", ""); code != http.StatusOK || batch.Status != economy.CardBatchIssued {
			t.Fatalf("approve = %d %+v, want the batch issued", code, batch)
		}
	}
	for _, c := range batch.Cards {
		ic, ok := issuer.Card(issuerRef(t, pool, c.ID))
		if c.Status != economy.MoneyCardActive || c.Partner != issuerfake.Name || !ok || ic.Merchant != c.Merchant || ic.Cap.Minor != c.CapMinor ||
			ic.State != money.CardActive || c.ExpiresAt == nil || time.Until(*c.ExpiresAt) > 24*time.Hour {
			t.Fatalf("card %+v at the issuer %+v, want active, locked to its merchant, capped and closing within 24 hours", c, ic)
		}
	}
	if _, ok := issuer.Card("ic_0004"); ok {
		t.Fatal("approving twice issued a fourth card")
	}
	if got, want := b4610Postings(t, pool, ws), "card_hold agent -40000; card_hold hold 40000"; got != want {
		t.Fatalf("one approval posted %q, want %q", got, want)
	}
	b.balance(t, 10000)

	air, hotel, car := issuerRef(t, pool, batch.Cards[0].ID), issuerRef(t, pool, batch.Cards[1].ID), issuerRef(t, pool, batch.Cards[2].ID)
	charge := func(ref, merchant string, minor int64, wantOK bool, wantReason string) {
		t.Helper()
		ok, reason, err := issuer.Charge(ctx, ref, merchant, money.Amount{Minor: minor, Asset: "GBP"})
		if err != nil || ok != wantOK || !strings.Contains(reason, wantReason) {
			t.Fatalf("%s charges %d to %s = %v %q %v, want %v %q", merchant, minor, ref, ok, reason, err, wantOK, wantReason)
		}
	}
	charge(air, "British Airways", 18000, true, "")
	if ic, _ := issuer.Card(air); ic.State != money.CardClosed {
		t.Fatalf("after its one charge the airline's card is %q at the issuer, want closed", ic.State)
	}
	charge(air, "British Airways", 1000, false, "it pays once")
	charge(hotel, "Hilton", 5000, false, "pays only Marriott Lisbon")
	charge(hotel, "Marriott Lisbon", 15001, false, "more than the card's cap")
	if err := b.store.PauseAgent(ctx, ws, b.acct.AgentID, "the owner paused it"); err != nil {
		t.Fatal(err)
	}
	charge(hotel, "Marriott Lisbon", 15000, false, "paused")
	if err := b.store.ResumeAgent(ctx, ws, b.acct.AgentID); err != nil {
		t.Fatal(err)
	}
	charge(hotel, "marriott  lisbon", 15000, true, "")
	// The issuer asking again about the first charge is answered as before, and moves nothing again.
	if ok, _, err := b.store.AuthorizeCardCharge(ctx, issuerfake.Name, money.Authorization{ID: "iauth_1", Card: air, Merchant: "British Airways",
		Amount: money.Amount{Minor: 18000, Asset: "GBP"}}); !ok || err != nil {
		t.Fatalf("the first charge asked again = %v %v, want approved as before", ok, err)
	}

	if n, err := b.store.RunCardExpiries(ctx, time.Now().Add(25*time.Hour)); err != nil || n != 1 {
		t.Fatalf("expiries a day later = %d %v, want the unused car-hire card closed", n, err)
	}
	if ic, _ := issuer.Card(car); ic.State != money.CardClosed {
		t.Fatalf("the expired card is %q at the issuer, want closed", ic.State)
	}
	charge(car, "Hertz", 1000, false, "expired")

	if got, want := b4610Postings(t, pool, ws), "card_hold agent -40000; card_hold hold 40000; "+
		"card_charge hold -20000; card_charge partner 18000; card_charge agent 2000; "+
		"card_charge hold -15000; card_charge partner 15000; "+
		"card_release hold -5000; card_release agent 5000"; got != want {
		t.Fatalf("the cards posted %q, want %q", got, want)
	}
	b.balance(t, 17000)
	var approved, declined int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE approved), count(*) FILTER (WHERE NOT approved) FROM money_card_authorizations
		WHERE workspace_id = $1`, ws).Scan(&approved, &declined); err != nil || approved != 2 || declined != 5 {
		t.Fatalf("authorisations recorded: %d approved, %d declined (%v), want 2 and 5", approved, declined, err)
	}
}

// issuerRef is the issuer's reference for card id.
func issuerRef(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var ref string
	if err := pool.QueryRow(context.Background(), `SELECT partner_ref FROM money_cards WHERE id = $1`, id).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	return ref
}
