package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
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
	// B61.131: the closing row is the words, money out, money in, then the balance; a line shows where its money
	// stands only when it is not complete.
	cells := pdfTexts(w.Body.Bytes())
	closing, states := "", 0
	for i, c := range cells {
		if c == "Closing balance" && i+3 < len(cells) {
			closing = cells[i+3]
		}
		if c == "Pending" || c == "Failed" || c == "Returned" {
			states++
		}
	}
	if want := economy.MoneyFigure(ledger, "GBP"); ledger != 125_45 || closing != want {
		t.Fatalf("the PDF closes at %q; the ledger holds %d for the account (%s), want 12545. Cells: %q", closing, ledger, want, cells)
	}
	if sums := addsUp(t, cells); len(sums) != 1 || sums[0][3] != economy.Money(ledger, "GBP") {
		t.Errorf("the PDF adds up as %v; want one closing at %s", sums, economy.Money(ledger, "GBP"))
	}
	if states != 0 || !slices.Contains(cells, "3 lines") {
		t.Errorf("%d lines read as not complete, or the statement does not count 3 lines: the two payments in and the move to the agent are complete. Cells: %q", states, cells)
	}
	body := strings.Join(cells, "\n")
	for _, never := range []string{"Sort code", "Account number", "IBAN", "test money", "Preview"} {
		if strings.Contains(body, never) {
			t.Errorf("the PDF says %q; the Test partner's account has no details and the statement no disclaimer:\n%s", never, body)
		}
	}
}
