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
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/issuerfake"
	"github.com/talyvor/lens/internal/partners"
)

// B53.18 — the synthetic key charges a test workspace's single-use money card at a merchant, as its issuer would: a
// charge at Tesco on its Tesco card is approved once, posts one card_charge with MCC 5411 and country GB and shows under
// groceries; a second charge on the used card is declined; a card of another workspace is 404; and a workspace that is
// not synthetic is refused 403 with nothing written.
func TestB5318_ASyntheticChargeOnATestWorkspacesMoneyCardPostsOnceWithItsMerchantDetails(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, syntheticKey = "ws-b5318", "b5318-synthetic-key"
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b5318"), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
				Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool),
				Switches: money.SwitchSet{{Capability: money.CapCards, Partner: issuerfake.Name}: true}})
			if err != nil {
				t.Fatal(err)
			}
			if err := layer.Register(issuerfake.New(store.AuthorizeCardCharge).Adapter()); err != nil {
				t.Fatal(err)
			}
			return layer
		})
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, false)`, ws); err != nil {
		t.Fatal(err)
	}
	batch, err := b.store.RequestCardBatch(ctx, ws, economy.CardBatch{AgentID: b.acct.AgentID, Currency: "GBP", IdempotencyKey: "shop",
		Cards: []economy.MoneyCard{{Merchant: "Tesco", CapMinor: 5000}}})
	if err != nil {
		t.Fatal(err)
	}
	if batch, err = b.store.ApproveCardBatch(ctx, ws, batch.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	card := batch.Cards[0].ID

	r := chi.NewRouter()
	mountSyntheticDueRoutes(r, syntheticKey, syntheticDeps{audit: pool, due: syntheticDueDeps{db: pool, moneyCards: b.store}})
	type answer struct {
		AuthorizationID string `json:"authorization_id"`
		Approved        bool   `json:"approved"`
		Reason          string `json:"reason"`
		MCC             string `json:"mcc"`
		Country         string `json:"country"`
		Channel         string `json:"channel"`
		Wallet          string `json:"wallet"`
	}
	charge := func(workspace, cardID string) (int, answer) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/synthetic/workspaces/"+workspace+"/money/cards/"+cardID+"/charge",
			strings.NewReader(`{"merchant": "Tesco", "amount_minor": 3120}`))
		req.Header.Set(syntheticKeyHeader, syntheticKey)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out answer
		if w.Code == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatalf("charge answered %s: %v", w.Body.String(), err)
			}
		}
		return w.Code, out
	}
	written := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM money_card_authorizations WHERE workspace_id = $1)
			+ (SELECT count(*) FROM money_entries WHERE workspace_id = $1 AND kind = 'card_charge')`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if code, _ := charge(ws, card); code != http.StatusForbidden || written() != 0 {
		t.Fatalf("a charge on a real workspace's card = %d with %d rows written, want 403 and none", code, written())
	}
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET synthetic = true WHERE id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if code, _ := charge(ws, "mcard_not_this_workspaces"); code != http.StatusNotFound || written() != 0 {
		t.Fatalf("a charge on no card of the workspace = %d, want 404 and nothing written", code)
	}

	code, got := charge(ws, card)
	if want := (answer{AuthorizationID: got.AuthorizationID, Approved: true, MCC: "5411", Country: "GB", Channel: money.ChannelContactless,
		Wallet: money.WalletGooglePay}); code != http.StatusOK || got != want || got.AuthorizationID == "" {
		t.Fatalf("a charge at Tesco on its card = %d %+v, want 200 %+v", code, got, want)
	}
	var mcc, country string
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*), min(card_mcc), min(card_country) FROM money_entries WHERE workspace_id = $1 AND kind = 'card_charge'`, ws).
		Scan(&n, &mcc, &country); err != nil || n != 1 || mcc != "5411" || country != "GB" {
		t.Fatalf("card_charge entries = %d MCC %q country %q (%v), want one with 5411 GB", n, mcc, country, err)
	}
	if got, want := b4610Postings(t, pool, ws), "card_hold agent -5000; card_hold hold 5000; card_charge hold -5000; card_charge partner 3120; card_charge agent 1880"; got != want {
		t.Fatalf("the card posted %q, want %q", got, want)
	}

	if code, again := charge(ws, card); code != http.StatusOK || again.Approved || again.Reason == "" {
		t.Fatalf("a second charge on the used card = %d %+v, want 200 declined with a reason", code, again)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_entries WHERE workspace_id = $1 AND kind = 'card_charge'`, ws).Scan(&n); err != nil || n != 1 {
		t.Fatalf("card_charge entries after the second charge = %d (%v), want still one", n, err)
	}

	spend := chi.NewRouter()
	mountMoneySpendingRoutes(spend, b.store)
	req := httptest.NewRequest(http.MethodGet, "/v1/money/spending?from=2000-01-01&to=2100-01-01", nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT,
		UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
	w := httptest.NewRecorder()
	spend.ServeHTTP(w, req)
	var out economy.Spending
	if err := json.Unmarshal(w.Body.Bytes(), &out); w.Code != http.StatusOK || err != nil || len(out.Payments) != 1 ||
		out.Payments[0].Merchant != "Tesco" || out.Payments[0].Category != economy.SpendGroceries || out.Payments[0].AmountMinor != 3120 {
		t.Fatalf("spending = %d %s, want the one Tesco payment of 3120 under groceries", w.Code, w.Body.String())
	}
}
