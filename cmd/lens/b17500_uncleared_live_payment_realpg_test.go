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

// B17.500 — while payments out has no clearance, a person's live payment from a workspace with no key is refused 403
// for the live money it would use and the capability's class, not asked for a passkey first: a key added for it would
// sign a payment that still could not be made. Nothing is posted and no payment row is written.
func TestLivePayment_UnclearedIsRefusedBeforeTheKey(t *testing.T) {
	pool := agentRoutesDB(t)
	const ws = "ws-b17500-uncleared-live"
	confirmedAccounts(t, pool, ws) // B61.52
	ctx := economy.WithUseCountry(context.Background(), "GB")
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
	payee, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
	if err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMoneyPaymentRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	req := httptest.NewRequest(http.MethodPost, "/v1/money/payments", strings.NewReader(`{"payee_id":"`+payee.ID+`","account_id":"`+company.ID+
		`","currency":"GBP","amount_minor":1000,"funding":"live","reference":"Live","idempotency_key":"live-1"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithAuthContext(economy.WithUseCountry(req.Context(), "GB"), owner))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	said := jsonOf(out["error"])
	if w.Code != http.StatusForbidden || !strings.Contains(said, "live") || !strings.Contains(said, "class RED") || said == economy.ErrKeyBeforeLivePayment.Error() {
		t.Errorf("a live payment while payments out is uncleared = %d %v, want 403 naming live money and class RED", w.Code, out)
	}
	var rows, postings int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM money_payments WHERE workspace_id = $1),
		(SELECT count(*) FROM money_entries WHERE workspace_id = $1 AND kind LIKE 'payment_out%')`, ws).Scan(&rows, &postings); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || postings != 0 {
		t.Fatalf("the refused live payment left %d payment rows and %d entries, want nothing", rows, postings)
	}
}
