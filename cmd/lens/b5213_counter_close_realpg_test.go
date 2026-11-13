package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
)

// b5213Env is a workspace with money, a payee Acme Ltd, a permission mandate letting Counter pay Acme up to
// 11,500.00 GBP a payment on its own, and the negotiation routes, called as its owner.
type b5213Env struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	store *economy.DualTokenStore
	ws    string
	acme  string // the payee's id
	sign  func(challenge string) *passkey.Assertion
	r     chi.Router
	// partner is the account every payment out moves its amount to.
	partner string
}

func b5213Setup(t *testing.T, ws string) *b5213Env {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const rpID, origin = "app.talyvor.com", "https://app.talyvor.com"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	e := &b5213Env{t: t, ctx: ctx, pool: pool, store: store, ws: ws}
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws).Scan(&e.partner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: e.partner, AmountMinor: -5000000}, {AccountID: company.ID, AmountMinor: 5000000}}}); err != nil {
		t.Fatal(err)
	}
	acme, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Acme Ltd", Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
	if err != nil {
		t.Fatal(err)
	}
	e.acme = acme.ID
	if _, err := pool.Exec(ctx, `UPDATE money_payees SET called_at = now() WHERE id = $1`, acme.ID); err != nil {
		t.Fatal(err)
	}
	e.sign = b5018Passkey(t, store, ws, rpID, origin)
	pc, pch, _, err := store.PreparePermissionMandate(ctx, ws, economy.PermissionMandateTerms{
		Principal: economy.MandatePrincipal{Type: "person", ID: "ada", Name: "Ada Lovelace"}, Clients: []string{economy.ClientCounter},
		Scope:  economy.MandateScope{Payees: []string{acme.ID}, Currencies: []string{"GBP"}},
		Limits: economy.MandateLimits{PerPayment: map[string]int64{"GBP": 1150000}}, NewPayee: economy.MandateNewPayee{Policy: economy.NewPayeeConfirmationOfPayee},
		ExpiresAt: time.Now().Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePermissionMandate(ctx, ws, "ada", pc, e.sign(pch)); err != nil {
		t.Fatal(err)
	}
	e.r = chi.NewRouter()
	mountNegotiationRoutes(e.r, store)
	mountNegotiationDealRoutes(e.r, store)
	return e
}

// call answers method path as ws's owner, decoding a 2xx body into out.
func (e *b5213Env) call(ws, method, path, body string, out any) (int, string) {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}
	req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	if out != nil && w.Code < 300 {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			e.t.Fatalf("%s: %v in %s", path, err, w.Body.String())
		}
	}
	return w.Code, w.Body.String()
}

// mandate keeps a signed negotiation mandate buying from acme.example: target 10,000.00, walk-away 12,000.00, must-have
// 99.9% uptime.
func (e *b5213Env) mandate(authority string, deadline time.Time) string {
	e.t.Helper()
	var prep struct{ Canonical, Challenge string }
	if code, msg := e.call(e.ws, http.MethodPost, "/v1/negotiations/prepare", `{"task":{"kind":"renewal","what":"Acme CRM, 40 seats"},
		"side":"buy","currency":"GBP","target_minor":1000000,"walk_away_minor":1200000,"must_haves":["99.9% uptime"],"deadline":"`+
		deadline.Format(time.RFC3339)+`","counterparties":["acme.example"],"channels":["agent"],"authority":"`+authority+`"}`, &prep); code != http.StatusOK {
		e.t.Fatalf("prepare = %d %s", code, msg)
	}
	b, _ := json.Marshal(map[string]any{"canonical": prep.Canonical, "assertion": e.sign(prep.Challenge)})
	var m economy.NegotiationMandate
	if code, msg := e.call(e.ws, http.MethodPost, "/v1/negotiations", string(b), &m); code != http.StatusCreated {
		e.t.Fatalf("keep = %d %s", code, msg)
	}
	return m.ID
}

// theirs records acme.example's offer at price, meeting the must-have when meets.
func (e *b5213Env) theirs(id string, price int64, meets bool) {
	e.t.Helper()
	met := `"99.9% uptime"`
	if !meets {
		met = ""
	}
	if code, msg := e.call(e.ws, http.MethodPost, "/v1/negotiations/"+id+"/terms", fmt.Sprintf(`{"counterparty":"acme.example","party":"theirs",
		"channel":"agent","terms":{"price_minor":%d,"term_months":12,"sla":"99.9%% uptime","must_haves_met":[%s]}}`, price, met), nil); code != http.StatusCreated {
		e.t.Fatalf("their offer = %d %s", code, msg)
	}
}

