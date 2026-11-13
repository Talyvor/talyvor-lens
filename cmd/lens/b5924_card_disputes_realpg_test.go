package main

import (
	"context"
	"encoding/csv"
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

// B59.24 — card disputes, refunds and exports, against the Sandbox's disputes on real Postgres. A merchant's refund
// credits the agent's account once, however often the issuer sends it, and never more than the payment; the owner
// opens a dispute on a card payment with a reason and the issuer takes it; a won dispute credits once and a lost one
// credits nothing, each as its own ledger lines; and the CSV export holds the caller's workspace's card payments,
// disputes and refunds only.
func TestB5924_RefundsAndWonDisputesCreditOnceAndTheExportIsTheWorkspacesOwn(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5924"
	disputes := &money.SandboxDisputes{Partner: money.SandboxName}
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			sandbox := money.NewSandbox(reg, "whsec_b5924")
			sandbox.Disputes = disputes
			layer, err := money.NewRegistry(money.Config{Sandbox: sandbox, Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
				Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool)})
			if err != nil {
				t.Fatal(err)
			}
			store.SetSandboxDisputes(disputes)
			return layer
		})
	agent := b.acct.AgentID
	r := chi.NewRouter()
	mountCardSpendRoutes(r, b.store)
	mountCardDisputeRoutes(r, b.store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string, out any) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil {
			_ = json.Unmarshal(w.Body.Bytes(), out)
		}
		return w.Code, w.Body.String()
	}
	// pay is the agent paying merchant amount by one-time card; its id is the issuer's.
	pay := func(merchant string, amount int64) string {
		t.Helper()
		var spend economy.CardSpend
		if code, body := call(http.MethodPost, "/v1/money/card-spends", jsonOf(map[string]any{"agent_id": agent, "merchant": merchant,
			"amount_minor": amount, "currency": "GBP", "idempotency_key": merchant}), &spend); code != http.StatusCreated || spend.Card == nil {
			t.Fatalf("the agent's card for %s = %d %s", merchant, code, body)
		}
		id := "iauth_" + strings.ToLower(merchant)
		if ok, why, err := b.store.AuthorizeCardCharge(ctx, money.SandboxName, money.Authorization{ID: id, Card: issuerRef(t, pool, spend.Card.ID),
			Merchant: merchant, Amount: money.Amount{Minor: amount, Asset: "GBP"}}); err != nil || !ok {
			t.Fatalf("the agent pays %s = %v %q %v", merchant, ok, why, err)
		}
		return id
	}
	credits := func() string { // the refund and dispute lines on the ledger
		t.Helper()
		var out []string
		for _, l := range strings.Split(b4610Postings(t, pool, ws), "; ") {
			if strings.HasPrefix(l, "card_refund ") || strings.HasPrefix(l, "card_dispute") {
				out = append(out, l)
			}
		}
		return strings.Join(out, "; ")
	}
	figma, notion, canva := pay("Figma", 4500), pay("Notion", 3000), pay("Canva", 2000)
	b.balance(t, 50000-4500-3000-2000)

	// A merchant's refund credits once, however often it arrives, and never more than what was paid.
	for range 2 {
		var re economy.CardPaymentRefund
		if code, body := call(http.MethodPost, "/v1/money/card-refunds", `{"payment_id":"`+figma+`","amount_minor":1000,"idempotency_key":"part"}`,
			&re); code != http.StatusCreated || re.CreditedMinor != 1000 {
			t.Fatalf("the Sandbox's merchant refunds £10.00 of Figma = %d %s, want 201 and £10.00 credited", code, body)
		}
	}
	b.balance(t, 40500+1000)
	if got, want := credits(), "card_refund partner -1000; card_refund agent 1000"; got != want {
		t.Fatalf("after a refund sent twice the ledger has %q, want %q", got, want)
	}
	if re, err := b.store.ReceiveCardRefund(ctx, money.SandboxName, money.CardRefund{ID: "re_all", Payment: figma,
		Amount: money.Amount{Minor: 9999, Asset: "GBP"}}); err != nil || re.CreditedMinor != 3500 {
		t.Fatalf("a refund of more than is left = %+v %v, want only the £35.00 left credited", re, err)
	}
	b.balance(t, 41500+3500)

	// The owner disputes Notion with a reason; the issuer has it. Won, it credits once.
	var d economy.CardDispute
	if code, body := call(http.MethodPost, "/v1/money/card-disputes", `{"payment_id":"`+notion+`","reason":"Charged after I cancelled"}`, &d); code !=
		http.StatusCreated || d.Status != economy.CardDisputeWithIssuer || d.Reason != "Charged after I cancelled" || d.AmountMinor != 3000 {
		t.Fatalf("open a dispute on Notion = %d %s, want 201, with the issuer, for £30.00 with its reason", code, body)
	}
	if code, _ := call(http.MethodPost, "/v1/money/card-disputes", `{"payment_id":"`+notion+`","reason":"Again"}`, nil); code != http.StatusCreated {
		t.Fatalf("the same payment disputed again = %d, want the same dispute", code)
	}
	for range 2 {
		if code, body := call(http.MethodPost, "/v1/money/card-disputes/"+d.ID+"/sandbox-outcome", `{"won":true}`, &d); code != http.StatusOK ||
			d.Status != economy.CardDisputeWon || d.CreditedMinor != 3000 {
			t.Fatalf("the issuer decides the Notion dispute won = %d %s, want won and £30.00 credited", code, body)
		}
	}
	b.balance(t, 45000+3000)

	// Canva's dispute is lost: nothing is credited.
	if code, body := call(http.MethodPost, "/v1/money/card-disputes", `{"payment_id":"`+canva+`","reason":"Not as described"}`, &d); code != http.StatusCreated {
		t.Fatalf("open a dispute on Canva = %d %s", code, body)
	}
	if code, body := call(http.MethodPost, "/v1/money/card-disputes/"+d.ID+"/sandbox-outcome", `{"won":false}`, &d); code != http.StatusOK ||
		d.Status != economy.CardDisputeLost || d.CreditedMinor != 0 {
		t.Fatalf("the issuer decides the Canva dispute lost = %d %s, want lost and nothing credited", code, body)
	}
	if code, _ := call(http.MethodPost, "/v1/money/card-disputes/"+d.ID+"/sandbox-outcome", `{"won":true}`, nil); code != http.StatusConflict {
		t.Fatalf("a lost dispute decided again as won = %d, want 409", code)
	}
	b.balance(t, 48000)
	if got, want := credits(), "card_refund partner -1000; card_refund agent 1000; card_refund partner -3500; card_refund agent 3500; "+
		"card_dispute_won partner -3000; card_dispute_won agent 3000"; got != want {
		t.Fatalf("the ledger's credits are %q, want %q", got, want)
	}

	// The export: this workspace's card payments, disputes and refunds, and none of another's.
	if _, err := pool.Exec(ctx, `INSERT INTO money_card_authorizations (partner, id, card_id, workspace_id, merchant, amount_minor, currency, approved)
		VALUES ($1, 'iauth_elsewhere', 'mcard_elsewhere', 'ws-b5924-other', 'Elsewhere Ltd', 777, 'GBP', true)`, money.SandboxName); err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format(time.DateOnly)
	code, body := call(http.MethodGet, "/v1/money/card-exports?from="+today+"&to="+today, "", nil)
	if code != http.StatusOK {
		t.Fatalf("export = %d %s", code, body)
	}
	rows, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("the export is not CSV: %v\n%s", err, body)
	}
	kinds := map[string]int{}
	for _, row := range rows[1:] {
		kinds[row[0]]++
		if strings.Contains(strings.Join(row, ","), "elsewhere") || strings.Contains(strings.Join(row, ","), "Elsewhere") {
			t.Fatalf("the export holds another workspace's payment: %v", row)
		}
	}
	if kinds["payment"] != 3 || kinds["dispute"] != 2 || kinds["refund"] != 2 {
		t.Fatalf("the export holds %v, want 3 payments, 2 disputes and 2 refunds\n%s", kinds, body)
	}
	if code, _ := call(http.MethodGet, "/v1/money/card-exports?from=2026-01-01&to=2025-01-01", "", nil); code != http.StatusBadRequest {
		t.Fatalf("an export ending before it starts = %d, want 400", code)
	}
}
