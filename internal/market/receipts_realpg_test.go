package market

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v81/webhook"

	"github.com/talyvor/lens/internal/ecbrate"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/taxprofile"
)

// B32.40 — a GB consumer's paid two-line invoice gets receipt 1 of the year, whose net, VAT and gross equal its tax
// lines and Stripe's invoice total to the cent, with the VAT also in pounds at the ECB rate; until Talyvor's VAT number
// is set it reads "VAT registration pending" and is a Preview, and a replayed invoice.paid issues nothing more. The
// next paid invoice — a DE business's, reverse charged — gets receipt 2, with the note and no VAT.
func TestMarketReceipts_APaidBillGetsTheNextReceiptOfTheYear(t *testing.T) {
	ctx := context.Background()
	pool := migratedDB(t)
	const seller, gb, de = "ws-rcpt-seller", "ws-rcpt-gb", "ws-rcpt-de"
	m := newTaxedMarket(t, pool, false, []string{seller, gb, de}, "GB", "EU")
	supplier := Supplier{LegalName: "TALYVOR LTD", Address: "71-75 Shelton Street, London, WC2H 9JQ"}
	m.store.SetReceipts(Receipts{Supplier: supplier, Buyers: m.profiles, Rates: ecbrate.New(pool, "")})
	rateDay := time.Now().UTC().AddDate(0, 0, -1).Truncate(24 * time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO ecb_reference_rates (rate_date, currency, per_eur) VALUES ($1, 'USD', 1.1000), ($1, 'GBP', 0.8500)`,
		rateDay); err != nil {
		t.Fatal(err)
	}
	if _, err := m.profiles.Put(ctx, gb, taxprofile.Input{LegalName: "Ana Smith", Address: "1 High Street, London", PostalCode: "N1 1AA",
		Country: "GB"}); err != nil {
		t.Fatal(err)
	}
	if p, err := m.profiles.Put(ctx, de, taxprofile.Input{LegalName: "Muster GmbH", Address: "Hauptstraße 1, Berlin", Country: "DE",
		TaxID: "DE123456789"}); err != nil || !p.TaxIDValid {
		t.Fatalf("DE profile = %+v, %v; want its VAT number valid", p, err)
	}
	period := 30
	publish := func(title string, price int64) Listing {
		t.Helper()
		l, err := m.store.Publish(ctx, seller, Draft{Kind: "prompt", Title: title, Visibility: "public", Artifact: json.RawMessage(adder),
			Offers: []Offer{{Kind: OfferRent, Licence: LicenceCommercial, PriceUSDMicros: price, PeriodDays: &period}}})
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	adderListing, summariser := publish("Adder", 10_000_000), publish("Summariser", 5_000_000)
	bill := m.bill("price_market_tax")
	deps := LicenceDeps{Meter: bill, Capabilities: &askedCapabilities{}}
	rent := func(ws string, l Listing) string {
		t.Helper()
		lic, _, err := m.store.License(ctx, deps, ws, "", l.ID, "rent-"+ws+"-"+l.ID, LicenceRequest{OfferID: l.Offers[0].ID})
		if err != nil || lic.Charge != ChargeBilled || lic.MeterError != "" {
			t.Fatalf("%s rents %s = %+v, %v", ws, l.Title, lic, err)
		}
		return lic.UseID
	}
	// paid delivers Stripe's invoice.paid for ws's marketplace bill, its total totalCents, as Stripe signs it.
	paid := func(ws, invoiceID string, totalCents int64) {
		t.Helper()
		now := time.Now()
		obj := map[string]any{"id": invoiceID, "object": "invoice", "subscription": "sub_market_" + ws, "total": totalCents,
			"status_transitions": map[string]any{"paid_at": now.Unix()},
			"lines": map[string]any{"object": "list", "data": []any{map[string]any{"id": "il_" + invoiceID, "object": "line_item",
				"period": map[string]any{"start": now.Add(-24 * time.Hour).Unix(), "end": now.Add(time.Hour).Unix()},
				"price":  map[string]any{"id": "price_market", "object": "price"}}}}}
		raw, _ := json.Marshal(map[string]any{"id": "evt_" + invoiceID, "object": "event", "type": "invoice.paid", "livemode": true,
			"created": now.Unix(), "data": map[string]any{"object": obj}})
		req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", bytes.NewReader(raw))
		req.Header.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%s", now.Unix(), hex.EncodeToString(webhook.ComputeSignature(now, raw, "whsec_market_tax"))))
		w := httptest.NewRecorder()
		bill.HandleWebhook(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("invoice.paid %s = %d %s", invoiceID, w.Code, w.Body.String())
		}
	}
	only := func(ws string) Receipt {
		t.Helper()
		list, err := m.store.ReceiptsOf(ctx, ws)
		if err != nil || len(list) != 1 {
			t.Fatalf("%s's receipts = %+v, %v; want one", ws, list, err)
		}
		r, err := m.store.Receipt(ctx, ws, list[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	uses := []string{rent(gb, adderListing), rent(gb, summariser)}
	var taxLines []ReceiptLine
	for _, u := range uses {
		line, err := m.store.TaxLineOf(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		taxLines = append(taxLines, ReceiptLine{UseID: u, NetUSDMicros: line.TaxableUSDMicros, RateBps: line.RateBps, TaxUSDMicros: line.TaxUSDMicros,
			Treatment: line.Treatment, Jurisdiction: line.Jurisdiction, Note: line.Note})
	}
	// Stripe's invoice: the two rents' price line and their tax line, each rounded to the cent — $15.00 + $3.00.
	for range 2 {
		paid(gb, "in_rcpt_gb", 1_800)
	}
	r := only(gb)
	year := time.Now().UTC().Year()
	if r.Number != fmt.Sprintf("%d-000001", year) || r.Sequence != 1 || r.Series != "live" || r.InvoiceID != "in_rcpt_gb" {
		t.Fatalf("the GB receipt is %s (sequence %d, %s, invoice %s); want %d-000001, the year's first live receipt", r.Number, r.Sequence,
			r.Series, r.InvoiceID, year)
	}
	got := make([]ReceiptLine, len(r.Lines))
	for i, l := range r.Lines {
		got[i] = l
		got[i].Description = ""
	}
	if fmt.Sprint(got) != fmt.Sprint(taxLines) || r.Lines[0].Description != "Adder — rental" {
		t.Fatalf("the receipt's lines = %+v; want its tax lines %+v, the first described as \"Adder — rental\"", r.Lines, taxLines)
	}
	if r.NetUSDMicros != 15_000_000 || r.TaxUSDMicros != 3_000_000 || r.GrossUSDMicros != 18_000_000 || r.GrossCents != 1_800 ||
		r.MatchesStripe == nil || !*r.MatchesStripe {
		t.Fatalf("the GB receipt's net %d, VAT %d, gross %d (%d¢, Stripe %v); want 15,000,000, 3,000,000 and 18,000,000, Stripe's 1800¢",
			r.NetUSDMicros, r.TaxUSDMicros, r.GrossUSDMicros, r.GrossCents, r.StripeTotal)
	}
	// $3.00 of VAT at 0.85 GBP and 1.10 USD to the euro: £2.318…, so £2.32.
	if r.TaxLocal == nil || r.TaxLocal.Currency != "GBP" || r.TaxLocal.AmountMinor != 232 || r.TaxLocal.Source != "ecb" ||
		!r.TaxLocal.RateDate.Equal(rateDay) {
		t.Fatalf("the GB receipt's VAT in pounds = %+v; want £2.32 at the ECB rate of %s", r.TaxLocal, rateDay.Format("2006-01-02"))
	}
	if !r.Preview || r.SupplierVAT() != VATRegistrationPending || r.Buyer.Name != "Ana Smith" || r.ReverseCharge {
		t.Fatalf("the GB receipt: preview %v, supplier VAT %q, buyer %q, reverse charge %v; want a Preview reading %q, to Ana Smith",
			r.Preview, r.SupplierVAT(), r.Buyer.Name, r.ReverseCharge, VATRegistrationPending)
	}
	page, err := r.HTML()
	if err != nil || !bytes.Contains(page, []byte(VATRegistrationPending)) || !bytes.Contains(page, []byte("£2.32")) {
		t.Fatalf("the GB receipt's page (%v) does not read %q and £2.32", err, VATRegistrationPending)
	}
	if doc := r.PDF(); !bytes.HasPrefix(doc, []byte("%PDF-1.4")) || !bytes.Contains(doc, []byte("(Receipt "+r.Number+")")) ||
		!bytes.HasSuffix(doc, []byte("%%EOF\n")) {
		t.Fatalf("the GB receipt's PDF is not a PDF of receipt %s", r.Number)
	}
	// A TEST- receipt issued while the VAT number is unset is as much a Preview reading it as a live one.
	if _, err := pool.Exec(ctx, `INSERT INTO market_receipts (id, series, year, number, invoice_id, buyer_workspace_id, paid_at, supplier, buyer)
		SELECT 'rcpt_test_gb', 'test', year, 1, 'in_rcpt_gb_test', buyer_workspace_id, paid_at, supplier, buyer FROM market_receipts WHERE id = $1`,
		r.ID); err != nil {
		t.Fatal(err)
	}
	if tr, err := m.store.Receipt(ctx, gb, "rcpt_test_gb"); err != nil || tr.Number != fmt.Sprintf("TEST-%d-000001", year) || !tr.Preview ||
		tr.PreviewReason != VATRegistrationPending {
		t.Fatalf("the GB test receipt is %s, preview %v %q (%v); want TEST-%d-000001, a Preview reading %q", tr.Number, tr.Preview,
			tr.PreviewReason, err, year, VATRegistrationPending)
	}

	// Talyvor's VAT number is set; the next paid bill, a DE business's, is reverse charged.
	supplier.VATNumber = "GB123456789"
	m.store.SetReceipts(Receipts{Supplier: supplier, Buyers: m.profiles, Rates: ecbrate.New(pool, "")})
	rent(de, adderListing)
	paid(de, "in_rcpt_de", 1_000)
	r = only(de)
	const note = "Reverse charge: the customer accounts for the tax due in DE"
	if r.Number != fmt.Sprintf("%d-000002", year) || r.Sequence != 2 || r.TaxUSDMicros != 0 || r.GrossUSDMicros != 10_000_000 ||
		!r.ReverseCharge || !slices.Contains(r.Notes, note) || r.Lines[0].Treatment != string(partners.TaxReverseCharge) ||
		r.Buyer.VATNumber != "DE123456789" || r.Preview || r.MatchesStripe == nil || !*r.MatchesStripe {
		t.Fatalf("the DE receipt = %+v; want %d-000002, reverse charged with its note and no VAT, the buyer's VAT number, not a Preview", r, year)
	}
	if page, err := r.HTML(); err != nil || !bytes.Contains(page, []byte(note)) || !bytes.Contains(page, []byte("GB123456789")) ||
		strings.Contains(string(page), VATRegistrationPending) {
		t.Fatalf("the DE receipt's page (%v) does not carry the note and Talyvor's VAT number", err)
	}
}