// closeDeal accepts acme.example's latest offer, paid to payee when it is not "".
func (e *b5213Env) closeDeal(id, payee string) (int, string, economy.NegotiationDeal) {
	e.t.Helper()
	var d economy.NegotiationDeal
	code, msg := e.call(e.ws, http.MethodPost, "/v1/negotiations/"+id+"/close", `{"counterparty":"acme.example","payee_id":"`+payee+`"}`, &d)
	return code, msg, d
}

// paymentsOut reads the ledger: each payment out is one entry moving its amount to the partner.
func (e *b5213Env) paymentsOut() (n int, minor int64) {
	e.t.Helper()
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*), COALESCE(sum(p.amount_minor), 0)::bigint FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
		WHERE e.workspace_id = $1 AND e.kind = 'payment_out' AND p.account_id = $2`, e.ws, e.partner).Scan(&n, &minor); err != nil {
		e.t.Fatal(err)
	}
	return n, minor
}

// approval prepares the statement approving mandate id's waiting deal.
func (e *b5213Env) approval(id string) (statement, challenge string) {
	e.t.Helper()
	var prep struct {
		Statement string `json:"statement"`
		Challenge string `json:"challenge"`
	}
	if code, msg := e.call(e.ws, http.MethodPost, "/v1/negotiations/"+id+"/deal/approval", "", &prep); code != http.StatusOK {
		e.t.Fatalf("the approval statement = %d %s", code, msg)
	}
	return prep.Statement, prep.Challenge
}

func (e *b5213Env) approve(id, statement string, a *passkey.Assertion) (int, string) {
	bb, _ := json.Marshal(map[string]any{"statement": statement, "assertion": a})
	return e.call(e.ws, http.MethodPost, "/v1/negotiations/"+id+"/deal/approve", string(bb), nil)
}

// B52.13 — through the routes: a deal beyond the mandate — an authority short of accepting, a payee the owner never
// signed for with this counterparty, or a payment above the agent's limits — waits for the owner's signature on a
// statement of the counterparty's exact terms against the mandate and the payee's name, and pays nothing until signed;
// an altered statement is refused; signing it twice pays once. Inside the mandate's authority, terms that meet every
// must-have, sit inside the walk-away and pay a payee the owner signed for, inside the agent's limits, are accepted at
// once and paid through the payment request path.
func TestB5213_CounterClosesInsideTheMandateAndAsksTheOwnerForTheRest(t *testing.T) {
	e := b5213Setup(t, "ws-b5213")
	deadline := time.Now().Add(48 * time.Hour)

	// Beyond the mandate's authority, to a payee never signed for: it waits for the owner and pays nothing.
	b := e.mandate(economy.AuthorityNegotiate, deadline)
	e.theirs(b, 1050000, true)
	code, msg, waiting := e.closeDeal(b, e.acme)
	if code != http.StatusAccepted || waiting.Status != "awaiting_approval" || len(waiting.Outside) != 2 || waiting.RequestID != "" ||
		!strings.Contains(waiting.Outside[1], "has not paid this payee for acme.example") {
		t.Fatalf("an out-of-mandate close = %d %s", code, msg)
	}
	if n, _ := e.paymentsOut(); n != 0 {
		t.Fatalf("a deal waiting for the owner paid: %d payments out", n)
	}

	// The owner reads the counterparty's exact terms against the mandate, and who is paid by name, and signs.
	statement, challenge := e.approval(b)
	for _, want := range []string{`"price_minor":1050000`, `"walk_away_minor":1200000`, `"authority":"negotiate"`, `"payee_name":"Acme Ltd"`} {
		if !strings.Contains(statement, want) {
			t.Fatalf("the approval statement lacks %s: %s", want, statement)
		}
	}
	altered := strings.Replace(statement, `"price_minor":1050000`, `"price_minor":900000`, 1)
	if code, msg := e.approve(b, altered, e.sign(economy.MandateChallenge(altered))); code != http.StatusBadRequest {
		t.Fatalf("an altered statement = %d %s", code, msg)
	}
	if code, msg := e.approve(b, statement, e.sign(economy.MandateChallenge("another statement"))); code != http.StatusBadRequest {
		t.Fatalf("a signature over another statement = %d %s", code, msg)
	}
	if n, _ := e.paymentsOut(); n != 0 {
		t.Fatalf("an altered or unsigned statement paid: %d payments out", n)
	}
	if code, msg := e.approve(b, statement, e.sign(challenge)); code != http.StatusOK || !strings.Contains(msg, `"status":"accepted"`) {
		t.Fatalf("the owner's signature = %d %s", code, msg)
	}
	if code, msg := e.approve(b, statement, e.sign(challenge)); code < 400 {
		t.Fatalf("signing again = %d %s", code, msg)
	}
	if n, minor := e.paymentsOut(); n != 1 || minor != 1050000 {
		t.Fatalf("after the owner signed the ledger has %d payments out of %d, want 1 of 1050000", n, minor)
	}

	// Inside the mandate, to the payee the owner signed for with acme.example: accepted at once, paid once, and closed.
	a := e.mandate(economy.AuthorityAccept, deadline)
	e.theirs(a, 1100000, true) // another amount: the same payee paid the same amount twice is flagged a duplicate (B41.103)
	code, msg, deal := e.closeDeal(a, e.acme)
	if code != http.StatusOK || deal.Status != "accepted" || deal.Terms.PriceMinor != 1100000 || len(deal.Outside) != 0 || deal.RequestID == "" {
		t.Fatalf("an in-mandate close = %d %s", code, msg)
	}
	if n, minor := e.paymentsOut(); n != 2 || minor != 2150000 {
		t.Fatalf("after the in-mandate close the ledger has %d payments out of %d, want 2 of 2150000", n, minor)
	}
	var closed economy.NegotiationMandate
	if code, msg := e.call(e.ws, http.MethodGet, "/v1/negotiations/"+a, "", &closed); code != http.StatusOK || closed.Status != "closed" ||
		closed.Closing == nil || closed.Closing.ID != deal.ID {
		t.Fatalf("the closed negotiation reads %d %s", code, msg)
	}

	// A payment above the agent's limits waits for the owner too, though the mandate lets Counter accept.
	c := e.mandate(economy.AuthorityAccept, deadline)
	e.theirs(c, 1180000, true)
	if code, msg, over := e.closeDeal(c, e.acme); code != http.StatusAccepted || over.Status != "awaiting_approval" ||
		!strings.Contains(strings.Join(over.Outside, " "), "limits.per_payment") {
		t.Fatalf("a close over the agent's limits = %d %s", code, msg)
	}
	if n, _ := e.paymentsOut(); n != 2 {
		t.Fatalf("a deal waiting for the owner paid: %d payments out", n)
	}
	b501ZeroSum(t, e.pool)
}

// B52.13 — the mandate's guards: a price at the walk-away and an unmet must-have go to the owner though the authority
// lets Counter accept; a declined deal, a revoked mandate, a mandate past its deadline and another workspace's mandate
// accept and pay nothing; no caller can take a deal's payment key; an accepted deal whose payment failed after it was
// kept is paid again, once.
func TestB5213_CounterKeepsToTheMandateAndPaysAnAcceptedDealOnce(t *testing.T) {
	e := b5213Setup(t, "ws-b5213-guards")
	deadline := time.Now().Add(48 * time.Hour)
	outside := func(id string, price int64, meets bool, want string) {
		t.Helper()
		e.theirs(id, price, meets)
		code, msg, d := e.closeDeal(id, "")
		if code != http.StatusAccepted || d.Status != "awaiting_approval" || !strings.Contains(strings.Join(d.Outside, " "), want) {
			t.Fatalf("a close that should be beyond the mandate (%s) = %d %s", want, code, msg)
		}
	}
	outside(e.mandate(economy.AuthorityAccept, deadline), 1200000, true, "at or past the walk-away")
	outside(e.mandate(economy.AuthorityAccept, deadline), 1100000, false, "does not meet the must-have: 99.9% uptime")

	// Declined: the statement prepared before the decline approves nothing.
	d := e.mandate(economy.AuthorityNegotiate, deadline)
	e.theirs(d, 1010000, true)
	if code, msg, _ := e.closeDeal(d, e.acme); code != http.StatusAccepted {
		t.Fatalf("close = %d %s", code, msg)
	}
	statement, challenge := e.approval(d)
	if code, msg := e.call(e.ws, http.MethodPost, "/v1/negotiations/"+d+"/deal/decline", "", nil); code != http.StatusOK {
		t.Fatalf("decline = %d %s", code, msg)
	}
	if code, msg := e.approve(d, statement, e.sign(challenge)); code < 400 {
		t.Fatalf("approving a declined deal = %d %s", code, msg)
	}

	// Revoked while the deal waits: the owner's signature over the statement approves nothing, and nothing closes.
	r := e.mandate(economy.AuthorityNegotiate, deadline)
	e.theirs(r, 1020000, true)
	if code, msg, _ := e.closeDeal(r, e.acme); code != http.StatusAccepted {
		t.Fatalf("close = %d %s", code, msg)
	}
	statement, challenge = e.approval(r)
	if code, msg := e.call(e.ws, http.MethodPost, "/v1/negotiations/"+r+"/revoke", "", nil); code != http.StatusOK {
		t.Fatalf("revoke = %d %s", code, msg)
	}
	if code, msg := e.approve(r, statement, e.sign(challenge)); code != http.StatusBadRequest {
		t.Fatalf("approving under a revoked mandate = %d %s", code, msg)
	}
	if code, msg, _ := e.closeDeal(r, ""); code != http.StatusBadRequest {
		t.Fatalf("closing a revoked mandate = %d %s", code, msg)
	}

	// Past its deadline: nothing closes.
	x := e.mandate(economy.AuthorityAccept, time.Now().Add(2*time.Second))
	e.theirs(x, 1030000, true)
	time.Sleep(3 * time.Second) // the deadline kept is at most 2s after the mandate
	if code, msg, _ := e.closeDeal(x, ""); code != http.StatusBadRequest {
		t.Fatalf("closing past the deadline = %d %s", code, msg)
	}

	// Another workspace's owner cannot close or approve this workspace's negotiation.
	o := e.mandate(economy.AuthorityAccept, deadline)
	e.theirs(o, 1040000, true)
	if code, msg := e.call("ws-b5213-other", http.MethodPost, "/v1/negotiations/"+o+"/close", `{"counterparty":"acme.example"}`, nil); code != http.StatusNotFound {
		t.Fatalf("another workspace's close = %d %s", code, msg)
	}
	if n, _ := e.paymentsOut(); n != 0 {
		t.Fatalf("a refused deal paid: %d payments out", n)
	}

	// Accepted inside the mandate and kept, then the payment failed before it was prepared — as a crash leaves it.
	code, msg, kept := e.closeDeal(o, "")
	if code != http.StatusOK || kept.Status != "accepted" {
		t.Fatalf("an in-mandate close = %d %s", code, msg)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE negotiation_deals SET payee_id = $2 WHERE id = $1`, kept.ID, e.acme); err != nil {
		t.Fatal(err)
	}
	// No caller takes the deal's payment key first: a payment request under it is Counter's alone.
	_, _, err := e.store.PreparePaymentRequest(e.ctx, e.ws, "ada", economy.ClientCounter+":"+kept.ID, economy.PaymentRequestIn{Client: economy.ClientChat,
		Items: []economy.PaymentRequestItemIn{{PayeeID: e.acme, Currency: "GBP", AmountMinor: 1, Reference: "taken"}}})
	if !errors.Is(err, economy.ErrPaymentRequest) {
		t.Fatalf("a caller took a Counter deal's payment key: %v", err)
	}
	for range 2 {
		var paid economy.NegotiationDeal
		if code, msg := e.call(e.ws, http.MethodPost, "/v1/negotiations/"+o+"/deal/pay", "", &paid); code != http.StatusOK || paid.RequestID == "" {
			t.Fatalf("paying the accepted deal again = %d %s", code, msg)
		}
	}
	if n, minor := e.paymentsOut(); n != 1 || minor != 1040000 {
		t.Fatalf("the accepted deal paid %d times, %d in all; want once, 1040000", n, minor)
	}
	b501ZeroSum(t, e.pool)
}
