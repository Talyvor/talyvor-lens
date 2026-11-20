package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B30.23 — a month of test activity on a company's GBP account gives a PDF statement for the month whose closing
// balance is what the ledger holds for the account, every line with where its money stands, and no made-up account
// details while the Test partner holds the account.
func TestMoneyAccountRoutes_AMonthsPDFClosesAtTheLedgerBalance(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b3023-statement-pdf"
	confirmedAccounts(t, pool, ws) // B61.52
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(partners.NewRegistry(nil))
	store.SetScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(ctx, ws, "Buyer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	agentAccount, err := store.OpenCurrencyAccount(ctx, ws, agent.ID, "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	for i, amount := range []int64{120_00, 30_55} {
		if _, err := store.AddTestMoney(ctx, ws, company.ID, amount, "b3023-in-"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.MoveAgentMoney(ctx, ws, agentAccount.ID, economy.ToAgent, 25_10, "test", "b3023-fund", "owner"); err != nil {
		t.Fatal(err)
	}
	var ledger int64
	if err := pool.QueryRow(ctx, `SELECT sum(amount_minor)::bigint FROM money_postings WHERE account_id = $1`, company.ID).Scan(&ledger); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMoneyAccountRoutes(r, store)
	month := time.Now().UTC().Format("2006-01")
	req := httptest.NewRequest(http.MethodGet, "/v1/money/accounts/"+company.ID+"/statement?month="+month+"&format=pdf", nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(),
		&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/pdf" || !strings.HasPrefix(w.Body.String(), "%PDF-") {
		t.Fatalf("the month's PDF = %d %q %.80q; want 200, a PDF", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	// Each cell is one string shown on the page, in row order; £ arrives as its WinAnsi octal escape.
	var cells []string
	for _, m := range regexp.MustCompile(`\(((?:[^()\\]|\\.)*)\) Tj`).FindAllStringSubmatch(w.Body.String(), -1) {
		cells = append(cells, strings.NewReplacer(`\(`, "(", `\)`, ")", `\243`, "£", `\267`, "·").Replace(m[1]))
	}
	closing, states := "", 0
	for i, c := range cells {
		if c == "closing" && i+5 < len(cells) {
			closing = cells[i+5]
		}
		if c == "completed" {
			states++
		}
	}
	if want := economy.Money(ledger, "GBP"); ledger != 125_45 || closing != want {
		t.Fatalf("the PDF closes at %q; the ledger holds %d for the account (%s), want 12545. Cells: %q", closing, ledger, want, cells)
	}
	if states != 3 {
		t.Errorf("%d lines read completed; want the two payments in and the move to the agent. Cells: %q", states, cells)
	}
	body := strings.Join(cells, "\n")
	for _, never := range []string{"Sort code", "Account number", "IBAN", "test money", "Preview"} {
		if strings.Contains(body, never) {
			t.Errorf("the PDF says %q; the Test partner's account has no details and the statement no disclaimer:\n%s", never, body)
		}
	}
}
