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

// B30.147 — a payment's ledger entry, readable: an international payment carries its entry_id once it has completed,
// and GET /v1/money/payments/{id}/entry answers every posting of that entry with its account's purpose, summing to
// zero in GBP and in EUR, its revenue posting Talyvor's fee and its EUR what arrives. The figures are checked against
// the ledger's own rows, not the answer's word for them. An agent's key reads only its own payment's entry.
func TestB30147_PaymentEntryReadsEveryPostingAndTheFee(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b30147-entry"
	confirmedAccounts(t, pool, ws) // B61.52
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	acct := allowanceAccount(t, pool, store, ws, "", "GBP", 500_00)
	payee, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Grace Hopper", Country: "DE", IBAN: "DE89370400440532013000", BIC: "COBADEFFXXX"})
	if err != nil {
		t.Fatal(err)
	}
	q, err := store.QuoteInternational(ctx, ws, economy.OutsidePayment{PayeeID: payee.ID, AccountID: acct, Currency: "GBP", AmountMinor: 100_00})
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.PayOutside(ctx, ws, economy.OutsidePayment{PayeeID: payee.ID, AccountID: acct, Currency: "GBP", AmountMinor: 100_00,
		QuoteID: q.ID, Reference: "Rechnung 7", IdempotencyKey: "intl"})
	if err != nil || p.Status != economy.PaymentCompleted || p.FeeMinor <= 0 {
		t.Fatalf("the international payment = %+v, %v; want completed with a fee", p, err)
	}
	var ledgerEntry string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_entries WHERE workspace_id = $1 AND kind = 'payment_out' AND idempotency_key = 'payment_out:intl'`,
		ws).Scan(&ledgerEntry); err != nil {
		t.Fatal(err)
	}
	if p.EntryID != ledgerEntry {
		t.Fatalf("the payment's entry_id = %q, want its payment_out entry %s", p.EntryID, ledgerEntry)
	}

	agent, err := store.CreateAgent(ctx, ws, "Other", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, ws, agent.ID, "key-b30147-other"); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountMoneyPaymentRoutes(r, store)
	get := func(who *auth.AuthContext, path string) (int, []byte) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.Bytes()
	}
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	code, body := get(owner, "/v1/money/payments/"+p.ID+"/entry")
	var e economy.PaymentEntry
	if err := json.Unmarshal(body, &e); code != http.StatusOK || err != nil {
		t.Fatalf("GET the payment's entry = %d %s (%v), want 200", code, body, err)
	}
	if e.EntryID != ledgerEntry || e.PaymentID != p.ID {
		t.Fatalf("the entry read = %s for %s, want %s for %s", e.EntryID, e.PaymentID, ledgerEntry, p.ID)
	}

	// The answer's postings are the ledger's rows, line for line.
	rows, err := pool.Query(ctx, `SELECT p.account_id, a.purpose, p.amount_minor, p.currency FROM money_postings p
		JOIN money_accounts a ON a.id = p.account_id WHERE p.entry_id = $1 ORDER BY p.line`, ledgerEntry)
	if err != nil {
		t.Fatal(err)
	}
	var ledger []economy.PaymentEntryPosting
	for rows.Next() {
		var lp economy.PaymentEntryPosting
		if err := rows.Scan(&lp.AccountID, &lp.Purpose, &lp.AmountMinor, &lp.Currency); err != nil {
			t.Fatal(err)
		}
		ledger = append(ledger, lp)
	}
	rows.Close()
	if len(e.Postings) != len(ledger) {
		t.Fatalf("the entry read %d postings, the ledger holds %d: %+v", len(e.Postings), len(ledger), e.Postings)
	}
	sums, revenue, arrives := map[string]int64{}, int64(0), int64(0)
	for i, lp := range ledger {
		if e.Postings[i] != lp {
			t.Errorf("posting %d read %+v, the ledger holds %+v", i+1, e.Postings[i], lp)
		}
		sums[lp.Currency] += lp.AmountMinor
		if lp.Purpose == economy.MoneyRevenue && lp.Currency == "GBP" {
			revenue += lp.AmountMinor
		}
		if lp.Currency == "EUR" && lp.AmountMinor > 0 {
			arrives += lp.AmountMinor
		}
	}
	if len(sums) != 2 || sums["GBP"] != 0 || sums["EUR"] != 0 {
		t.Errorf("the ledger's postings sum to %v, want 0 in GBP and in EUR", sums)
	}
	if revenue != p.FeeMinor || arrives != p.ArrivesMinor {
		t.Errorf("the ledger posted %d to revenue and %d in EUR, want the fee %d and what arrives %d", revenue, arrives, p.FeeMinor, p.ArrivesMinor)
	}
	if want := []economy.PaymentEntrySum{{Currency: "GBP"}, {Currency: "EUR"}}; len(e.Sums) != 2 || e.Sums[0] != want[0] || e.Sums[1] != want[1] {
		t.Errorf("the entry's sums read %+v, want %+v", e.Sums, want)
	}

	other := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-b30147-other", Scopes: []string{auth.ScopeProxy}}
	if code, body := get(other, "/v1/money/payments/"+p.ID+"/entry"); code != http.StatusNotFound || strings.Contains(string(body), ledgerEntry) {
		t.Errorf("another agent's key reading the company's payment's entry = %d %s, want 404 and nothing of it", code, body)
	}
}
