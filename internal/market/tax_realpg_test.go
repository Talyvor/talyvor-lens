package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/taxprofile"
)

// taxStripe is Stripe in test mode for a taxed marketplace bill: it keeps what it is sent.
type taxStripe struct {
	events      []string // "event customer identifier value"
	items       []string // "subscription price": a tax price added to a bill
	credits     []string // "idempotency-key cents": every credit asked for, retries included
	failCredits string   // a credit whose key starts with this fails
}

func (f *taxStripe) CreateCustomer(_ context.Context, ws string) (string, error) {
	return "cus_" + ws, nil
}
func (f *taxStripe) CreateCheckoutSession(context.Context, billing.CheckoutParams) (string, string, error) {
	return "", "", errors.New("not in this test")
}
func (f *taxStripe) CardFingerprint(context.Context, string) (string, error) { return "", nil }
func (f *taxStripe) CreateMarketSubscription(_ context.Context, _, _, ws string) (string, error) {
	return "sub_market_" + ws, nil
}
func (f *taxStripe) SendMeterEvent(_ context.Context, name, customer, identifier string, value int64, _ time.Time) error {
	f.events = append(f.events, fmt.Sprintf("%s %s %s %d", name, customer, identifier, value))
	return nil
}
func (f *taxStripe) CreditMarketUse(_ context.Context, _, _ string, cents float64, _, key string) (string, error) {
	f.credits = append(f.credits, fmt.Sprintf("%s %g", key, cents))
	if f.failCredits != "" && strings.HasPrefix(key, f.failCredits) {
		return "", errors.New("stripe: 503")
	}
	return "ii_" + key, nil
}
func (f *taxStripe) AddMarketSubscriptionItem(_ context.Context, subscription, price string) error {
	f.items = append(f.items, subscription+" "+price)
	return nil
}

// taxedMarket is the marketplace with tax on, as main wires it: the Test tax partner over the fixture rates and the
// registrations given, buyers resolved from their tax profiles, and a bill whose tax line is taxPrice ("" none).
type taxedMarket struct {
	store    *Store
	profiles *taxprofile.Store
	stripe   *taxStripe
	bill     func(taxPrice string) *billing.Service
}

