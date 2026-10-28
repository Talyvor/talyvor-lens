package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/circle"
	"github.com/talyvor/lens/internal/money/circle/circlefake"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B46.18 — through the fake Circle, switched on for agents' wallets: an agent pays a saved USDC address on Base the
// owner approved from its own wallet once however often it is asked, the money held until Circle's notification says
// the transfer completed, and settled once — the same notification again is refused, and another about the same
// transfer changes nothing. An address the owner has not approved is refused before anything is held or sent, and one
// Circle refuses releases its hold.
func TestB4618_AnAgentPaysAnApprovedUSDCAddressThroughCircleOnceAndItsNotificationSettlesItOnce(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b4618-circle"
	f := circlefake.New(t)
	reg := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, reg)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(reg)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, ""), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
		Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool), Switches: money.SwitchSet{
			{Capability: money.CapWallets, Partner: circle.Name}: true, {Capability: money.CapTransfers, Partner: circle.Name}: true}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := circle.New(circle.Config{BaseURL: f.URL, APIKey: f.APIKey, EntitySecret: f.EntitySecret, WalletSetID: f.WalletSetID,
		Blockchain: circlefake.Blockchain, Allowlist: store.AddressAllowlist()})
	if err != nil {
		t.Fatal(err)
	}
	if err := layer.Register(a); err != nil {
		t.Fatal(err)
	}
	store.SetMoneyLayer(layer)

	// The agent's wallet is at Circle and holds 500 USDC there, as the ledger does.
	agent, err := store.CreateAgent(ctx, ws, "Payer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.AgentWallet(ctx, ws, agent.ID)
	if err != nil || w.Partner != circle.Name {
		t.Fatalf("the agent's wallet = %+v, %v; want it at Circle", w, err)
	}
	var walletRef, partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT a.partner_account_ref, p.id FROM money_accounts a JOIN money_accounts p ON p.workspace_id = a.workspace_id
		AND p.partner_account_ref = a.partner_account_ref AND p.purpose = 'partner' WHERE a.id = $1`, w.AccountID).Scan(&walletRef, &partnerAccount); err != nil {
		t.Fatal(err)
	}
	f.Deposit(walletRef, "tx-in-b4618", money.Amount{Minor: 500_000_000, Asset: money.USDC})
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -500_000_000}, {AccountID: w.AccountID, AmountMinor: 500_000_000}}}); err != nil {
		t.Fatal(err)
	}
	balance := func(want int64) {
		t.Helper()
		if got, err := store.Balance(ctx, ws, w.AccountID); err != nil || got.TestMinor != want {
			t.Fatalf("the agent's wallet holds %+v (%v), want %d", got, err, want)
		}
	}

	if _, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Eve", Country: "GB", Address: "0x1234"}); !errors.Is(err, economy.ErrPayee) {
		t.Fatalf("a payee at 0x1234 = %v, want refused: that is no address on Base", err)
	}
	save := func(name, address string, approve bool) economy.OutsidePayee {
		t.Helper()
		p, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: name, Country: "GB", Network: "base", Address: address})
		if err != nil || !p.NeedsConfirmation || p.Check != economy.PayeeAddress {
			t.Fatalf("save %s = %+v, %v; want saved, waiting for the owner to approve the address", name, p, err)
		}
		if approve { // the owner approves it with a passkey (B30.16's confirmation, tested there)
			if _, err := pool.Exec(ctx, `UPDATE money_payees SET confirmed_at = now(), confirmed_by = 'passkey' WHERE id = $1`, p.ID); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	ada := save("Ada Lovelace", "0x00000000000000000000000000000000000ADA01", true)
	stranger := save("Mallory", "0x00000000000000000000000000000000000BAD01", false)
	pay := func(payee economy.OutsidePayee, key string, minor int64) (economy.OutsidePayment, error) {
		return store.PayOutside(ctx, ws, economy.OutsidePayment{AgentID: agent.ID, PayeeID: payee.ID, Currency: money.USDC, AmountMinor: minor,
			Reference: "Invoice " + key, IdempotencyKey: key})
	}
	r := chi.NewRouter()
	r.Post("/v1/money/webhooks/{partner}", layer.ServeWebhook)
	notify := func(id, txID string) int {
		t.Helper()
		body, h := f.Notify(id, "transactions.outbound", txID)
		req := httptest.NewRequest(http.MethodPost, "/v1/money/webhooks/circle", bytes.NewReader(body))
		req.Header = h
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	p, err := pay(ada, "usdc", 25_000_000)
	if err != nil || p.Status != economy.PaymentSent || p.Partner != circle.Name || p.PartnerRef == "" {
		t.Fatalf("pay 25 USDC = %+v, %v; want sent to Circle, held until it completes", p, err)
	}
	if again, err := pay(ada, "usdc", 25_000_000); err != nil || again.ID != p.ID || f.Outbound() != 1 {
		t.Fatalf("pay it again = %+v, %v with Circle asked %d times; want the same payment, sent once", again, err, f.Outbound())
	}
	f.Complete(p.PartnerRef)
	for i, c := range []struct {
		id   string
		want int
	}{{"b4618000-0000-4000-8000-00000000done", http.StatusOK}, {"b4618000-0000-4000-8000-00000000done", http.StatusConflict},
		{"b4618000-0000-4000-8000-0000000again", http.StatusOK}} {
		if got := notify(c.id, p.PartnerRef); got != c.want {
			t.Fatalf("Circle's notification %d = %d, want %d", i+1, got, c.want)
		}
	}
	if got, want := b468Postings(t, pool, ws, "usdc"), "payment_out_hold agent -25000000; payment_out_hold hold 25000000; payment_out hold -25000000; payment_out partner 25000000"; got != want {
		t.Errorf("25 USDC posted %q, want the hold and the payment out once", got)
	}
	if read, err := store.RefreshOutsidePayment(ctx, ws, "", p.ID); err != nil || read.Status != economy.PaymentCompleted {
		t.Fatalf("after Circle completed it = %+v, %v; want completed", read, err)
	}
	balance(475_000_000)

	if _, err := pay(stranger, "usdc-stranger", 10_000_000); !errors.Is(err, economy.ErrPayeeUnconfirmed) {
		t.Fatalf("pay an address the owner has not approved = %v, want refused", err)
	}
	if got := b468Postings(t, pool, ws, "usdc-stranger"); got != "" || f.Outbound() != 1 {
		t.Fatalf("the refused payment posted %q with Circle asked %d times; want nothing held and nothing sent", got, f.Outbound())
	}
	balance(475_000_000)

	f.Screening(ada.Address, "DENIED", "SANCTIONS")
	p, err = pay(ada, "usdc-denied", 10_000_000)
	if err != nil || p.Status != economy.PaymentFailed {
		t.Fatalf("pay 10 USDC to an address Circle's screening denies = %+v, %v; want failed", p, err)
	}
	if got, want := b468Postings(t, pool, ws, "usdc-denied"), "payment_out_hold agent -10000000; payment_out_hold hold 10000000; payment_out_failed hold -10000000; payment_out_failed agent 10000000"; got != want {
		t.Errorf("10 USDC refused by Circle posted %q, want %q", got, want)
	}
	balance(475_000_000)
}
