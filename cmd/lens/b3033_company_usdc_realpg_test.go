package main

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B30.33 — a company opens a USDC account at the stablecoin partner (the Test partner) and pays saved USDC addresses
// the owner approved from it: an approved address is paid and posts, one the owner has not approved is refused, and a
// sanctioned one is refused and opens a case — neither of those moves a micro-USDC.
func TestB3033_ACompanyPaysAnApprovedUSDCAddressFromItsOwnAccountAndNeverAnUnlistedOrSanctionedOne(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b3033"
	const sanctioned = "0x00000000000000000000000000000000000bad02"
	reg := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, b487Screening{sanctioned: partners.AddressSanctioned})
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(reg)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)

	acct, err := store.OpenCurrencyAccount(ctx, ws, "", "USDC", "")
	if err != nil || acct.Currency != "USDC" || acct.Purpose != economy.MoneyCompany || acct.PartnerAccountRef != "" || acct.Address != "" {
		t.Fatalf("open the company's USDC account = %+v, %v; want it open, its made-up test address shown nowhere", acct, err)
	}
	var address, partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT a.partner_account_ref, p.id FROM money_accounts a JOIN money_accounts p ON p.workspace_id = a.workspace_id
		AND p.partner_account_ref = a.partner_account_ref AND p.purpose = 'partner' WHERE a.id = $1`, acct.ID).Scan(&address, &partnerAccount); err != nil {
		t.Fatalf("the account's receiving address at the partner: %v", err)
	}
	if len(address) != 42 || address[:2] != "0x" {
		t.Fatalf("the account is kept under %q, want the partner's receiving address", address)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -100_000_000}, {AccountID: acct.ID, AmountMinor: 100_000_000}}}); err != nil {
		t.Fatal(err)
	}

	save := func(name, address string, approve bool) economy.OutsidePayee {
		t.Helper()
		p, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: name, Country: "GB", Network: "base", Address: address})
		if err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
		if approve { // the owner approves it with a passkey (B30.16's confirmation, tested there)
			if _, err := pool.Exec(ctx, `UPDATE money_payees SET confirmed_at = now(), confirmed_by = 'passkey' WHERE id = $1`, p.ID); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	pay := func(payee economy.OutsidePayee, key string) (economy.OutsidePayment, error) {
		return store.PayOutside(ctx, ws, economy.OutsidePayment{PayeeID: payee.ID, Currency: "USDC", AmountMinor: 25_000_000,
			Reference: "Invoice " + key, IdempotencyKey: key})
	}

	p, err := pay(save("Ada Lovelace", "0x00000000000000000000000000000000000ada01", true), "listed")
	if err != nil || p.Status != economy.PaymentCompleted || p.Partner != "test" || p.PartnerRef == "" {
		t.Fatalf("pay an approved address = %+v, %v; want completed at the stablecoin partner", p, err)
	}
	if got, want := b468Postings(t, pool, ws, "listed"), "payment_out_hold company -25000000; payment_out_hold hold 25000000; payment_out hold -25000000; payment_out partner 25000000"; got != want {
		t.Errorf("the approved payment posted %q, want %q", got, want)
	}

	if _, err := pay(save("Mallory", "0x00000000000000000000000000000000000bad01", false), "unlisted"); !errors.Is(err, economy.ErrPayeeUnconfirmed) {
		t.Fatalf("pay an address the owner has not approved = %v, want refused", err)
	}
	if got := b468Postings(t, pool, ws, "unlisted"); got != "" {
		t.Errorf("the unlisted payment posted %q, want nothing", got)
	}

	if _, err := pay(save("Eve", sanctioned, true), "sanctioned"); err == nil {
		t.Fatal("paid a sanctioned address, want refused")
	}
	if got := b468Postings(t, pool, ws, "sanctioned"); got != "" {
		t.Errorf("the sanctioned payment posted %q, want nothing", got)
	}
	var cases int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM compliance_cases WHERE workspace_id = $1 AND name = $2`, ws, sanctioned).Scan(&cases); err != nil || cases != 1 {
		t.Fatalf("cases for the sanctioned address = %d, %v; want one", cases, err)
	}
	if got, err := store.Balance(ctx, ws, acct.ID); err != nil || got.TestMinor != 75_000_000 {
		t.Fatalf("the account holds %+v (%v), want 75 USDC: only the approved payment left it", got, err)
	}
}
