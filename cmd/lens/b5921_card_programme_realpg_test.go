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
	"github.com/talyvor/lens/internal/money/issuerfake"
	"github.com/talyvor/lens/internal/partners"
)

// B59.21 — approval before the card, against the fake issuer on real Postgres. The fake issuer's programme is
// preapproved by default: a payment above the agent's rules creates no card and raises no limit until the owner
// approves it; then the one-time card's limit at the issuer is the approved amount, and a standing card's limit is
// raised by it and lowered back once the approval expires. Set to realtime, the programme is today's path: nothing is
// approved first, and the issuer asks Lens at each payment.
func TestB5921_PreapprovedProgrammeMakesNoCardUntilTheOwnerApproves(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5921"
	var issuer *issuerfake.Issuer
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b5921"), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
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
	b.balance(t, 50000)
	agent := b.acct.AgentID
	r := chi.NewRouter()
	mountCardSpendRoutes(r, b.store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	ask := func(body string) (int, economy.CardSpend, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/money/card-spends", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out economy.CardSpend
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		var held struct {
			ApprovalID string `json:"approval_id"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &held)
		return w.Code, out, held.ApprovalID
	}
	approve := func(id string) {
		t.Helper()
		if _, err := b.store.DecideAgentApproval(ctx, ws, id, true); err != nil {
			t.Fatalf("the owner approves %s: %v", id, err)
		}
	}

	// Every payment is above the agent's rules: each waits for the owner.
	if _, err := b.store.SetAgentRules(ctx, ws, agent, economy.AgentRules{ApprovalAboveULXC: 1}); err != nil {
		t.Fatal(err)
	}

	// A one-time card for one payment.
	oneTime := `{"agent_id":"` + agent + `","merchant":"Figma","amount_minor":4500,"currency":"GBP","idempotency_key":"figma"}`
	var approvalID string
	for range 2 { // asked twice before the owner answers: still nothing
		code, _, id := ask(oneTime)
		if code != http.StatusAccepted || id == "" {
			t.Fatalf("a payment above the agent's rules = %d (approval %q), want 202 waiting for the owner", code, id)
		}
		approvalID = id
	}
	if n := issuer.Issued(); n != 0 {
		t.Fatalf("before the owner approved, the issuer issued %d cards, want none", n)
	}
	if got := b4610Postings(t, pool, ws); got != "" {
		t.Fatalf("before the owner approved, the ledger has %q, want nothing", got)
	}
	approve(approvalID)
	code, spend, _ := ask(oneTime)
	if code != http.StatusCreated || spend.Status != economy.CardSpendActive || spend.Card == nil {
		t.Fatalf("asked again once approved = %d %+v, want 201 and an active one-time card", code, spend)
	}
	ic, ok := issuer.Card(issuerRef(t, pool, spend.Card.ID))
	if !ok || ic.Cap.Minor != 4500 || ic.Cap.Asset != "GBP" || ic.Merchant != "Figma" || ic.State != money.CardActive {
		t.Fatalf("the one-time card at the issuer = %+v, want active, locked to Figma, its limit the approved £45.00", ic)
	}
	if got, want := b4610Postings(t, pool, ws), "card_hold agent -4500; card_hold hold 4500"; got != want {
		t.Fatalf("the approved card posted %q, want %q", got, want)
	}
	if code, again, _ := ask(oneTime); code != http.StatusCreated || again.ID != spend.ID || issuer.Issued() != 1 {
		t.Fatalf("the same request again = %d %+v with %d cards issued, want the same card and no second one", code, again, issuer.Issued())
	}

	// A standing card for a subscription: the owner's approved budget raises its limit, for a time.
	standing, err := b.store.IssueReusableCard(ctx, ws, economy.ReusableCard{AgentID: agent, IdempotencyKey: "subs", Currency: "GBP",
		PerPaymentLimitMinor: 1000, DailyLimitMinor: 1000})
	if err != nil || standing.Status != economy.ReusableCardActive {
		t.Fatalf("issue the standing card = %+v %v", standing, err)
	}
	var standingRef string
	if err := pool.QueryRow(ctx, `SELECT partner_ref FROM money_reusable_cards WHERE id = $1`, standing.ID).Scan(&standingRef); err != nil {
		t.Fatal(err)
	}
	limit := func(want int64) {
		t.Helper()
		ic, _ := issuer.Card(standingRef)
		card, err := b.store.ReusableCards(ctx, ws, agent)
		if err != nil || len(card) != 1 || ic.Cap.Minor != want || card[0].DailyLimitMinor != want {
			t.Fatalf("the standing card's limit is %d at the issuer and %+v in Lens (%v), want %d", ic.Cap.Minor, card, err, want)
		}
	}
	subscription := `{"agent_id":"` + agent + `","merchant":"Notion","amount_minor":3000,"currency":"GBP","standing_card_id":"` + standing.ID +
		`","idempotency_key":"notion-nov"}`
	code, _, approvalID = ask(subscription)
	if code != http.StatusAccepted || approvalID == "" {
		t.Fatalf("a subscription above the agent's rules = %d, want 202 waiting for the owner", code)
	}
	limit(1000)
	approve(approvalID)
	if code, spend, _ = ask(subscription); code != http.StatusCreated || spend.Status != economy.CardSpendActive || spend.ExpiresAt == nil {
		t.Fatalf("the subscription asked again once approved = %d %+v, want 201, active until it expires", code, spend)
	}
	limit(4000)
	if n, err := b.store.RunCardSpendExpiries(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("expiries before the approval's time = %d %v, want none", n, err)
	}
	limit(4000)
	if n, err := b.store.RunCardSpendExpiries(ctx, spend.ExpiresAt.Add(time.Second)); err != nil || n != 1 {
		t.Fatalf("expiries after the approval's time = %d %v, want 1", n, err)
	}
	limit(1000)
	if code, spend, _ = ask(subscription); spend.Status != economy.CardSpendExpired {
		t.Fatalf("the expired subscription asked again = %d %+v, want it expired, the limit not raised again", code, spend)
	}
	limit(1000)

	// Realtime: today's path. Nothing is approved first, and nothing is issued.
	b.store.SetCardProgrammeModes(map[string]string{issuerfake.Name: economy.CardProgrammeRealtime})
	if code, _, _ := ask(`{"agent_id":"` + agent + `","merchant":"Canva","amount_minor":900,"currency":"GBP","idempotency_key":"canva"}`); code != http.StatusConflict ||
		issuer.Issued() != 2 {
		t.Fatalf("asking first on a realtime programme = %d with %d cards issued, want 409 and no new card", code, issuer.Issued())
	}
}
