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
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/fees"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B59.42 — a route of either side names its money: the currency of an account, a payment or a conversion, and the
// rail of a payee (a bank account or a USDC address). A stablecoin workspace is refused a bank payee and a GBP account,
// a fiat workspace a USDC address and a USDC account, and either is refused a conversion across the sides, each 409
// wrong_money_kind with the workspace's kind and nothing on the ledger; a hybrid workspace is refused none of them.
func TestB5942_EitherRoutesJudgeTheMoneyTheyName(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, b487Screening{})
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	store.SetFXMargin(func(ctx context.Context, q economy.FeeQuerier, workspaceID string) (int64, string, error) {
		return billing.FXMarginBPS(ctx, q, workspaceID, fees.Defaults())
	})

	r := chi.NewRouter()
	mountMoneyAccountRoutes(r, store)
	mountMoneyPayeeRoutes(r, store)
	mountMoneyPaymentRoutes(r, store)
	mountMoneyConvertRoutes(r, store)
	call := func(ws, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT,
			UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	entries := func(ws string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_entries WHERE workspace_id = $1`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// account is a workspace of kind with an agent, the company's GBP and USDC accounts holding test money, and a saved
	// bank payee and USDC address, all made while it was hybrid, as a workspace made before B59.6 has them.
	type account struct{ ws, agent, bank, address string }
	open := func(kind string) account {
		t.Helper()
		a := account{ws: "ws-b5942-" + kind}
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, $1, $1, 'business', 'hybrid')`, a.ws); err != nil {
			t.Fatal(err)
		}
		agent, err := store.CreateAgent(ctx, a.ws, "Payer", "owner")
		if err != nil {
			t.Fatal(err)
		}
		a.agent = agent.ID
		for _, c := range []struct {
			currency string
			minor    int64
		}{{"GBP", 500_00}, {"USDC", 500_000_000}} {
			acct, err := store.OpenCurrencyAccount(ctx, a.ws, "", c.currency, "test")
			if err != nil {
				t.Fatal(err)
			}
			var partner string
			if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = $2`,
				a.ws, c.currency).Scan(&partner); err != nil {
				t.Fatal(err)
			}
			if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: a.ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
				IdempotencyKey: "top-up-" + c.currency, Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
					{AccountID: partner, AmountMinor: -c.minor}, {AccountID: acct.ID, AmountMinor: c.minor}}}); err != nil {
				t.Fatal(err)
			}
		}
		bank, err := store.CreateOutsidePayee(ctx, a.ws, economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
		if err != nil {
			t.Fatal(err)
		}
		address, err := store.CreateOutsidePayee(ctx, a.ws, economy.OutsidePayee{Name: "Grace Hopper", Country: "GB", Network: "base",
			Address: "0x00000000000000000000000000000000000ada42"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE money_payees SET confirmed_at = now(), confirmed_by = 'passkey' WHERE id = $1`, address.ID); err != nil {
			t.Fatal(err)
		}
		a.bank, a.address = bank.ID, address.ID
		if _, err := pool.Exec(ctx, `UPDATE workspaces SET money_kind = $2 WHERE id = $1`, a.ws, kind); err != nil {
			t.Fatal(err)
		}
		return a
	}
	accounts := map[string]account{}
	for _, kind := range []string{economy.MoneyKindFiat, economy.MoneyKindStablecoin, economy.MoneyKindHybrid} {
		accounts[kind] = open(kind)
	}

	type step struct{ name, path, body string }
	pay := func(a account, payee, currency, minor, key string) step {
		return step{"pays " + key, "/v1/money/payments", `{"payee_id":"` + payee + `","currency":"` + currency + `","amount_minor":` + minor +
			`,"reference":"B59.42","idempotency_key":"` + key + `"}`}
	}
	openAccount := func(currency string) step {
		return step{"opens a " + currency + " account", "/v1/money/accounts", `{"currency":"` + currency + `","funding":"test"}`}
	}
	save := func(name, body string) step { return step{"saves " + name, "/v1/money/payees", body} }
	bankPayee := save("a bank payee", `{"name":"Alan Turing","country":"GB","sort_code":"200000","account_number":"11223344"}`)
	usdcPayee := save("a USDC address", `{"name":"Joan Clarke","country":"GB","network":"base","address":"0x00000000000000000000000000000000000c1a42"}`)
	convert := func(a account, sell, buy string) step {
		return step{"converts " + sell + " to " + buy, "/v1/workspaces/" + a.ws + "/agents/" + a.agent + "/money/convert/quote",
			`{"sell_currency":"` + sell + `","buy_currency":"` + buy + `","sell_minor":10000}`}
	}

	fiat, stablecoin, hybrid := accounts[economy.MoneyKindFiat], accounts[economy.MoneyKindStablecoin], accounts[economy.MoneyKindHybrid]
	refused := map[string][]step{
		economy.MoneyKindStablecoin: {pay(stablecoin, stablecoin.bank, "GBP", "1000", "bank"), openAccount("GBP"), bankPayee,
			convert(stablecoin, "GBP", "USDC")},
		economy.MoneyKindFiat: {pay(fiat, fiat.address, "USDC", "10000000", "address"), openAccount("USDC"), usdcPayee,
			convert(fiat, "GBP", "USDC")},
	}
	// Each single kind's own side is open to it.
	if code, out := call(fiat.ws, "/v1/money/accounts", `{"currency":"EUR","funding":"test"}`); code != http.StatusCreated {
		t.Fatalf("a fiat workspace opens a EUR account = %d %v, want it opened", code, out)
	}
	if code, out := call(stablecoin.ws, "/v1/money/payments", pay(stablecoin, stablecoin.address, "USDC", "10000000", "own-side").body); code != http.StatusCreated {
		t.Fatalf("a stablecoin workspace pays a USDC address = %d %v, want it paid", code, out)
	}
	for kind, steps := range refused {
		ws := accounts[kind].ws
		for _, s := range steps {
			before := entries(ws)
			code, out := call(ws, s.path, s.body)
			if code != http.StatusConflict || out["code"] != "wrong_money_kind" || out["money_kind"] != kind {
				t.Errorf("a %s workspace %s = %d %v, want 409 wrong_money_kind naming %s", kind, s.name, code, out, kind)
			}
			if after := entries(ws); after != before {
				t.Errorf("a %s workspace %s: %d money entries became %d, want nothing posted", kind, s.name, before, after)
			}
		}
	}
	if got := b468Postings(t, pool, stablecoin.ws, "bank"); got != "" {
		t.Errorf("the stablecoin workspace's payment to a bank payee posted %q, want nothing", got)
	}
	if got := b468Postings(t, pool, fiat.ws, "address"); got != "" {
		t.Errorf("the fiat workspace's payment to a USDC address posted %q, want nothing", got)
	}

	// A hybrid workspace holds both sides: every one of those is open to it. A conversion across the sides is Instant
	// transfer's (B59.14): it is refused as a conversion, never for the kind.
	for _, s := range []step{pay(hybrid, hybrid.bank, "GBP", "1000", "bank"), pay(hybrid, hybrid.address, "USDC", "10000000", "address"),
		openAccount("EUR"), bankPayee, usdcPayee} {
		if code, out := call(hybrid.ws, s.path, s.body); code != http.StatusCreated {
			t.Errorf("a hybrid workspace %s = %d %v, want it done", s.name, code, out)
		}
	}
	if code, out := call(hybrid.ws, convert(hybrid, "GBP", "USDC").path, convert(hybrid, "GBP", "USDC").body); out["code"] == "wrong_money_kind" {
		t.Errorf("a hybrid workspace converts GBP to USDC = %d %v, refused for its kind", code, out)
	}
	if got, want := b468Postings(t, pool, hybrid.ws, "bank"), "payment_out_hold company -1000; payment_out_hold hold 1000; payment_out hold -1000; payment_out partner 1000"; got != want {
		t.Errorf("the hybrid workspace's payment to a bank payee posted %q, want %q", got, want)
	}
	if got, want := b468Postings(t, pool, hybrid.ws, "address"), "payment_out_hold company -10000000; payment_out_hold hold 10000000; payment_out hold -10000000; payment_out partner 10000000"; got != want {
		t.Errorf("the hybrid workspace's payment to a USDC address posted %q, want %q", got, want)
	}
}