func newTaxedMarket(t *testing.T, pool *pgxpool.Pool, synthetic bool, workspaces []string, registrations ...string) taxedMarket {
	t.Helper()
	ctx := context.Background()
	for _, ws := range workspaces {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, $2, true)`,
			ws, synthetic); err != nil {
			t.Fatal(err)
		}
	}
	taxData := partners.NewTaxStore(pool)
	f, err := os.Open("../partners/testdata/tax/rates.csv")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := partners.ParseTaxRatesCSV(f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := taxData.ImportRates(ctx, rows); err != nil {
		t.Fatal(err)
	}
	for _, j := range registrations {
		if err := taxData.AddRegistration(ctx, partners.TaxRegistration{Jurisdiction: j, Scheme: j + " fixture", Number: j + "000000001",
			EffectiveFrom: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
			t.Fatal(err)
		}
	}
	bank := economy.NewDualTokenStore(nil, pool, nil)
	registry := partners.NewRegistry(bank)
	registry.UseTaxData(taxData)
	m := taxedMarket{store: NewStore(pool), profiles: taxprofile.NewStore(pool, registry), stripe: &taxStripe{}}
	m.store.SetTax(Tax{Partners: registry, Buyers: m.profiles, Registrations: taxData})
	m.bill = func(taxPrice string) *billing.Service {
		return billing.New(pool, bank, m.stripe, "whsec_market_tax").WithMarketBill(m.stripe, "price_market", "talyvor_marketplace_use", m.store).
			WithMarketTax("talyvor_marketplace_tax", taxPrice)
	}
	return m
}

const adder = `{"template":"What is {{a}} + {{b}}?","model":"claude-haiku-4-5"}`

var addOneAndTwo = UseRequest{Variables: map[string]string{"a": "1", "b": "2"}}

// B32.39 — a GB consumer's $10.00 rental writes a 2,000,000 µUSD tax line at 2000 bps and a 20,000,000 µLXC tax
// meter event beside the rent's own; its bill shows the tax; once the invoice is paid one clear entry posts
// +12,000,000 stripe:clearing, −1,500,000 fee, −8,500,000 seller and −2,000,000 tax:GB. A DE business with a valid
// VAT number gets a reverse-charge line of 0 with its note, and no tax event. Taking the listing down credits the GB
// buyer its tax as its own line, once, though its price's credit fails the first time, and the reversal leaves
// nothing owed on tax:GB.
func TestMarketTax_AGBRentalIsTaxedBilledAndClearedToTheTaxAccount(t *testing.T) {
	ctx := context.Background()
	pool := migratedDB(t)
	const seller, gb, de = "ws-tax-seller", "ws-tax-gb", "ws-tax-de"
	m := newTaxedMarket(t, pool, true, []string{seller, gb, de}, "GB", "EU")
	if _, err := m.profiles.Put(ctx, gb, taxprofile.Input{LegalName: "Ana Smith", Country: "GB"}); err != nil {
		t.Fatal(err)
	}
	if p, err := m.profiles.Put(ctx, de, taxprofile.Input{LegalName: "Muster GmbH", Country: "DE", TaxID: "DE123456789"}); err != nil || !p.TaxIDValid {
		t.Fatalf("DE profile = %+v, %v; want its VAT number valid", p, err)
	}
	period := 30
	l, err := m.store.Publish(ctx, seller, Draft{Kind: "prompt", Title: "Adder", Visibility: "public", Artifact: json.RawMessage(adder),
		Offers: []Offer{{Kind: OfferRent, Licence: LicenceCommercial, PriceUSDMicros: 10_000_000, PeriodDays: &period}}})
	if err != nil {
		t.Fatal(err)
	}
	var asked askedCapabilities
	deps := LicenceDeps{Meter: m.bill("price_market_tax"), Capabilities: &asked}
	rent := func(ws string) Licence {
		t.Helper()
		lic, _, err := m.store.License(ctx, deps, ws, "", l.ID, "rent-"+ws, LicenceRequest{OfferID: l.Offers[0].ID})
		if err != nil || lic.Charge != ChargeBilled || lic.MeterError != "" {
			t.Fatalf("%s rents = %+v, %v", ws, lic, err)
		}
		return lic
	}

	gbRent := rent(gb)
	line, err := m.store.TaxLineOf(ctx, gbRent.UseID)
	if err != nil || line.Jurisdiction != "GB" || line.RateBps != 2000 || line.TaxableUSDMicros != 10_000_000 || line.TaxUSDMicros != 2_000_000 ||
		line.Treatment != string(partners.TaxStandard) || line.Partner != "test" {
		t.Fatalf("the GB rental's tax line = %+v, %v; want 2,000,000 µUSD at 2000 bps in GB", line, err)
	}
	wantEvents := []string{
		"talyvor_marketplace_use cus_" + gb + " " + gbRent.UseID + " 100000000",
		"talyvor_marketplace_tax cus_" + gb + " tax-" + gbRent.UseID + " 20000000",
	}
	if fmt.Sprint(m.stripe.events) != fmt.Sprint(wantEvents) || fmt.Sprint(m.stripe.items) != "[sub_market_"+gb+" price_market_tax]" {
		t.Fatalf("Stripe got events %v and items %v; want %v, the tax price added to the bill once", m.stripe.events, m.stripe.items, wantEvents)
	}
	bill, err := m.store.MonthBill(ctx, gb, time.Now().UTC())
	if err != nil || len(bill.Lines) != 1 || bill.Lines[0].TaxUSDMicros != 2_000_000 || bill.Lines[0].TaxJurisdiction != "GB" ||
		bill.NetUSDMicros != 10_000_000 || bill.TaxUSDMicros != 2_000_000 || bill.GrossUSDMicros != 12_000_000 {
		t.Fatalf("the GB buyer's bill = %+v, %v; want its line taxed 2,000,000 and net 10,000,000, tax 2,000,000, gross 12,000,000", bill, err)
	}

	if n, err := m.store.ClearInvoice(ctx, gb, "in_tax_1", time.Unix(0, 0), time.Now().Add(time.Hour), time.Now(), false); err != nil || n != 1 {
		t.Fatalf("clear = %d, %v", n, err)
	}
	entries, err := m.store.JournalFor(ctx, gbRent.UseID)
	if err != nil || len(entries) != 1 || entries[0].Kind != JournalClear {
		t.Fatalf("the rental's journal = %+v, %v; want one clear entry", entries, err)
	}
	got := map[string]int64{}
	for _, p := range entries[0].Postings {
		got[p.Account] += p.AmountUSDMicros
	}
	want := map[string]int64{AccountStripeClearing: 12_000_000, AccountMarketFee: -1_500_000, SellerHoldback(seller): -8_500_000, AccountTax("GB"): -2_000_000}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the rental's clear entry posts %v; want %v", got, want)
	}
	if b, err := m.store.JournalBalance(ctx, AccountTax("GB"), ""); err != nil || b != -2_000_000 {
		t.Fatalf("tax:GB balance = %d, %v; want −2,000,000 owed to HMRC", b, err)
	}

	deRent := rent(de)
	line, err = m.store.TaxLineOf(ctx, deRent.UseID)
	if err != nil || line.Jurisdiction != "DE" || line.TaxUSDMicros != 0 || line.Treatment != string(partners.TaxReverseCharge) ||
		line.Note != "Reverse charge: the customer accounts for the tax due in DE" || !strings.Contains(string(line.Evidence), `"DE123456789"`) {
		t.Fatalf("the DE business's tax line = %+v, %v; want reverse charge, 0, its note and its VAT number in the evidence", line, err)
	}
	if n := len(m.stripe.events); n != 3 || !strings.Contains(m.stripe.events[2], " "+deRent.UseID+" ") {
		t.Fatalf("Stripe has %d events (%v); want the DE rent's own and no tax event for it", n, m.stripe.events)
	}

	refunder := m.bill("price_market_tax")
	m.stripe.failCredits = "market-refund-use_"
	if td, err := m.store.TakeDown(ctx, refunder, l.ID, "copied work"); err != nil || td.CreditError == "" {
		t.Fatalf("takedown = %+v, %v; want the price credits refused this time", td, err)
	}
	m.stripe.failCredits = ""
	if _, _, err := m.store.RefundTakenDown(ctx, refunder); err != nil {
		t.Fatal(err)
	}
	taxCredit := "market-refund-tax-" + gbRent.UseID + " 200"
	var taxCredits int
	for _, c := range m.stripe.credits {
		if strings.HasPrefix(c, "market-refund-tax-") {
			taxCredits++
			if c != taxCredit {
				t.Errorf("tax credit %q; want only %q", c, taxCredit)
			}
		}
	}
	if taxCredits != 1 || !slices.Contains(m.stripe.credits, "market-refund-"+gbRent.UseID+" 1000") {
		t.Fatalf("credits asked for %v; want the GB tax credited once (200¢) and its price (1000¢)", m.stripe.credits)
	}
	if b, err := m.store.JournalBalance(ctx, AccountTax("GB"), ""); err != nil || b != 0 {
		t.Fatalf("tax:GB after the refund = %d, %v; want 0: the reversal gave the tax back", b, err)
	}
}

// B32.39 — with a live key and no EU registration, a real DE consumer's use is refused before it runs and writes no
// row: "not available in your country yet". A real GB consumer, where Talyvor is registered, is served.
func TestMarketTax_ALiveKeyRefusesASaleTalyvorCannotAccountTheTaxOn(t *testing.T) {
	ctx := context.Background()
	pool := migratedDB(t)
	const seller, gb, de = "ws-live-seller", "ws-live-gb", "ws-live-de"
	m := newTaxedMarket(t, pool, false, []string{seller, gb, de}, "GB")
	m.store.SetLiveStripe(true)
	for ws, country := range map[string]string{gb: "GB", de: "DE"} {
		if _, err := m.profiles.Put(ctx, ws, taxprofile.Input{LegalName: ws, Country: country}); err != nil {
			t.Fatal(err)
		}
	}
	l, err := m.store.Publish(ctx, seller, Draft{Kind: "prompt", Title: "Adder", PricePerUseULXC: 10_000_000, Visibility: "public",
		Artifact: json.RawMessage(adder)})
	if err != nil {
		t.Fatal(err)
	}
	deps := UseDeps{Runner: answers("3"), Meter: m.bill("price_market_tax")}
	if u, err := m.store.Use(ctx, deps, de, "", l.ID, addOneAndTwo); !errors.Is(err, ErrNotSoldHere) ||
		!strings.Contains(err.Error(), "not available in your country yet") {
		t.Fatalf("a live DE consumer's use = %+v, %v; want refused: not available in your country yet", u, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_uses WHERE buyer_workspace_id = $1`, de).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("the refused use left %d rows (%v); want none", rows, err)
	}
	if u, err := m.store.Use(ctx, deps, gb, "", l.ID, addOneAndTwo); err != nil || u.Charge != ChargeBilled {
		t.Fatalf("a live GB consumer's use = %+v, %v; want it served and billed", u, err)
	}
}

