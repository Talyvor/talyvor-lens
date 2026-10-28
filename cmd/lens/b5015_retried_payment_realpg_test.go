package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/tenant"
)

// B50.15 — a payment to another company's agent sent 20 times at once with one Idempotency-Key is recorded once on
// the payer's marketplace bill, and the key with another amount is refused (409) and records nothing; a fund or a
// withdraw sent again with its key and another amount answers 409 and moves nothing.
func TestB5015_RetriedCompanyPaymentAndMovesLandOnce(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const payerCo, payeeCo = "ws-b5015-payer", "ws-b5015-payee"
	bank := economy.NewDualTokenStore(nil, pool, nil)
	bank.SetCompanyPayments(market.NewStore(pool))
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, bank, tenant.NewStore(pool))
	call := func(method, path, key, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: payerCo, AuthMethod: auth.MethodJWT, UserID: "owner-" + payerCo, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	agent := func(ws, name string) string {
		t.Helper()
		a, err := bank.CreateAgent(ctx, ws, name, "owner-"+ws)
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	buyer, seller := agent(payerCo, "buyer"), agent(payeeCo, "seller")
	count := func(q string, args ...any) (n int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	pay := func(ulxc int64) (int, string) {
		return call(http.MethodPost, "/v1/workspaces/"+payerCo+"/agents/"+buyer+"/pay", "pay-1",
			fmt.Sprintf(`{"to_agent_id":%q,"amount_ulxc":%d,"memo":"Q3 translation"}`, seller, ulxc))
	}
	codes := make(chan int, 20)
	b501AtOnce(func() { c, _ := pay(10_000_000); codes <- c })
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Errorf("a retry of the payment answered %d, want 200 with the payment made", c)
		}
	}
	const uses = `SELECT count(*) FROM market_uses WHERE agent_id = $1`
	if n := count(uses, buyer); n != 1 {
		t.Fatalf("20 payments to another company's agent at once under one key recorded %d uses, want 1", n)
	}
	if code, body := pay(20_000_000); code != http.StatusConflict || count(uses, buyer) != 1 {
		t.Fatalf("the key with another amount = %d %s, %d uses; want 409 and nothing recorded", code, body, count(uses, buyer))
	}

	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 50000000, 50000000)`, payerCo); err != nil {
		t.Fatal(err)
	}
	const held = `SELECT COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_postings WHERE account = $1`
	for _, step := range []struct {
		move     string
		ulxc     int64
		want     int
		heldULXC int64
	}{
		{"fund", 10_000_000, http.StatusOK, 10_000_000},
		{"fund", 10_000_000, http.StatusOK, 10_000_000},
		{"fund", 20_000_000, http.StatusConflict, 10_000_000},
		{"withdraw", 3_000_000, http.StatusOK, 7_000_000},
		{"withdraw", 4_000_000, http.StatusConflict, 7_000_000},
	} {
		code, body := call(http.MethodPost, "/v1/workspaces/"+payerCo+"/agents/"+buyer+"/"+step.move, step.move+"-1",
			fmt.Sprintf(`{"amount_ulxc":%d}`, step.ulxc))
		if code != step.want || count(held, "agent:"+buyer) != step.heldULXC {
			t.Fatalf("%s of %d under its key = %d %s, agent holds %d; want %d and %d", step.move, step.ulxc, code, body,
				count(held, "agent:"+buyer), step.want, step.heldULXC)
		}
	}
}

// B50.15 — B50.1's lead: money in through a partner, posted 20 times at once under one key, lands once and every one
// of the 20 is answered. It is screened before PostMoney takes its locks: screening takes a connection of its own, and
// one held under the locks while the 19 others wait on them with theirs could leave it none.
func TestB5015_TwentyIdenticalPaymentsInAtOnceLandOnce(t *testing.T) {
	pool := agentRoutesDB(t)
	const ws = "ws-b5015-in"
	ada := economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"}
	b := newB468Payer(t, pool, ws, "GBP", ada, func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
		return sandboxMoneyLayer(t, pool, store, reg)
	})
	var partnerAccount string
	if err := pool.QueryRow(context.Background(), `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = 'GBP'`, ws).
		Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 20)
	b501AtOnce(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := b.store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "payment_in",
			IdempotencyKey: "payment_in:ref-1", Funding: economy.FundingTest, Counterparty: "Grace Hopper", Postings: []economy.MoneyPosting{
				{AccountID: partnerAccount, AmountMinor: -1500}, {AccountID: b.acct.ID, AmountMinor: 1500}}})
		errs <- err
	})
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("one of 20 identical payments in at once = %v, want each answered with the one entry", err)
		}
	}
	b.balance(t, 50000+1500)
	b501Balanced(t, pool, ws)
}
