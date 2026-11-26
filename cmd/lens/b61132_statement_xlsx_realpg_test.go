package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B61.132 — a month on a company's GBP account, asked for as format=xlsx, is an Excel workbook named for the month
// whose Money in, Money out and Closing balance are the JSON statement's to the cent, with one row per line.
func TestMoneyAccountRoutes_TheExcelStatementIsTheJSONStatement(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b61132-statement-xlsx"
	confirmedAccounts(t, pool, ws)
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
		if _, err := store.AddTestMoney(ctx, ws, company.ID, amount, "b61132-in-"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.MoveAgentMoney(ctx, ws, agentAccount.ID, economy.ToAgent, 25_10, "test", "b61132-fund", "owner"); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMoneyAccountRoutes(r, store)
	month := time.Now().UTC().Format("2006-01")
	get := func(format string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/money/accounts/"+company.ID+"/statement?month="+month+"&format="+format, nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	var st economy.MoneyStatement
	if w := get("json"); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &st) != nil {
		t.Fatalf("the JSON statement = %d %s", w.Code, w.Body.String())
	}
	w := get("xlsx")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" ||
		w.Header().Get("Content-Disposition") != `attachment; filename="talyvor-statement-GBP-`+month+`.xlsx"` {
		t.Fatalf("the Excel statement = %d %q %q %.120q; want 200, an Excel workbook named for the month", w.Code,
			w.Header().Get("Content-Type"), w.Header().Get("Content-Disposition"), w.Body.String())
	}
	cells := xlsxSheet(t, w.Body.Bytes(), 1)
	pounds := func(ref string) *big.Rat {
		v, ok := new(big.Rat).SetString(cells[ref].Value)
		if !ok {
			t.Fatalf("%s = %+v; want a number", ref, cells[ref])
		}
		return v
	}
	if st.ClosingMinor != 125_45 || len(st.Lines) != 3 {
		t.Fatalf("the JSON statement closes at %d with %d lines; want 12545 with the two payments in and the move to the agent", st.ClosingMinor, len(st.Lines))
	}
	for _, f := range []struct {
		ref, name string
		minor     int64
	}{{"J7", "Opening balance", st.OpeningMinor}, {"J8", "Money in", st.InMinor}, {"J9", "Money out", st.OutMinor}, {"J10", "Closing balance", st.ClosingMinor},
		{fmt.Sprintf("J%d", 15+len(st.Lines)), "the closing row", st.ClosingMinor}} {
		if got := pounds(f.ref); got.Cmp(big.NewRat(f.minor, 100)) != 0 {
			t.Errorf("%s (%s) = %s; the JSON statement says %d pence", f.name, f.ref, got.FloatString(2), f.minor)
		}
	}
	if cells[fmt.Sprintf("C%d", 15+len(st.Lines))].Inline != "Closing balance" || cells["J11"].Value != "Adds up" {
		t.Errorf("the sheet does not hold one row per line between its opening and closing rows, or does not add up: %+v", cells)
	}
}
