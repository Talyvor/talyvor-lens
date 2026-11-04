package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B53.6 — the Coach words code's findings, its validator keeps only sentences true to them, product and debt
// questions get free help, a gambling finding offers the block, and the Coach can only read.

type b536Bank struct{ now time.Time }

func (b b536Bank) Spending(context.Context, string, time.Time, time.Time) (economy.Spending, error) {
	at := b.now.Add(-time.Hour)
	return economy.Spending{Payments: []economy.SpendPayment{
		{Merchant: "Pret", AmountMinor: 450, Currency: "GBP", Category: economy.SpendEatingOut, CreatedAt: at},
		{Merchant: "Pret", AmountMinor: 450, Currency: "GBP", Category: economy.SpendEatingOut, CreatedAt: at},
		{Merchant: "Bet365", MCC: "7995", AmountMinor: 500, Currency: "GBP", Category: economy.SpendEntertainment, CreatedAt: at},
		{Merchant: "Tesco", AmountMinor: 3000, Currency: "GBP", Category: economy.SpendGroceries, CreatedAt: b.now.AddDate(0, 0, -10)},
	}}, nil
}

func (b b536Bank) SpendFindings(context.Context, string, time.Time, time.Time) ([]economy.SpendFinding, error) {
	three := 3
	return []economy.SpendFinding{
		{Rule: economy.FindingDuplicate, Merchant: "Pret", Category: economy.SpendEatingOut, AmountMinor: 450, Currency: "GBP", MinutesApart: &three},
		{Rule: economy.FindingGambling, Merchant: "Bet365", MCC: "7995", Category: economy.SpendEntertainment, AmountMinor: 500, Currency: "GBP"},
	}, nil
}

func (b b536Bank) SpendBudgets(context.Context, string, time.Time) ([]economy.SpendBudget, error) {
	return []economy.SpendBudget{{Category: economy.SpendEatingOut, Period: economy.BudgetMonth, AmountMinor: 20000, Currency: "GBP",
		PeriodEnd: b.now.Add(9*24*time.Hour - time.Hour), SpentMinor: 16000, LeftMinor: 4000, Percent: 80}}, nil
}

// b536Model answers each fact with a sentence chosen to pass or to break the validator.
type b536Model struct{ calls int }

func (m *b536Model) Infer(_ context.Context, _ string, input string) (string, error) {
	m.calls++
	switch {
	case strings.Contains(input, `"kind":"duplicate"`):
		return "You paid Pret 4.50 GBP twice, 7 minutes apart.", nil // 7 is not in the finding
	case strings.Contains(input, `"kind":"gambling"`):
		return "You paid 5.00 GBP at Bet365.", nil // a sensitive merchant the finding does not name
	case strings.Contains(input, `"kind":"budget"`):
		return "Eating out is at 80% of this month's budget with 9 days left: 40.00 GBP to go.", nil
	}
	return "A payday loan would cover this week's 39.00 GBP.", nil // a product
}

func b536Do(t *testing.T, r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: "ws_b536", AuthMethod: auth.MethodJWT,
		UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%s %s = %d %s", method, path, w.Code, w.Body.String())
	}
	return w
}

func TestB536_Coach(t *testing.T) {
	model := &b536Model{}
	r := chi.NewRouter()
	mountMoneyCoachRoutes(r, b536Bank{now: time.Now().UTC()}, model, "test-model")

	var report economy.CoachReport
	if err := json.Unmarshal(b536Do(t, r, http.MethodGet, "/v1/money/coach", "").Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	byKind := map[string]economy.CoachLine{}
	for _, l := range append(report.Lines, report.Review...) {
		byKind[l.Fact.Kind] = l
	}

	t.Run("a sentence with a number not in its finding is replaced by the template", func(t *testing.T) {
		d := byKind[economy.FindingDuplicate]
		if d.Source != economy.CoachFromTemplate || !strings.Contains(d.Sentence, "3 minutes apart") || strings.Contains(d.Sentence, "7") {
			t.Fatalf("duplicate = %+v, want the template with 3 minutes", d)
		}
		if b := byKind["budget"]; b.Source != economy.CoachFromModel || b.Sentence != "Eating out is at 80% of this month's budget with 9 days left: 40.00 GBP to go." {
			t.Fatalf("budget = %+v, want the model's true sentence", b)
		}
		if w := byKind["week"]; w.Source != economy.CoachFromTemplate || strings.Contains(w.Sentence, "loan") {
			t.Fatalf("week = %+v, want the template over a product", w)
		}
	})

	t.Run("a gambling finding offers the block and never names the merchant", func(t *testing.T) {
		g := byKind[economy.FindingGambling]
		if g.Offer == nil || g.Offer.Kind != "block_gambling" || len(g.Help) != 1 || g.Help[0].Name != "GamCare" {
			t.Fatalf("gambling = %+v, want the block offer and GamCare", g)
		}
		if g.Fact.Merchant != "" || strings.Contains(g.Sentence, "Bet365") || g.Source != economy.CoachFromTemplate {
			t.Fatalf("gambling = %+v, want the merchant kept from the model and the sentence", g)
		}
	})

	t.Run("product, credit and debt questions get the free-help pointers and no product", func(t *testing.T) {
		before := model.calls
		for q, want := range map[string][]string{
			"Which savings account should I open?": {"MoneyHelper"},
			"Should I take a loan?":                {"StepChange", "National Debtline", "MoneyHelper"},
			"Is a stocks and shares ISA worth it?": {"MoneyHelper"},
			"How do I stop betting?":               {"GamCare", "MoneyHelper"},
		} {
			var a economy.CoachAnswer
			body, _ := json.Marshal(map[string]string{"question": q})
			if err := json.Unmarshal(b536Do(t, r, http.MethodPost, "/v1/money/coach/ask", string(body)).Body.Bytes(), &a); err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, h := range a.Help {
				names = append(names, h.Name)
			}
			if !slices.Equal(names, want) || !strings.Contains(a.Text, "can't recommend") {
				t.Fatalf("%q = %+v, want only %v", q, a, want)
			}
		}
		if model.calls != before {
			t.Fatal("a product question reached the model")
		}
	})

	t.Run("the Coach cannot pay, approve or issue", func(t *testing.T) {
		var methods []string
		reader := reflect.TypeFor[economy.CoachReader]()
		for i := range reader.NumMethod() {
			methods = append(methods, reader.Method(i).Name)
		}
		if !slices.Equal(methods, []string{"SpendBudgets", "SpendFindings", "Spending"}) {
			t.Fatalf("CoachReader = %v, want only its three reads", methods)
		}
		var routes []string
		_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			routes = append(routes, method+" "+route)
			return nil
		})
		slices.Sort(routes)
		if !slices.Equal(routes, []string{"GET /v1/money/coach", "POST /v1/money/coach/ask"}) {
			t.Fatalf("routes = %v", routes)
		}
	})
}
