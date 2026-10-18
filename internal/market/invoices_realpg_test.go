package market

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/taxprofile"
)

// B28.140 / B17.189 — the testers' market-bill-invoices: a GB buyer's bill paid twice, a third use left unpaid and the
// first invoice refunded lists the period in progress, then B, then A, each period starting where the last ended and
// each paid one linking its PDF; each invoice's bill holds exactly its own use, A's refunded, and totals the invoice
// less its refunds — B's what its receipt collected.
func TestMarketInvoices_TwoPaidBillsAndOneUnpaidUseReadByInvoice(t *testing.T) {
	ctx := context.Background()
	pool := migratedDB(t)
	const seller, gb = "ws-inv-seller", "ws-inv-gb"
	m := newTaxedMarket(t, pool, true, []string{seller, gb}, "GB")
	if _, err := m.profiles.Put(ctx, gb, taxprofile.Input{LegalName: "Ana Smith", Country: "GB"}); err != nil {
		t.Fatal(err)
	}
	m.store.SetReceipts(Receipts{Supplier: Supplier{LegalName: "Talyvor Ltd"}, Buyers: m.profiles, AppURL: "https://app.example"})
	deps := LicenceDeps{Meter: m.bill("price_market_tax"), Capabilities: &askedCapabilities{}}
	period := 30
	rent := func(i int) string {
		t.Helper()
		l, err := m.store.Publish(ctx, seller, Draft{Kind: "prompt", Title: fmt.Sprintf("Adder %d", i), Visibility: "public",
			Artifact: json.RawMessage(adder), Offers: []Offer{{Kind: OfferRent, Licence: LicenceCommercial, PriceUSDMicros: 10_000_000, PeriodDays: &period}}})
		if err != nil {
			t.Fatal(err)
		}
		lic, _, err := m.store.License(ctx, deps, gb, "", l.ID, fmt.Sprintf("rent-%d", i), LicenceRequest{OfferID: l.Offers[0].ID})
		if err != nil || lic.Charge != ChargeBilled {
			t.Fatalf("rental %d = %+v, %v", i, lic, err)
		}
		return lic.UseID
	}
	pay := func() string {
		t.Helper()
		inv, n, err := m.store.PayTestBill(ctx, gb, time.Now())
		if err != nil || n != 1 {
			t.Fatalf("paying the bill = %d uses, %v; want one", n, err)
		}
		return inv
	}
	useA := rent(1)
	a := pay()
	useB := rent(2)
	b := pay()
	useC := rent(3)
	if n, err := m.store.RefundTestBill(ctx, gb, a); err != nil || n != 1 {
		t.Fatalf("refunding A = %d, %v", n, err)
	}

	list, err := m.store.Invoices(ctx, gb)
	if err != nil || len(list) != 3 || list[0].ID != UpcomingInvoice || list[1].ID != b || list[2].ID != a {
		t.Fatalf("invoices = %+v, %v; want upcoming, %s, %s", list, err, b, a)
	}
	up, invB, invA := list[0], list[1], list[2]
	const gross = 12_000_000 // $10.00 and GB's 20% tax
	if invA.Status != "paid" || invA.GrossUSDMicros != gross || invA.RefundedUSDMicros != gross ||
		invB.Status != "paid" || invB.GrossUSDMicros != gross || invB.RefundedUSDMicros != 0 || up.Status != UpcomingInvoice {
		t.Fatalf("A %+v, B %+v, upcoming %+v; want A charged and refunded %d, B charged it", invA, invB, up, gross)
	}
	if invB.PeriodStart.Before(invA.PeriodEnd) || up.PeriodStart.Before(invB.PeriodEnd) || !invA.PeriodStart.Before(invA.PeriodEnd) {
		t.Fatalf("periods A %v–%v, B %v–%v, upcoming %v–%v; each should start where the last ended", invA.PeriodStart, invA.PeriodEnd,
			invB.PeriodStart, invB.PeriodEnd, up.PeriodStart, up.PeriodEnd)
	}
	receipts, err := m.store.ReceiptsOf(ctx, gb)
	if err != nil || len(receipts) != 2 {
		t.Fatalf("receipts = %+v, %v", receipts, err)
	}
	var receiptB ReceiptSummary
	for _, r := range receipts {
		if r.InvoiceID == b {
			receiptB = r
		}
	}
	if invB.InvoicePDF != "https://app.example/api/marketplace/receipts/"+receiptB.ID+"?format=pdf" {
		t.Fatalf("B's PDF = %q; want its receipt's, %s", invB.InvoicePDF, receiptB.ID)
	}

	for _, c := range []struct {
		inv, use string
		refunded bool
		gross    int64
	}{{a, useA, true, 0}, {b, useB, false, receiptB.GrossUSDMicros}, {UpcomingInvoice, useC, false, gross}} {
		bill, err := m.store.InvoiceBill(ctx, gb, c.inv)
		if err != nil || bill.Invoice == nil || bill.Invoice.ID != c.inv || len(bill.Lines) != 1 || bill.Lines[0].UseID != c.use ||
			(bill.Lines[0].Refunded != nil) != c.refunded || bill.GrossUSDMicros != c.gross ||
			bill.GrossUSDMicros != bill.Invoice.GrossUSDMicros-bill.Invoice.RefundedUSDMicros {
			t.Fatalf("the bill for %s = %+v, %v; want only %s (refunded %v), gross %d", c.inv, bill, err, c.use, c.refunded, c.gross)
		}
	}
	if _, err := m.store.InvoiceBill(ctx, seller, b); err != ErrNoInvoice {
		t.Fatalf("another workspace reading B's bill = %v; want ErrNoInvoice", err)
	}
}
