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

// B30.21 — a CSV of 50 payouts with two bad rows is validated on upload: 48 rows wait for one approval, the two are
// reported with their reasons, and nothing moves. One approval pays the 48 — the ledger shows 48 payments out from
// the hold to the partner — and approving again pays nothing twice.
func TestMoneyPayoutRoutes_FiftyRowsTwoBadOneApproval(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b3021-payouts"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	var payees []string
	for _, name := range []string{"Ada Lovelace", "Grace Hopper", "Mary Somerville"} {
		p, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: name, Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
		if err != nil {
			t.Fatal(err)
		}
		payees = append(payees, p.ID)
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws).Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	// £5,000.00 of test money in, from outside, to the company's account.
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -500000}, {AccountID: company.ID, AmountMinor: 500000}}}); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMoneyPayoutRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, contentType, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Idempotency-Key", "payroll-october")
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	// 48 good rows of £20.00, £21.00 … £67.00 (none ending 13, 14 or 15), one to a payee that does not exist and one
	// whose amount is not an amount.
	var csvRows []string
	var total int64
	for i := 0; i < 48; i++ {
		csvRows = append(csvRows, payees[i%3]+","+jsonOf(20+i)+".00,GBP,Pay "+jsonOf(i+1))
		total += int64(2000 + 100*i)
	}
	csvRows = append(csvRows, "mpayee_nobody,10.00,GBP,Nobody", payees[0]+",ten pounds,GBP,Bad amount")
	body := "payee,amount,currency,reference\n" + strings.Join(csvRows, "\n") + "\n"
	paymentsOut := func() (n int, minor int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(p.amount_minor), 0)::bigint FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
			WHERE e.workspace_id = $1 AND e.kind = 'payment_out' AND p.account_id = $2`, ws, partnerAccount).Scan(&n, &minor); err != nil {
			t.Fatal(err)
		}
		return n, minor
	}
	rowsOf := func(out map[string]any) []map[string]any {
		var rows []map[string]any
		for _, r := range out["rows"].([]any) {
			rows = append(rows, r.(map[string]any))
		}
		return rows
	}

	code, out := call(http.MethodPost, "/v1/money/payouts", "text/csv", body)
	if code != http.StatusCreated || out["status"] != "awaiting_approval" || out["rows_total"] != float64(50) || out["rows_valid"] != float64(48) ||
		out["rows_invalid"] != float64(2) || out["totals_minor"].(map[string]any)["GBP"] != float64(total) {
		t.Fatalf("upload = %d %v, want 201 awaiting approval with 48 of 50 rows valid, £%d.00 in all", code, out, total/100)
	}
	batch := out["id"].(string)
	rows := rowsOf(out)
	if d, _ := rows[48]["detail"].(string); rows[48]["status"] != "invalid" || !strings.Contains(d, "no such payee mpayee_nobody") {
		t.Errorf("row 49 = %v, want invalid for its payee", rows[48])
	}
	if d, _ := rows[49]["detail"].(string); rows[49]["status"] != "invalid" || !strings.Contains(d, `amount "ten pounds" is not`) {
		t.Errorf("row 50 = %v, want invalid for its amount", rows[49])
	}
	if rows[0]["status"] != "valid" || rows[0]["payee_name"] != "Ada Lovelace" || rows[0]["amount_minor"] != float64(2000) {
		t.Errorf("row 1 = %v, want valid, £20.00 to Ada Lovelace", rows[0])
	}
	if n, _ := paymentsOut(); n != 0 {
		t.Fatalf("before approval %d payments went out, want none", n)
	}
	if code, again := call(http.MethodPost, "/v1/money/payouts", "text/csv", body); code != http.StatusCreated || again["id"] != batch {
		t.Errorf("the same upload again = %d %v, want the same batch", code, again)
	}
	if code, _ := call(http.MethodPost, "/v1/money/payouts", "application/json", `{"rows":[{"payee_id":"`+payees[0]+`","amount_minor":500,"currency":"GBP"}]}`); code != http.StatusConflict {
		t.Errorf("the same key for other rows = %d, want 409", code)
	}

	code, out = call(http.MethodPost, "/v1/money/payouts/"+batch+"/approve", "application/json", "")
	if code != http.StatusOK || out["status"] != "sent" || out["approved_by"] == "" {
		t.Fatalf("approve = %d %v, want 200 sent", code, out)
	}
	completed := 0
	for _, row := range rowsOf(out) {
		if row["status"] == "completed" && strings.HasPrefix(row["payment_id"].(string), "mpay_") {
			completed++
		}
	}
	if completed != 48 {
		t.Errorf("%d rows completed with a payment, want 48", completed)
	}
	if n, minor := paymentsOut(); n != 48 || minor != total {
		t.Errorf("the ledger shows %d payments out, £%d.00 in all, want 48 of £%d.00", n, minor/100, total/100)
	}
	if b, err := store.Balance(ctx, ws, company.ID); err != nil || b.TestMinor != 500000-total {
		t.Errorf("the company's account holds %+v (%v), want %d test pence", b, err, 500000-total)
	}
	if code, again := call(http.MethodPost, "/v1/money/payouts/"+batch+"/approve", "application/json", ""); code != http.StatusOK || again["status"] != "sent" {
		t.Errorf("approve again = %d %v, want 200 sent", code, again)
	}
	if n, _ := paymentsOut(); n != 48 {
		t.Errorf("after approving again the ledger shows %d payments out, want still 48", n)
	}
	if code, out := call(http.MethodGet, "/v1/money/payouts", "", ""); code != http.StatusOK || len(out["payouts"].([]any)) != 1 {
		t.Errorf("list = %d %v, want the one batch", code, out)
	}
	if code, out := call(http.MethodGet, "/v1/money/payouts/"+batch, "", ""); code != http.StatusOK || len(out["rows"].([]any)) != 50 {
		t.Errorf("read = %d, want 200 with 50 rows", code)
	}
}
