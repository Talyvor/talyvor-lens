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
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B28.346 — pay a saved payee by name: POST /v1/money/payments with payee_name and no payee_id pays the one saved payee
// of that name, in any case and spacing, and writes the same postings paying it by id does. A name two saved payees
// share, or none has, pays nothing. The challenge a person signs, asked by name, is the one asked by id. Read off postings.
func TestB28346_PaySavedPayeeByNameWritesTheSamePostingsAsByID(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b28346-pay-by-name"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	confirmedAccounts(t, pool, ws)
	acct, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	ada, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
	if err != nil {
		t.Fatal(err)
	}
	for _, number := range []string{"11223344", "22334455"} {
		if _, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Sam Smith", Country: "GB", SortCode: "200000", AccountNumber: number}); err != nil {
			t.Fatal(err)
		}
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws).Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -50000}, {AccountID: acct.ID, AmountMinor: 50000}}}); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMoneyPaymentRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	payment := func(payee, key string) string {
		return `{` + payee + `,"account_id":"` + acct.ID + `","currency":"GBP","amount_minor":2000,"reference":"Invoice","idempotency_key":"` + key + `"}`
	}
	// postings is every posting of the payment under key, in order: "kind account amount", the account by its purpose.
	postings := func(key string) string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT e.kind, a.purpose, p.amount_minor, e.counterparty FROM money_entries e
			JOIN money_postings p ON p.entry_id = e.id JOIN money_accounts a ON a.id = p.account_id
			WHERE e.workspace_id = $1 AND e.idempotency_key LIKE 'payment_out%:' || $2 ORDER BY e.created_at, p.line`, ws, key)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var kind, purpose, counterparty string
			var amount int64
			if err := rows.Scan(&kind, &purpose, &amount, &counterparty); err != nil {
				t.Fatal(err)
			}
			out = append(out, kind+" "+purpose+" "+jsonOf(amount)+" to "+counterparty)
		}
		return strings.Join(out, "; ")
	}

	code, byID := call("/v1/money/payments", payment(`"payee_id":"`+ada.ID+`"`, "by-id"))
	if code != http.StatusCreated || byID["status"] != "completed" {
		t.Fatalf("pay Ada by id = %d %v, want 201 completed", code, byID)
	}
	code, byName := call("/v1/money/payments", payment(`"payee_name":"  ada   LOVELACE "`, "by-name"))
	if code != http.StatusCreated || byName["status"] != "completed" || byName["payee_id"] != ada.ID || byName["payee_name"] != "Ada Lovelace" {
		t.Fatalf("pay Ada by name = %d %v, want 201 completed to %s", code, byName, ada.ID)
	}
	if postings("by-id") == "" || postings("by-name") != postings("by-id") {
		t.Errorf("paid by name posted %q, by id %q: want the same postings", postings("by-name"), postings("by-id"))
	}
	if b, err := store.Balance(ctx, ws, acct.ID); err != nil || b.TestMinor != 46000 {
		t.Errorf("the account holds %+v (%v), want £460.00 after the two £20.00 payments", b, err)
	}

	for name, want := range map[string]int{"Sam Smith": http.StatusConflict, "Ada": http.StatusNotFound, "Nobody": http.StatusNotFound} {
		if code, out := call("/v1/money/payments", payment(`"payee_name":"`+name+`"`, "name-"+name)); code != want {
			t.Errorf("pay %q by name = %d %v, want %d", name, code, out, want)
		}
		if got := postings("name-" + name); got != "" {
			t.Errorf("paying %q by name posted %q, want nothing", name, got)
		}
	}
	var payments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_payments WHERE workspace_id = $1`, ws).Scan(&payments); err != nil || payments != 2 {
		t.Errorf("%d payment rows (%v), want only the two paid to Ada", payments, err)
	}

	// The challenge asked by name is for the payee the name resolves to, so a signature over it pays exactly that payee.
	_, viaID := call("/v1/money/payments/challenge", payment(`"payee_id":"`+ada.ID+`"`, "signed"))
	_, viaName := call("/v1/money/payments/challenge", payment(`"payee_name":"Ada Lovelace"`, "signed"))
	var idPurpose, namePurpose string
	for c, purpose := range map[any]*string{viaID["challenge"]: &idPurpose, viaName["challenge"]: &namePurpose} {
		if err := pool.QueryRow(ctx, `SELECT purpose FROM webauthn_challenges WHERE challenge = $1`, c).Scan(purpose); err != nil {
			t.Fatalf("challenge %v: %v", c, err)
		}
	}
	if idPurpose == "" || namePurpose != idPurpose {
		t.Errorf("challenge by name is for %q, by id for %q: want the same payment", namePurpose, idPurpose)
	}
}
