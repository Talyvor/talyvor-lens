package main

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/coinbase"
	"github.com/talyvor/lens/internal/money/coinbase/cdpfake"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/tenant"
)

type b485Pushes chan nativepush.Message

func (p b485Pushes) Send(_ context.Context, _, _ string, m nativepush.Message) error {
	p <- m
	return nil
}

// B48.5 — a workspace's budget for the network fees a paymaster pays for its agents: each sponsored send is one row
// held at its quoted fee and charged what the chain took; a send the budget cannot cover is refused with a clear error
// and the owner asked to top up once, however many sends are refused; a top-up lets the next one through. The
// paymaster and the chain themselves are in internal/money/smartaccount's devnet test.
func TestB485_TheWalletPaysNetworkFeesWithinTheWorkspacesBudget(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, base = "ws-b485", int64(8453)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetGasBudgetDefault(1_000)
	store.ServeGas(base, "", "")
	push := make(b485Pushes, 4)
	store.SetNativePush(push)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, platform, public_key, attestation, push_token)
		VALUES ('dev-b485', $1, 'test', '\x00', '\x00', 'tok')`, ws); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	admin := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "talyvor-ops", IsAdmin: true, Scopes: []string{auth.ScopeKeys}}
	as := owner
	call := func(method, path, body string) (int, economy.GasBudget) {
		t.Helper()
		req := httptest.NewRequest(method, "/v1/workspaces/"+ws+"/agents/gas-budget"+path, strings.NewReader(body))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), as)))
		var b economy.GasBudget
		if w.Code == http.StatusOK {
			_ = json.Unmarshal(w.Body.Bytes(), &b)
		}
		return w.Code, b
	}
	row := func(hash string) (held int64, charged *int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT held_wei, charged_wei FROM gas_sponsorships WHERE chain_id = $1 AND op_ref = $2 AND workspace_id = $3`,
			base, hash, ws).Scan(&held, &charged); err != nil {
			t.Fatal(err)
		}
		return held, charged
	}

	gas := store.SponsoredGas(ws, base)
	// A send quoted at 600 wei is held, and charged the 420 the chain took: one row, and a second charge changes nothing.
	if err := gas.Hold(ctx, "0xop1", big.NewInt(600)); err != nil {
		t.Fatal(err)
	}
	if err := gas.Hold(ctx, "0xop1", big.NewInt(600)); err != nil {
		t.Fatal(err)
	}
	if code, b := call(http.MethodGet, "?chain_id=8453", ""); code != http.StatusOK || b.HeldWei != 600 || b.LeftWei != 400 {
		t.Fatalf("while it is on its way = %d %+v", code, b)
	}
	for range 2 {
		if err := gas.Charge(ctx, "0xop1", big.NewInt(420)); err != nil {
			t.Fatal(err)
		}
	}
	if held, charged := row("0xop1"); held != 600 || charged == nil || *charged != 420 {
		t.Fatalf("the sponsorship's row holds %d and is charged %v, want 600 and 420", held, charged)
	}

	// A send the bundler refused is released and costs nothing.
	if err := gas.Hold(ctx, "0xop2", big.NewInt(100)); err != nil {
		t.Fatal(err)
	}
	if err := gas.Release(ctx, "0xop2"); err != nil {
		t.Fatal(err)
	}
	if code, b := call(http.MethodGet, "?chain_id=8453", ""); code != http.StatusOK || b.ChargedWei != 420 || b.HeldWei != 0 || b.LeftWei != 580 || b.TopUpAsked {
		t.Fatalf("after one charge and one release = %d %+v", code, b)
	}

	// 580 left and three sends that may cost 600 — the released one sent again among them: all are refused, nothing
	// is held, and the owner is asked once.
	for _, h := range []string{"0xop3", "0xop4", "0xop2"} {
		err := gas.Hold(ctx, h, big.NewInt(600))
		if !errors.Is(err, smartaccount.ErrGasBudgetSpent) || !strings.Contains(err.Error(), "580 wei left") || !strings.Contains(err.Error(), "top it up") {
			t.Fatalf("a send over the budget: %v", err)
		}
	}
	var holds int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM gas_sponsorships WHERE workspace_id = $1`, ws).Scan(&holds); err != nil || holds != 2 {
		t.Fatalf("%d sponsorship rows, want 2 (%v)", holds, err)
	}
	if code, b := call(http.MethodGet, "?chain_id=8453", ""); code != http.StatusOK || !b.TopUpAsked {
		t.Fatalf("after the refusals = %d %+v, want the owner asked", code, b)
	}
	select {
	case m := <-push:
		if m.Data["type"] != "gas_budget_spent" || !strings.Contains(m.Body, "Top it up") {
			t.Fatalf("the owner was asked %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the owner was never asked to top up")
	}
	select {
	case m := <-push:
		t.Fatalf("the owner was asked again: %+v", m)
	case <-time.After(300 * time.Millisecond):
	}

	// Talyvor pays what the budget sponsors, so the owner cannot top it up; Talyvor's admin does, the ask is cleared and
	// the send goes through.
	if code, _ := call(http.MethodPost, "/top-up", `{"chain_id":8453,"wei":5000}`); code != http.StatusForbidden {
		t.Fatalf("the owner's top-up = %d, want 403", code)
	}
	as = admin
	if code, _ := call(http.MethodPost, "/top-up", `{"chain_id":8453,"wei":0}`); code != http.StatusBadRequest {
		t.Fatalf("a top-up of nothing = %d, want 400", code)
	}
	code, b := call(http.MethodPost, "/top-up", `{"chain_id":8453,"wei":5000}`)
	if code != http.StatusOK || b.BudgetWei != 6_000 || b.LeftWei != 5_580 || b.TopUpAsked {
		t.Fatalf("top-up = %d %+v", code, b)
	}
	var by string
	if err := pool.QueryRow(ctx, `SELECT user_id FROM gas_budget_top_ups WHERE workspace_id = $1 AND wei = 5000`, ws).Scan(&by); err != nil || by != "talyvor-ops" {
		t.Fatalf("the top-up's row names %q (%v)", by, err)
	}
	for _, h := range []string{"0xop3", "0xop2"} {
		if err := gas.Hold(ctx, h, big.NewInt(600)); err != nil {
			t.Fatalf("after the top-up, %s: %v", h, err)
		}
	}
	if code, b := call(http.MethodGet, "?chain_id=8453", ""); code != http.StatusOK || b.HeldWei != 1_200 || b.LeftWei != 4_380 {
		t.Fatalf("two sends held after the top-up = %d %+v", code, b)
	}

	// A chain no paymaster serves has no budget, and asking about it writes nothing; another workspace's is its own.
	if code, _ := call(http.MethodGet, "?chain_id=10", ""); code != http.StatusBadRequest {
		t.Fatalf("a chain Lens serves no fees on = %d, want 400", code)
	}
	if b, err := store.WorkspaceGasBudget(ctx, "ws-b485-read", base); err != nil || b.LeftWei != 1_000 {
		t.Fatalf("an untouched workspace's budget = %+v, %v", b, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM gas_budgets WHERE chain_id = 10 OR workspace_id = 'ws-b485-read'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("reading budgets wrote %d rows (%v), want none", rows, err)
	}
	if code, _ := call(http.MethodGet, "", ""); code != http.StatusBadRequest {
		t.Fatalf("no chain = %d, want 400", code)
	}
	if err := store.SponsoredGas("ws-other", base).Charge(ctx, "0xop3", big.NewInt(1)); err == nil {
		t.Fatal("another workspace charged this workspace's hold")
	}
}

type b485ClearAll struct{}

func (b485ClearAll) Screen(context.Context, string, string) error { return nil }

// B48.5 — the live path: an agent's own key pays a USDC address from its wallet at Coinbase through POST
// /v1/money/payments, and CDP's paymaster pays the network fee out of the workspace's gas budget. The send holds the
// fee's most from the budget before CDP is asked; a send the rest cannot cover is refused before CDP is asked — the
// payment fails, its USDC goes back to the agent and the owner is asked to top up; the minute's sweep charges each
// sent operation what its receipt says the paymaster paid, a released one included, which lets the next send through.
func TestB485_AnAgentsUSDCSendThroughTheAppIsRefusedWhenTheGasBudgetIsSpentAndChargedWhenItRuns(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, sepolia = "ws-b485-cdp", int64(84532)
	const quote = (3*2_000_000 + 500_000 + 100_000) * 2 * cdpfake.GasPrice // Lens's gas limits at twice the node's price
	f := cdpfake.New(t)
	reg := partners.NewRegistry(nil)
	store := economy.NewDualTokenStore(nil, pool, nil)
	screener := screening.NewScreener(pool, reg)
	store.SetAccountPartners(reg)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	store.SetGasBudgetDefault(quote + quote/2) // one send's hold, and not a second's
	push := make(b485Pushes, 4)
	store.SetNativePush(push)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, platform, public_key, attestation, push_token)
		VALUES ('dev-b485-cdp', $1, 'test', '\x00', '\x00', 'tok')`, ws); err != nil {
		t.Fatal(err)
	}
	layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, ""), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
		Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool), Switches: money.SwitchSet{
			{Capability: money.CapWallets, Partner: coinbase.Name}: true, {Capability: money.CapTransfers, Partner: coinbase.Name}: true}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := coinbase.Config{BaseURL: f.URL, APIKeyID: f.APIKeyID, APIKeySecret: f.APIKeySecret, WalletSecret: f.WalletSecret,
		Network: cdpfake.Network, PaymasterURL: f.RPCURL, RPCURL: f.RPCURL, WebhookSecret: f.WebhookSecret,
		Allowlist: store.AddressAllowlist(), Screener: b485ClearAll{}}
	if _, err := coinbase.New(cfg); err == nil {
		t.Fatal("a paymaster with no gas budget to pay it from was accepted")
	}
	cfg.Gas = store
	a, err := coinbase.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := layer.Register(a); err != nil {
		t.Fatal(err)
	}
	store.SetMoneyLayer(layer)

	agent, err := store.CreateAgent(ctx, ws, "Payer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.AgentWallet(ctx, ws, agent.ID)
	if err != nil || w.Partner != coinbase.Name {
		t.Fatalf("the agent's wallet = %+v, %v; want it at Coinbase", w, err)
	}
	var walletRef, partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT a.partner_account_ref, p.id FROM money_accounts a JOIN money_accounts p ON p.workspace_id = a.workspace_id
		AND p.partner_account_ref = a.partner_account_ref AND p.purpose = 'partner' WHERE a.id = $1`, w.AccountID).Scan(&walletRef, &partnerAccount); err != nil {
		t.Fatal(err)
	}
	f.Deposit(walletRef, "tx-in-b485", money.Amount{Minor: 100_000_000, Asset: money.USDC})
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -100_000_000}, {AccountID: w.AccountID, AmountMinor: 100_000_000}}}); err != nil {
		t.Fatal(err)
	}
	payee, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", Network: "base",
		Address: "0x00000000000000000000000000000000000ADA01"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE money_payees SET confirmed_at = now(), confirmed_by = 'passkey' WHERE id = $1`, payee.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, ws, agent.ID, "key-b485"); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountMoneyPaymentRoutes(r, store)
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	agentKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-b485"}
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	pay := func(key string) economy.OutsidePayment {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/money/payments", strings.NewReader(`{"payee_id":"`+payee.ID+
			`","amount_minor":10000000,"currency":"USDC","reference":"Invoice","idempotency_key":"`+key+`"}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req.WithContext(auth.WithAuthContext(req.Context(), agentKey)))
		var p economy.OutsidePayment
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &p) != nil {
			t.Fatalf("pay %s = %d %s", key, rec.Code, rec.Body)
		}
		return p
	}
	budget := func() economy.GasBudget {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/agents/gas-budget?chain_id=84532", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req.WithContext(auth.WithAuthContext(req.Context(), owner)))
		var b economy.GasBudget
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &b) != nil {
			t.Fatalf("GET gas-budget = %d %s", rec.Code, rec.Body)
		}
		return b
	}
	sponsorship := func(hash string) (ref string, held int64, charged *int64, released bool) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT op_ref, held_wei, charged_wei, released_at IS NOT NULL FROM gas_sponsorships
			WHERE workspace_id = $1 AND chain_id = $2 AND user_op_hash = $3`, ws, sepolia, hash).Scan(&ref, &held, &charged, &released); err != nil {
			t.Fatalf("the sponsorship of %s: %v", hash, err)
		}
		return ref, held, charged, released
	}

	// The first send holds its quoted fee and is sent by CDP.
	first := pay("usdc-1")
	if first.Status != economy.PaymentSent || first.Partner != coinbase.Name || first.PartnerRef == "" || f.Sent() != 1 {
		t.Fatalf("the first send = %+v with CDP asked %d times; want sent", first, f.Sent())
	}
	if _, held, charged, _ := sponsorship(first.PartnerRef); held != quote || charged != nil {
		t.Fatalf("the first send holds %d wei and is charged %v, want %d held", held, charged, quote)
	}
	if b := budget(); b.HeldWei != quote || b.LeftWei != quote/2 {
		t.Fatalf("while the first is on its way the budget is %+v", b)
	}

	// The second cannot be covered: CDP is never asked, the payment fails and its USDC is back, the owner is asked.
	second := pay("usdc-2")
	if second.Status != economy.PaymentFailed || !strings.Contains(second.Detail, "budget for network fees is used up") || f.Sent() != 1 {
		t.Fatalf("a send the gas budget cannot cover = %+v with CDP asked %d times; want failed before CDP is asked", second, f.Sent())
	}
	if got, want := b468Postings(t, pool, ws, "usdc-2"), "payment_out_hold agent -10000000; payment_out_hold hold 10000000; payment_out_failed hold -10000000; payment_out_failed agent 10000000"; got != want {
		t.Fatalf("the refused send posted %q, want %q", got, want)
	}
	// The owner is also told the payment failed; the two pushes arrive in either order.
	for asked, deadline := false, time.After(5*time.Second); !asked; {
		select {
		case m := <-push:
			switch m.Data["type"] {
			case "gas_budget_spent":
				if asked = m.Data["chain_id"] == "84532"; !asked {
					t.Fatalf("the owner was asked to top up %+v", m)
				}
			case "payment_failed":
			default:
				t.Fatalf("the owner was told %+v", m)
			}
		case <-deadline:
			t.Fatal("the owner was never asked to top the gas budget up")
		}
	}

	// The minute's sweep charges the first what its receipt says the paymaster paid, and frees the rest of its hold.
	if n, err := store.SettleSponsoredGas(ctx); err != nil || n != 1 {
		t.Fatalf("the sweep charged %d (%v), want 1", n, err)
	}
	if _, _, charged, _ := sponsorship(first.PartnerRef); charged == nil || *charged != cdpfake.GasCharged {
		t.Fatalf("the first send is charged %v, want the %d wei the paymaster paid", charged, cdpfake.GasCharged)
	}
	if b := budget(); b.ChargedWei != cdpfake.GasCharged || b.HeldWei != 0 || b.LeftWei != quote+quote/2-cdpfake.GasCharged {
		t.Fatalf("after the sweep the budget is %+v", b)
	}

	// So the third goes through. Released after it was sent (as when the bundler answers with an error), it is still
	// charged from its receipt.
	third := pay("usdc-3")
	if third.Status != economy.PaymentSent || f.Sent() != 2 {
		t.Fatalf("the send after the charge = %+v with CDP asked %d times; want sent", third, f.Sent())
	}
	ref, _, _, _ := sponsorship(third.PartnerRef)
	if err := store.SponsoredGas(ws, sepolia).Release(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if n, err := store.SettleSponsoredGas(ctx); err != nil || n != 1 {
		t.Fatalf("the sweep charged %d (%v), want the released one", n, err)
	}
	if _, _, charged, released := sponsorship(third.PartnerRef); charged == nil || *charged != cdpfake.GasCharged || released {
		t.Fatalf("the released send is charged %v (released %v), want %d", charged, released, cdpfake.GasCharged)
	}
	if n, err := store.SettleSponsoredGas(ctx); err != nil || n != 0 {
		t.Fatalf("a second sweep charged %d (%v), want nothing", n, err)
	}
}
