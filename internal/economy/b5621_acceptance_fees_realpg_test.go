package economy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/partners"
)

// b5621Issuer is a workspace whose agent issues invoices from its GBP account, and another company's agent with
// £100.00 to pay them.
func b5621Issuer(t *testing.T, ctx context.Context, s *DualTokenStore, ws, payerWS string) (seller, buyer Agent, company, sellerGBP CurrencyAccount) {
	t.Helper()
	var err error
	if seller, err = s.CreateAgent(ctx, ws, "Seller", "owner"); err != nil {
		t.Fatal(err)
	}
	if company, err = s.OpenCurrencyAccount(ctx, ws, "", "GBP", "test"); err != nil {
		t.Fatal(err)
	}
	if sellerGBP, err = s.OpenCurrencyAccount(ctx, ws, seller.ID, "GBP", "test"); err != nil {
		t.Fatal(err)
	}
	if payerWS == "" {
		return
	}
	if buyer, err = s.CreateAgent(ctx, payerWS, "Buyer", "owner"); err != nil {
		t.Fatal(err)
	}
	payerCompany, err := s.OpenCurrencyAccount(ctx, payerWS, "", "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	buyerGBP, err := s.OpenCurrencyAccount(ctx, payerWS, buyer.ID, "GBP", "test")
	if err != nil {
		t.Fatal(err)
	}
	details, err := s.CurrencyAccountDetails(ctx, payerWS, "", buyerGBP.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReceivePayment(ctx, InboundPayment{PartnerAccountRef: payerCompany.PartnerAccountRef, PartnerRef: "test_in_b5621_fund",
		Payer: "Buyer Ltd", Reference: details.PaymentReference, AmountMinor: 10000, Currency: "GBP"}); err != nil {
		t.Fatal(err)
	}
	return
}

func b5621Invoice(t *testing.T, ctx context.Context, s *DualTokenStore, ws, accountID string, minor int64) Invoice {
	t.Helper()
	inv, err := s.CreateInvoice(ctx, ws, "", InvoiceRequest{AccountID: accountID, CustomerName: "Buyer Ltd",
		Lines: []InvoiceLine{{Description: "Work", Quantity: 1, UnitAmountMinor: minor}}, DueDate: time.Now().UTC().AddDate(0, 0, 14).Format(time.DateOnly), Send: true})
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// b5621Fees is each fee entry of kind on the seller's account: its key, and what it moved there.
func b5621Fees(t *testing.T, ctx context.Context, s *DualTokenStore, kind, accountID string) map[string]int64 {
	t.Helper()
	rows, err := s.pool.Query(ctx, `SELECT e.idempotency_key, p.amount_minor FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
		WHERE e.kind = $1 AND p.account_id = $2`, kind, accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var key string
		var minor int64
		if err := rows.Scan(&key, &minor); err != nil {
			t.Fatal(err)
		}
		out[key] += minor
	}
	return out
}

// B56.21 — Talyvor keeps its fee for accepting each payment of an invoice, once however often the payment is sent, as
// its own entry from the issuer's account to Talyvor's revenue: 1% of an agent's payment or a transfer, and 0.75% of a
// card payment plus what Stripe kept for processing it. The invoice shows each fee. A card payment refunded half and
// then in full gives the fee back in the same shares.
func TestB5621_EachWayOfPayingAnInvoiceBooksItsFeeOnceAndARefundGivesItBack(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := screenedStore(pool)
	s.SetAccountPartners(partners.NewRegistry(nil))
	const ws, payerWS = "ws-b5621-issuer", "ws-b5621-payer"
	_, buyer, company, sellerGBP := b5621Issuer(t, ctx, s, ws, payerWS)

	byCard := b5621Invoice(t, ctx, s, ws, sellerGBP.ID, 12000)
	for range 2 { // Stripe sends it twice
		if _, err := s.PayInvoiceByCard(ctx, InvoiceCardPayment{InvoiceID: byCard.ID, SessionID: "cs_b5621", Payer: "Buyer Ltd",
			AmountMinor: 12000, Currency: "GBP", PaymentIntent: "pi_b5621", ProcessorFeeMinor: 200}); err != nil {
			t.Fatal(err)
		}
	}
	byAgent := b5621Invoice(t, ctx, s, ws, sellerGBP.ID, 3000)
	for range 2 { // a retry
		if _, err := s.PayInvoiceByAgent(ctx, byAgent.PayToken, payerWS, buyer.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	byTransfer := b5621Invoice(t, ctx, s, ws, sellerGBP.ID, 2500)
	details, err := s.CurrencyAccountDetails(ctx, ws, "", sellerGBP.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReceivePayment(ctx, InboundPayment{PartnerAccountRef: company.PartnerAccountRef, PartnerRef: "test_in_b5621_transfer",
		Payer: "Buyer Ltd", Reference: details.PaymentReference + " " + byTransfer.Reference, AmountMinor: 2500, Currency: "GBP"}); err != nil {
		t.Fatal(err)
	}
	for range 2 { // two sweeps
		if _, err := s.SweepInvoices(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	// 0.75% of £120.00 is 90p, and Stripe's £2.00; 1% of £30.00 is 30p; 1% of £25.00 is 25p.
	want := map[string]int64{byCard.ID: -290, byAgent.ID: -30, byTransfer.ID: -25}
	fees := b5621Fees(t, ctx, s, "acceptance_fee", sellerGBP.ID)
	if len(fees) != 3 {
		t.Fatalf("the seller's account has the fee entries %v; want one for each of the three payments", fees)
	}
	for id, fee := range want {
		inv, err := s.readInvoice(ctx, pool, `i.id = $1`, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(inv.Payments) != 1 || inv.Status != InvoicePaid || fees["acceptance_fee:"+inv.Payments[0].EntryID] != fee || inv.Payments[0].FeeMinor != -fee {
			t.Errorf("%s (%s) reads %s with payments %+v, and its fee entry moved %d; want paid, one payment showing a fee of %d",
				inv.Number, inv.Lines[0].Description, inv.Status, inv.Payments, fees["acceptance_fee:"+inv.Payments[0].EntryID], -fee)
		}
	}
	var revenue int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(b.balance_minor), 0)::bigint FROM money_account_balances b JOIN money_accounts a ON a.id = b.account_id
		WHERE a.workspace_id = $1 AND a.purpose = 'revenue' AND a.currency = 'GBP' AND b.funding = 'test'`, ws).Scan(&revenue); err != nil {
		t.Fatal(err)
	}
	if got := moneyBalance(t, s, ws, sellerGBP.ID).TestMinor; got != 17500-345 || revenue != 345 {
		t.Fatalf("after the three payments the seller holds %d and Talyvor's revenue %d; want 17155 and 345", got, revenue)
	}

	// Refunded half, the same again, then in full.
	for _, refunded := range []int64{6000, 6000, 12000} {
		if _, err := s.ReturnInvoiceCardPayment(ctx, "pi_b5621", refunded); err != nil {
			t.Fatal(err)
		}
	}
	returned := b5621Fees(t, ctx, s, "invoice_card_returned", sellerGBP.ID)
	back := b5621Fees(t, ctx, s, "acceptance_fee_returned", sellerGBP.ID)
	if len(returned) != 2 || len(back) != 2 {
		t.Fatalf("the refunds booked returns %v and fees given back %v; want two of each", returned, back)
	}
	for key, minor := range returned {
		if minor != -6000 || back["acceptance_fee_returned:"+strings.TrimPrefix(key, "invoice_card_returned:")] != 145 {
			t.Errorf("refund %s returned %d with a fee back of %v; want 6000 and 145", key, -minor, back)
		}
	}
	if got := moneyBalance(t, s, ws, sellerGBP.ID).TestMinor; got != 17155-12000+290 {
		t.Fatalf("after the full refund the seller holds %d; want %d, the card payment gone and its fee back", got, 17155-12000+290)
	}
	if _, err := s.ReturnInvoiceCardPayment(ctx, "pi_b5621_unknown", 100); !errors.Is(err, ErrInvoiceNotFound) {
		t.Fatalf("a refund of a payment intent no invoice was paid by = %v; want ErrInvoiceNotFound", err)
	}
}

// B56.21 — live money paid on an invoice needs merchant_acceptance as well as the payment's own capability: with
// payments_in cleared and merchant_acceptance not, a live card payment is refused and writes nothing, so it stays in
// Sandbox; once merchant_acceptance is cleared it posts, with its fee, as live money.
func TestB5621_ALivePaymentStaysInSandboxUntilAcceptingPaymentsIsCleared(t *testing.T) {
	pool := supplyPool(t)
	gb := WithUseCountry(context.Background(), "GB")
	s := screenedStore(pool)
	s.SetAccountPartners(partners.NewRegistry(nil))
	const ws = "ws-b5621-live"
	if _, err := pool.Exec(gb, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	planGatesOnPlan(t, pool, ws, "team", false)
	liveVerified(t, pool, ws)
	_, _, _, sellerGBP := b5621Issuer(t, gb, s, ws, "")
	clear := func(key string) {
		t.Helper()
		if _, err := s.ClearCapability(gb, key, "nicolai", ClearanceTerms{Reference: "B56.21 test", Licence: "EMI-900123",
			Partner: "Test Payments Ltd", Countries: []string{"GB"}, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	clear(CapabilityPaymentsIn)
	inv := b5621Invoice(t, gb, s, ws, sellerGBP.ID, 10000)
	pay := func() error {
		_, err := s.PayInvoiceByCard(gb, InvoiceCardPayment{InvoiceID: inv.ID, SessionID: "cs_b5621_live", Payer: "Buyer Ltd",
			AmountMinor: 10000, Currency: "GBP", Funding: FundingLive, PaymentIntent: "pi_b5621_live", ProcessorFeeMinor: 170})
		return err
	}

	var refusal *CapabilityRefusal
	if err := pay(); !errors.As(err, &refusal) || refusal.Capability.Key != CapabilityMerchantAcceptance {
		t.Fatalf("a live card payment with merchant_acceptance uncleared = %v; want refused by merchant_acceptance", err)
	}
	var entries int
	if err := pool.QueryRow(gb, `SELECT count(*) FROM money_entries WHERE workspace_id = $1 AND idempotency_key LIKE '%cs_b5621_live%'`, ws).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if b := moneyBalance(t, s, ws, sellerGBP.ID); entries != 0 || b.LiveMinor != 0 {
		t.Fatalf("the refused live payment wrote %d entries and left the seller %d of live money; want none", entries, b.LiveMinor)
	}

	clear(CapabilityMerchantAcceptance)
	if err := pay(); err != nil {
		t.Fatalf("the live card payment once both are cleared = %v", err)
	}
	fees := b5621Fees(t, gb, s, "acceptance_fee", sellerGBP.ID)
	if b := moneyBalance(t, s, ws, sellerGBP.ID); len(fees) != 1 || b.LiveMinor != 10000-75-170 || b.TestMinor != 0 {
		t.Fatalf("the live payment left fees %v and the seller %+v; want one fee of 245 and 9755 of live money", fees, b)
	}
}
