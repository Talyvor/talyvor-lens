package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B30.19 — an agent's account grants another Talyvor company a mandate of £30.00 a pull and £50.00 a month. A pull
// within it posts one entry, out of the agent's account and into the payee's, and a retry posts nothing more; a pull
// over the monthly maximum is refused and posts nothing; after the owner revokes it a pull is refused and posts
// nothing. An outside business (B30.129) pulls with its mandate's key into the mandate's hold account, and the pull is
// paid out of it to the business's saved payee through the partner: a completed one leaves the hold account at zero,
// with the partner's reference on the entry; one that fails (an amount ending 13) goes back to the agent's account.
func TestMandateRoutes_APayeePullsWithinTheMandateAndNotAfterIt(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, payeeWS = "ws-b3019-payer", "ws-b3019-payee"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetScreener(screener)
	store.SetPayeeScreener(screener)
	agent, err := store.CreateAgent(ctx, ws, "Subscriber", "owner")
	if err != nil {
		t.Fatal(err)
	}
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	agentGBP, err := store.OpenCurrencyAccount(ctx, ws, agent.ID, "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	payeeGBP, err := store.OpenCurrencyAccount(ctx, payeeWS, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	details, err := store.CurrencyAccountDetails(ctx, ws, "", agentGBP.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReceivePayment(ctx, economy.InboundPayment{PartnerAccountRef: company.PartnerAccountRef, PartnerRef: "test_in_b3019",
		Payer: "Owner Ltd", Reference: details.PaymentReference, AmountMinor: 10000, Currency: "GBP"}); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMandateRoutes(r, store)
	// The authed group's middleware, for the test: the workspace named on the request is the key's.
	mountMandatePullRoute(r, store, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			id := &auth.AuthContext{WorkspaceID: req.Header.Get("X-Test-Workspace"), AuthMethod: auth.MethodJWT, UserID: "payee", Scopes: []string{auth.ScopeKeys}}
			h.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), id)))
		})
	})
	call := func(asWS, key, method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if asWS != "" {
			req.Header.Set("X-Test-Workspace", asWS)
			req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: asWS, AuthMethod: auth.MethodJWT,
				UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		}
		if key != "" {
			req.Header.Set("X-Talyvor-Mandate-Key", key)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	postings := func() []string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT p.account_id, p.amount_minor, p.funding FROM money_postings p JOIN money_entries e ON e.id = p.entry_id
			WHERE e.kind = 'mandate_pull' ORDER BY e.created_at, p.line`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var acct, funding string
			var amount int64
			if err := rows.Scan(&acct, &amount, &funding); err != nil {
				t.Fatal(err)
			}
			got = append(got, fmt.Sprintf("%s %d %s", acct, amount, funding))
		}
		return got
	}
	expires := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)

	code, m := call(ws, "", http.MethodPost, "/v1/money/mandates", `{"account_id":"`+agentGBP.ID+`","payee_workspace_id":"`+payeeWS+
		`","max_per_pull_minor":3000,"max_per_month_minor":5000,"expires_at":"`+expires+`"}`)
	if code != http.StatusCreated || m["status"] != "active" || m["currency"] != "GBP" || m["pull_key"] != nil {
		t.Fatalf("grant = %d %v, want 201, an active GBP mandate with no key", code, m)
	}
	pullPath := "/v1/money/mandates/" + m["id"].(string) + "/pull"

	// Within the mandate: £20.00 moves from the agent's account into the payee's, as one entry; a retry moves nothing more.
	for range 2 {
		if code, out := call(payeeWS, "", http.MethodPost, pullPath, `{"amount_minor":2000,"idempotency_key":"inv-1"}`); code != http.StatusCreated {
			t.Fatalf("a pull of £20.00 = %d %v, want 201", code, out)
		}
	}
	want := []string{agentGBP.ID + " -2000 test", payeeGBP.ID + " 2000 test"}
	if got := postings(); strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Fatalf("the pull and its retry posted %v; want only %v", got, want)
	}
	if b, _ := store.Balance(ctx, ws, agentGBP.ID); b.TestMinor != 8000 {
		t.Errorf("the agent's balance = %+v; want 8000 test pence", b)
	}
	if b, _ := store.Balance(ctx, payeeWS, payeeGBP.ID); b.TestMinor != 2000 {
		t.Errorf("the payee's balance = %+v; want 2000 test pence", b)
	}
	// Only the payee may pull; another company finds no mandate.
	if code, out := call("ws-b3019-stranger", "", http.MethodPost, pullPath, `{"amount_minor":100,"idempotency_key":"x"}`); code != http.StatusNotFound {
		t.Errorf("a stranger's pull = %d %v, want 404", code, out)
	}
	// Over a pull's maximum, and over the month's: £20.00 then £20.00 more is £40.00; £20.00 again would be £60.00.
	if code, out := call(payeeWS, "", http.MethodPost, pullPath, `{"amount_minor":3500,"idempotency_key":"inv-big"}`); code != http.StatusForbidden {
		t.Errorf("a pull of £35.00 = %d %v, want 403: over £30.00 a pull", code, out)
	}
	if code, out := call(payeeWS, "", http.MethodPost, pullPath, `{"amount_minor":2000,"idempotency_key":"inv-2"}`); code != http.StatusCreated {
		t.Fatalf("a second pull of £20.00 = %d %v, want 201", code, out)
	}
	code, out := call(payeeWS, "", http.MethodPost, pullPath, `{"amount_minor":2000,"idempotency_key":"inv-3"}`)
	if msg, _ := out["error"].(string); code != http.StatusForbidden || !strings.Contains(msg, "maximum per month of 5000") {
		t.Fatalf("a third pull of £20.00 = %d %v, want 403 naming the monthly maximum", code, out)
	}
	if got := postings(); len(got) != 4 {
		t.Fatalf("after the refusals the pulls posted %v; want the two pulls' four postings", got)
	}
	if code, out := call(payeeWS, "", http.MethodGet, "/v1/money/mandates", ""); code != http.StatusOK ||
		len(out["received"].([]any)) != 1 || out["received"].([]any)[0].(map[string]any)["pulled_this_month_minor"] != 4000.0 {
		t.Errorf("the payee's mandates = %d %v, want the one received, £40.00 pulled this month", code, out)
	}

	// Revoked by the owner, it pulls nothing.
	if code, out := call(ws, "", http.MethodPost, "/v1/money/mandates/"+m["id"].(string)+"/revoke", ""); code != http.StatusOK || out["status"] != "revoked" {
		t.Fatalf("revoke = %d %v, want 200 revoked", code, out)
	}
	if code, out := call(payeeWS, "", http.MethodPost, pullPath, `{"amount_minor":100,"idempotency_key":"inv-4"}`); code != http.StatusConflict {
		t.Fatalf("a pull after revocation = %d %v, want 409", code, out)
	}
	if got := postings(); len(got) != 4 {
		t.Fatalf("after revocation the pulls posted %v; want still the four postings", got)
	}

	// An outside business: its mandate names the saved payee it is paid out to, its key is answered once, and what it
	// pulls goes through the mandate's hold account to the partner as a payment out, leaving the hold account at zero.
	acme, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Acme Hosting Ltd", Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
	if err != nil {
		t.Fatal(err)
	}
	if code, out := call(ws, "", http.MethodPost, "/v1/money/mandates", `{"account_id":"`+agentGBP.ID+`","payee_business_id":"GB-COMPANY-01234567",`+
		`"max_per_pull_minor":1500,"max_per_month_minor":1500,"expires_at":"`+expires+`"}`); code != http.StatusBadRequest {
		t.Errorf("grant to an outside business with no payee_id = %d %v, want 400", code, out)
	}
	code, biz := call(ws, "", http.MethodPost, "/v1/money/mandates", `{"account_id":"`+agentGBP.ID+`","payee_business_id":"GB-COMPANY-01234567",`+
		`"payee_id":"`+acme.ID+`","max_per_pull_minor":3000,"max_per_month_minor":3000,"expires_at":"`+expires+`"}`)
	key, _ := biz["pull_key"].(string)
	if code != http.StatusCreated || !strings.HasPrefix(key, "mdk_") || biz["hold_account_id"] == nil || biz["payee_id"] != acme.ID || biz["payee_name"] != "Acme Hosting Ltd" {
		t.Fatalf("grant to an outside business = %d %v, want 201 with a pull key, a hold account and the payee", code, biz)
	}
	holdAcct := biz["hold_account_id"].(string)
	bizPull := "/v1/money/mandates/" + biz["id"].(string) + "/pull"
	if code, out := call("", key+"x", http.MethodPost, bizPull, `{"amount_minor":1000,"idempotency_key":"acme-1"}`); code != http.StatusNotFound {
		t.Errorf("a pull with the wrong key = %d %v, want 404", code, out)
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = 'GBP'`, ws).Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	// payouts is every posting of the payments out of the holdAcct account, in order: "kind account amount partner_ref?".
	payouts := func() []string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT e.kind, p.account_id, p.amount_minor, e.partner_ref <> '' FROM money_postings p JOIN money_entries e ON e.id = p.entry_id
			WHERE e.kind LIKE 'payment_out%' ORDER BY e.created_at, p.line`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var kind, acct string
			var amount int64
			var ref bool
			if err := rows.Scan(&kind, &acct, &amount, &ref); err != nil {
				t.Fatal(err)
			}
			got = append(got, fmt.Sprintf("%s %s %d %v", kind, acct, amount, ref))
		}
		return got
	}
	holdBalance := func() int64 {
		t.Helper()
		var b int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0) FROM money_account_balances WHERE account_id = $1`, holdAcct).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	for range 2 { // the pull, and its retry: the same pull, the same payment
		code, out := call("", key, http.MethodPost, bizPull, `{"amount_minor":1000,"idempotency_key":"acme-1"}`)
		payment, _ := out["payment"].(map[string]any)
		if code != http.StatusCreated || payment["status"] != "completed" || payment["partner_ref"] == "" || payment["payee_id"] != acme.ID {
			t.Fatalf("the business's pull = %d %v, want 201 with its payment out completed at the partner", code, out)
		}
	}
	if got := postings(); len(got) != 6 || got[4] != agentGBP.ID+" -1000 test" || got[5] != holdAcct+" 1000 test" {
		t.Fatalf("the business's pull posted %v; want the agent's account to the holdAcct account", got)
	}
	want = []string{"payment_out " + holdAcct + " -1000 true", "payment_out " + partnerAccount + " 1000 true"}
	if got := payouts(); strings.Join(got, "; ") != strings.Join(want, "; ") || holdBalance() != 0 {
		t.Fatalf("the payout posted %v with the holdAcct account at %d; want %v, the holdAcct account at zero", got, holdBalance(), want)
	}
	// A pull whose payment fails at the partner (£10.13) comes back from the holdAcct account to the agent's account.
	if code, out := call("", key, http.MethodPost, bizPull, `{"amount_minor":1013,"idempotency_key":"acme-2"}`); code != http.StatusCreated ||
		out["payment"].(map[string]any)["status"] != "failed" {
		t.Fatalf("a pull of £10.13 = %d %v, want 201 with its payment out failed", code, out)
	}
	want = append(want, "payment_out_failed "+holdAcct+" -1013 false", "payment_out_failed "+agentGBP.ID+" 1013 false")
	if got := payouts(); strings.Join(got, "; ") != strings.Join(want, "; ") || holdBalance() != 0 {
		t.Fatalf("after the failed payout the payouts posted %v with the holdAcct account at %d; want %v, the holdAcct account at zero", got, holdBalance(), want)
	}
	if b, _ := store.Balance(ctx, ws, agentGBP.ID); b.TestMinor != 5000 {
		t.Errorf("the agent's balance = %+v; want 5000 test pence: £100 in, £40 to the company, £10 to the business, £10.13 back", b)
	}
	if got := postings(); len(got) != 8 {
		t.Fatalf("the business's two pulls posted %v; want eight postings in all", got)
	}

	// Six pulls of £10.00 at once under £25.00 a month, from an account holding £50.00: two fit, and only two post. The
	// mandate is held until at least three pulls wait for it, so each must count what those before it pulled meanwhile.
	_, race := call(ws, "", http.MethodPost, "/v1/money/mandates", `{"account_id":"`+agentGBP.ID+`","payee_workspace_id":"`+payeeWS+
		`","max_per_pull_minor":1000,"max_per_month_minor":2500,"expires_at":"`+expires+`"}`)
	hold, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(ctx, `SELECT 1 FROM money_mandates WHERE id = $1 FOR UPDATE`, race["id"]); err != nil {
		t.Fatal(err)
	}
	codes := make(chan int, 6)
	for i := range 6 {
		go func() {
			code, _ := call(payeeWS, "", http.MethodPost, "/v1/money/mandates/"+race["id"].(string)+"/pull",
				fmt.Sprintf(`{"amount_minor":1000,"idempotency_key":"race-%d"}`, i))
			codes <- code
		}()
	}
	for waiting, deadline := 0, time.Now().Add(10*time.Second); waiting < 3 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if err := hold.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
	}
	_ = hold.Rollback(ctx)
	posted := 0
	for range 6 {
		if <-codes == http.StatusCreated {
			posted++
		}
	}
	if got := postings(); posted != 2 || len(got) != 12 {
		t.Fatalf("%d of six concurrent pulls posted, %d postings in all; want 2 pulls and 12 postings", posted, len(got))
	}
}
