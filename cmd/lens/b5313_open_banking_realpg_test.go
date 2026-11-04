package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
)

// B53.13 — an account at another bank, connected through the Test open-banking partner: its card payments land in
// spending with their category, their bank and when they were read; they count in a budget; a later read adds only
// what is new; a consent near its end says so until renewed; and disconnecting deletes everything read. A workspace
// that is not a tester's cannot connect while no real partner is configured.
func TestB5313_OpenBankingImportsCategoriseCountAndGoOnDisconnect(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5313"
	store := economy.NewDualTokenStore(nil, pool, nil)
	reg := partners.NewRegistry(nil)
	store.SetBankInfo(reg.AccountInfo)
	r := chi.NewRouter()
	mountMoneyBankRoutes(r, store)
	mountMoneySpendingRoutes(r, store)
	call := func(method, path, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT,
			UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("%s %s = %d %s", method, path, w.Code, w.Body.String())
			}
		}
		return w.Code
	}

	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	if code := call(http.MethodPost, "/v1/money/banks", `{"bank": "Test Bank", "account": "Current"}`, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("a real workspace connected to the Test bank: %d, want 503", code)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET synthetic = true WHERE id = $1`, ws); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	reg.TestAccountInfo().Plant("Test Bank", "Current",
		partners.BankTransaction{ID: "t1", Amount: partners.Money{Minor: 100000, Currency: "GBP"}, Merchant: "Salary", BookedAt: now.Add(-3 * time.Minute)},
		partners.BankTransaction{ID: "t2", Amount: partners.Money{Minor: -2350, Currency: "GBP"}, Merchant: "Corner Shop", MCC: "5411", BookedAt: now.Add(-2 * time.Minute)})
	var conn economy.BankConnection
	if code := call(http.MethodPost, "/v1/money/banks", `{"bank": "Test Bank", "account": "Current"}`, &conn); code != http.StatusCreated {
		t.Fatalf("connect = %d %+v", code, conn)
	}
	if conn.Transactions != 2 || conn.BalanceMinor == nil || *conn.BalanceMinor != 97650 || conn.RefreshedAt == nil || conn.RenewalDue ||
		conn.ConsentExpiresAt.Before(now.AddDate(0, 0, 89)) {
		t.Fatalf("connection = %+v, want 2 transactions, 97650 balance, read now, consent for 90 days", conn)
	}
	if code := call(http.MethodPost, "/v1/money/banks", `{"bank": "TESTFAIL Bank", "account": "Current"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("a bank that refused consent = %d, want 400", code)
	}

	budget, err := store.SetSpendBudget(ctx, ws, economy.SpendBudget{Category: economy.SpendGroceries, Period: "month", AmountMinor: 10000, Currency: "GBP"}, "owner", now)
	if err != nil {
		t.Fatal(err)
	}
	spentOnGroceries := func() int64 {
		t.Helper()
		budgets, err := store.SpendBudgets(ctx, ws, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range budgets {
			if b.ID == budget.ID {
				return b.SpentMinor
			}
		}
		t.Fatal("no budget")
		return 0
	}
	period := "?from=" + now.Add(-time.Hour).Format(time.RFC3339) + "&to=" + now.Add(time.Hour).Format(time.RFC3339)
	spending := func() economy.Spending {
		t.Helper()
		var sp economy.Spending
		if code := call(http.MethodGet, "/v1/money/spending"+period, "", &sp); code != http.StatusOK {
			t.Fatalf("spending = %d", code)
		}
		return sp
	}
	sp := spending()
	if len(sp.Payments) != 1 {
		t.Fatalf("payments = %+v, want the shop only: money in is not spending", sp.Payments)
	}
	if p := sp.Payments[0]; p.Kind != "bank" || p.AmountMinor != 2350 || p.Category != economy.SpendGroceries || p.CategorySource != economy.SpendFromCard ||
		p.Source != "Test Bank" || p.RefreshedAt == nil || p.AgentID != "" {
		t.Fatalf("payment = %+v, want 2350 on groceries from the card's code, from Test Bank as of its read, on the owner's wallet", p)
	}
	if len(sp.Totals) != 1 || sp.Totals[0].ByCategory[economy.SpendGroceries] != 2350 {
		t.Fatalf("totals = %+v", sp.Totals)
	}
	if got := spentOnGroceries(); got != 2350 {
		t.Fatalf("groceries budget spent = %d, want 2350", got)
	}

	// A later read adds only what is new.
	reg.TestAccountInfo().Plant("Test Bank", "Current",
		partners.BankTransaction{ID: "t3", Amount: partners.Money{Minor: -1200, Currency: "GBP"}, Merchant: "Tesco Express", BookedAt: now.Add(-time.Minute)})
	if code := call(http.MethodPost, "/v1/money/banks/"+conn.ID+"/refresh", "", &conn); code != http.StatusOK || conn.Transactions != 3 {
		t.Fatalf("refresh = %d %+v, want 3 transactions", code, conn)
	}
	if got := spentOnGroceries(); got != 3550 {
		t.Fatalf("groceries budget spent = %d, want 3550 (Tesco placed by its name)", got)
	}

	// Two days from the consent's end its renewal is due, until it is renewed.
	if _, err := pool.Exec(ctx, `UPDATE bank_connections SET consent_expires_at = now() + interval '2 days' WHERE id = $1`, conn.ID); err != nil {
		t.Fatal(err)
	}
	var list struct{ Connections []economy.BankConnection }
	if call(http.MethodGet, "/v1/money/banks", "", &list); len(list.Connections) != 1 || !list.Connections[0].RenewalDue {
		t.Fatalf("connections = %+v, want one with its renewal due", list.Connections)
	}
	if code := call(http.MethodPost, "/v1/money/banks/"+conn.ID+"/renew", "", &conn); code != http.StatusOK || conn.RenewalDue {
		t.Fatalf("renew = %d %+v", code, conn)
	}

	// Disconnecting ends the consent and deletes everything read from the account.
	var consentRef string
	if err := pool.QueryRow(ctx, `SELECT consent_ref FROM bank_connections WHERE id = $1`, conn.ID).Scan(&consentRef); err != nil {
		t.Fatal(err)
	}
	if code := call(http.MethodDelete, "/v1/money/banks/"+conn.ID, "", nil); code != http.StatusNoContent {
		t.Fatalf("disconnect = %d", code)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM bank_transactions WHERE workspace_id = $1) + (SELECT count(*) FROM bank_connections WHERE workspace_id = $1)`,
		ws).Scan(&left); err != nil || left != 0 {
		t.Fatalf("rows left after disconnecting = %d %v, want 0", left, err)
	}
	if sp := spending(); len(sp.Payments) != 0 {
		t.Fatalf("payments after disconnecting = %+v", sp.Payments)
	}
	if got := spentOnGroceries(); got != 0 {
		t.Fatalf("groceries budget spent after disconnecting = %d, want 0", got)
	}
	if _, err := reg.TestAccountInfo().Transactions(ctx, consentRef, now); !errors.Is(err, partners.ErrNotFound) {
		t.Fatalf("reading under the ended consent = %v, want not found", err)
	}
}