// B32.39 — a bill with no tax line refuses a use's tax event but not its price: the price is metered and clears, the
// clear entry posts only what the invoice collected, and the tax waits, counted as a refusal. Once the bill has a
// tax line, the next pass sends the tax event alone, once.
func TestMarketTax_ARefusedTaxEventNeverStrandsABilledPrice(t *testing.T) {
	ctx := context.Background()
	pool := migratedDB(t)
	const seller, gb = "ws-untaxed-seller", "ws-untaxed-gb"
	m := newTaxedMarket(t, pool, true, []string{seller, gb}, "GB")
	if _, err := m.profiles.Put(ctx, gb, taxprofile.Input{LegalName: "Ana Smith", Country: "GB"}); err != nil {
		t.Fatal(err)
	}
	l, err := m.store.Publish(ctx, seller, Draft{Kind: "prompt", Title: "Adder", PricePerUseULXC: 100_000_000, Visibility: "public",
		Artifact: json.RawMessage(adder)})
	if err != nil {
		t.Fatal(err)
	}
	u, err := m.store.Use(ctx, UseDeps{Runner: answers("3"), Meter: m.bill("")}, gb, "", l.ID, addOneAndTwo)
	if err != nil || !strings.Contains(u.MeterError, "LENS_MARKET_TAX_PRICE_ID") {
		t.Fatalf("use = %+v, %v; want it served, its tax not yet on the bill", u, err)
	}
	var priceBilled, taxBilled bool
	var refusals int
	read := func() {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT metered_at IS NOT NULL, tax_metered_at IS NOT NULL, meter_refusals FROM market_uses WHERE id = $1`,
			u.ID).Scan(&priceBilled, &taxBilled, &refusals); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if !priceBilled || taxBilled || refusals != 1 || len(m.stripe.events) != 1 {
		t.Fatalf("price billed %v, tax billed %v, refusals %d, events %v; want the price alone on the bill and the tax refused once",
			priceBilled, taxBilled, refusals, m.stripe.events)
	}
	if n, err := m.store.ClearInvoice(ctx, gb, "in_untaxed_1", time.Unix(0, 0), time.Now().Add(time.Hour), time.Now(), false); err != nil || n != 1 {
		t.Fatalf("clear = %d, %v", n, err)
	}
	entries, err := m.store.JournalFor(ctx, u.ID)
	if err != nil || len(entries) != 1 || len(entries[0].Postings) != 3 || entries[0].Postings[0].AmountUSDMicros != 10_000_000 {
		t.Fatalf("the clear entry = %+v, %v; want +10,000,000 clearing, the fee and the seller, and no tax the invoice did not collect", entries, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE market_uses SET meter_retry_at = now() - interval '1 second', ran_at = ran_at - interval '2 minutes'
		WHERE id = $1`, u.ID); err != nil {
		t.Fatal(err)
	}
	taxed := m.bill("price_market_tax")
	for range 2 {
		if _, err := m.store.MeterPending(ctx, taxed, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	read()
	want := "talyvor_marketplace_tax cus_" + gb + " tax-" + u.ID + " 20000000"
	if !taxBilled || len(m.stripe.events) != 2 || m.stripe.events[1] != want {
		t.Fatalf("after the tax line was added: tax billed %v, events %v; want %q sent once and the price not again", taxBilled, m.stripe.events, want)
	}
}

// B17.154 — the testers' market-bill-tax and market-receipts pay a test GB consumer's taxed bill through PayTestBill,
// receipts on, and were answered 500 while the receipt's JSON failed to insert behind PgBouncer (22P02, fixed by
// #817). Paid that way, the bill clears with its tax owed on tax:GB and gets one receipt of net plus tax.
func TestMarketTax_ATestGBConsumersTaxedBillIsPaidWithItsReceipt(t *testing.T) {
	ctx := context.Background()
	pool := migratedDB(t)
	const seller, gb = "ws-paytax-seller", "ws-paytax-gb"
	m := newTaxedMarket(t, pool, true, []string{seller, gb}, "GB")
	if _, err := m.profiles.Put(ctx, gb, taxprofile.Input{LegalName: "Ana Smith", Country: "GB"}); err != nil {
		t.Fatal(err)
	}
	period := 30
	l, err := m.store.Publish(ctx, seller, Draft{Kind: "prompt", Title: "Adder", Visibility: "public", Artifact: json.RawMessage(adder),
		Offers: []Offer{{Kind: OfferRent, Licence: LicenceCommercial, PriceUSDMicros: 10_000_000, PeriodDays: &period}}})
	if err != nil {
		t.Fatal(err)
	}
	deps := LicenceDeps{Meter: m.bill("price_market_tax"), Capabilities: &askedCapabilities{}}
	if lic, _, err := m.store.License(ctx, deps, gb, "", l.ID, "rent-"+gb, LicenceRequest{OfferID: l.Offers[0].ID}); err != nil ||
		lic.Charge != ChargeBilled {
		t.Fatalf("the GB consumer rents = %+v, %v", lic, err)
	}

	m.store.SetReceipts(Receipts{Supplier: Supplier{LegalName: "Talyvor Ltd"}, Buyers: m.profiles})
	invoice, n, err := m.store.PayTestBill(ctx, gb, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("paying the GB consumer's bill = %d uses, %v; want the rental cleared", n, err)
	}
	if b, err := m.store.JournalBalance(ctx, AccountTax("GB"), ""); err != nil || b != -2_000_000 {
		t.Fatalf("tax:GB balance = %d, %v; want −2,000,000 owed", b, err)
	}
	receipts, err := m.store.ReceiptsOf(ctx, gb)
	if err != nil || len(receipts) != 1 || receipts[0].InvoiceID != invoice || receipts[0].TaxUSDMicros != 2_000_000 ||
		receipts[0].GrossUSDMicros != 12_000_000 {
		t.Fatalf("the GB consumer's receipts = %+v, %v; want one for %s, tax 2,000,000 of gross 12,000,000", receipts, err, invoice)
	}
}
