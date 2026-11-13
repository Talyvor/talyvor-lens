package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/taxprofile"
)

// tax.go — B32.39: VAT AND SALES TAX ON EVERY BILLED MARKETPLACE USE.
//
// Talyvor is the seller of record on the marketplace, so every billed use — a use, a buy, a rent, a subscription or
// renewal, a room run, a prize — carries its buyer's tax on top of its price. The tax is worked out once, when the
// use is metered: the tax partner's answer for the buyer's resolved profile (internal/taxprofile) and tax code
// digital_service, written to market_tax_lines with the evidence it was decided on. A tax above zero is a second
// meter event on the buyer's bill, identified "tax-" plus the use id, so Stripe's invoice shows it as its own line.
// Clearing posts it to tax:<jurisdiction>; a refund credits it back and reverses that posting. Talyvor's take and
// the seller's share are of the price before tax.
//
// While Lens's Stripe key is live, a real buyer's charge is refused before anything runs where Talyvor cannot account
// for its tax: a consumer in a jurisdiction marked registration_from_first_sale where Talyvor holds no registration,
// or a buyer whose location is unknown. A test workspace pays in Stripe test mode, so its charges are never refused.

// TaxCodeDigitalService is the tax code every marketplace sale is taxed under.
const TaxCodeDigitalService = "digital_service"

// SupplierCountry is where Talyvor supplies from: TALYVOR LTD is registered in England and Wales.
const SupplierCountry = "GB"

// TaxUnknownLocation is the treatment of a use whose buyer no evidence places: no tax was worked out.
const TaxUnknownLocation = "unknown_location"

// AccountTax is the journal's account for the tax owed to jurisdiction's tax authority.
func AccountTax(jurisdiction string) string { return "tax:" + jurisdiction }

// ErrNotSoldHere: Talyvor cannot account for the tax on this sale, so it does not make it.
var ErrNotSoldHere = errors.New("market: not available in your country yet")

// TaxBuyers says where a buyer is for tax: *taxprofile.Store.
type TaxBuyers interface {
	Resolve(ctx context.Context, workspaceID string) (taxprofile.Resolution, error)
}

// TaxPartners hands out the tax partner: *partners.Registry.
type TaxPartners interface {
	Tax() partners.TaxPartner
}

// TaxRegistrations is the operator's tax data a live sale is checked against: *partners.TaxStore.
type TaxRegistrations interface {
	RegistrationFromFirstSale(ctx context.Context, jurisdiction string) (bool, error)
	TaxRegistration(ctx context.Context, jurisdiction string, at time.Time) (partners.TaxRegistration, bool, error)
}

// Tax is what the marketplace works out a billed use's tax with.
type Tax struct {
	Partners      TaxPartners
	Buyers        TaxBuyers
	Registrations TaxRegistrations
}

// TaxMeter puts a billed use's tax on its buyer's bill as its own line, identified "tax-" plus the use id, so a
// retry never bills it twice. A Meter that is not one cannot bill a use whose tax is above zero.
type TaxMeter interface {
	MeterMarketTax(ctx context.Context, buyerWorkspaceID, useID string, ulxc int64, at time.Time) error
}

// TaxRefunder credits a refunded use's tax back on its buyer's bill, once per use.
type TaxRefunder interface {
	CreditMarketRefundTax(ctx context.Context, buyerWorkspaceID, useID string, ulxc int64, description string) (creditID string, err error)
}

// SetTax turns tax on: every billed use metered from now on carries its tax.
func (s *Store) SetTax(t Tax) { s.tax = &t }

// SetLiveStripe tells the store whether Lens's Stripe key is live: then a real buyer's sale Talyvor cannot account
// for the tax on is refused.
func (s *Store) SetLiveStripe(live bool) { s.liveStripe = live }

// TaxLine is a market_tax_lines row: the tax on one billed use.
type TaxLine struct {
	UseID            string          `json:"use_id"`
	Jurisdiction     string          `json:"jurisdiction"`
	RateBps          int             `json:"rate_bps"`
	TaxableUSDMicros int64           `json:"taxable_usd_micros"`
	TaxUSDMicros     int64           `json:"tax_usd_micros"`
	Treatment        string          `json:"treatment"`
	Note             string          `json:"note"`
	Evidence         json.RawMessage `json:"evidence"`
	Partner          string          `json:"partner"`
	CalculatedAt     time.Time       `json:"calculated_at"`
}

// TaxLineOf reads the tax on a use; pgx.ErrNoRows when none was worked out.
func (s *Store) TaxLineOf(ctx context.Context, useID string) (TaxLine, error) {
	var l TaxLine
	err := s.pool.QueryRow(ctx, `SELECT use_id, jurisdiction, rate_bps, taxable_usd_micros, tax_usd_micros, treatment, note, evidence,
		partner, calculated_at FROM market_tax_lines WHERE use_id = $1`, useID).
		Scan(&l.UseID, &l.Jurisdiction, &l.RateBps, &l.TaxableUSDMicros, &l.TaxUSDMicros, &l.Treatment, &l.Note, &l.Evidence,
			&l.Partner, &l.CalculatedAt)
	return l, err
}

// taxUse is the tax on one billed use of ulxc µLXC sold at at: its market_tax_lines row, worked out and written the
// first time it is asked for. nil when tax is not turned on, or the price is less than one µUSD.
func (s *Store) taxUse(ctx context.Context, useID, buyer string, ulxc int64, at time.Time) (*TaxLine, error) {
	gross := ulxc / ulxcPerUSDMicro
	if s.tax == nil || gross <= 0 {
		return nil, nil
	}
	if l, err := s.TaxLineOf(ctx, useID); err == nil {
		return &l, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("market: tax of %s: %w", useID, err)
	}
	r, err := s.tax.Buyers.Resolve(ctx, buyer)
	if err != nil {
		return nil, fmt.Errorf("market: where the buyer of %s is: %w", useID, err)
	}
	l := TaxLine{UseID: useID}
	if l.Evidence, err = json.Marshal(r); err != nil {
		return nil, err
	}
	if !r.Known {
		l.Treatment, l.Note = TaxUnknownLocation, "No tax charged: where the buyer is is not known"
	} else {
		partner := s.tax.Partners.Tax()
		res, err := partner.Calculate(partners.WithWorkspace(ctx, buyer), partners.TaxRequest{
			Supplier: partners.TaxParty{ID: partners.SupplierTalyvor, Country: SupplierCountry},
			Customer: r.Customer(),
			Lines:    []partners.TaxLine{{Ref: useID, TaxCode: TaxCodeDigitalService, AmountMicros: gross, Currency: "USD"}},
			At:       at,
		})
		if err != nil {
			return nil, fmt.Errorf("market: tax of %s: %w", useID, err)
		}
		if len(res.Lines) != 1 {
			return nil, fmt.Errorf("market: tax of %s: the %s tax partner answered %d lines for one", useID, res.Partner, len(res.Lines))
		}
		x := res.Lines[0]
		l.Jurisdiction, l.RateBps, l.TaxableUSDMicros, l.TaxUSDMicros = x.Jurisdiction, x.RateBps, x.TaxableMicros, x.TaxMicros
		l.Treatment, l.Note, l.Partner = string(x.Treatment), x.Note, res.Partner
	}
	// Two meters of one use at once write one line: the first; both answer it.
	if _, err := s.pool.Exec(ctx, `INSERT INTO market_tax_lines (use_id, jurisdiction, rate_bps, taxable_usd_micros, tax_usd_micros,
			treatment, note, evidence, partner) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (use_id) DO NOTHING`,
		l.UseID, l.Jurisdiction, l.RateBps, l.TaxableUSDMicros, l.TaxUSDMicros, l.Treatment, l.Note, l.Evidence, l.Partner); err != nil {
		return nil, fmt.Errorf("market: tax of %s: %w", useID, err)
	}
	if l, err = s.TaxLineOf(ctx, useID); err != nil {
		return nil, fmt.Errorf("market: tax of %s: %w", useID, err)
	}
	return &l, nil
}

// meterTax puts a use's tax on its buyer's bill, when there is any.
func meterTax(ctx context.Context, m Meter, tax *TaxLine, buyer, useID string, at time.Time) error {
	if tax == nil || tax.TaxUSDMicros == 0 {
		return nil
	}
	tm, ok := m.(TaxMeter)
	if !ok {
		return fmt.Errorf("%w: use %s owes %d µUSD of tax", billing.ErrNoMarketTax, useID, tax.TaxUSDMicros)
	}
	return tm.MeterMarketTax(ctx, buyer, useID, tax.TaxUSDMicros*ulxcPerUSDMicro, at)
}

// sellable refuses, while Lens's Stripe key is live, a real buyer's charge Talyvor cannot account for the tax on: one
// whose location is unknown, or a consumer's in a jurisdiction marked registration_from_first_sale where Talyvor
// holds no registration. It is asked before anything runs or is recorded.
func (s *Store) sellable(ctx context.Context, buyer string) error {
	if !s.liveStripe || s.tax == nil {
		return nil
	}
	var test bool
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT synthetic FROM workspaces WHERE id = $1), false)`, buyer).Scan(&test); err != nil {
		return fmt.Errorf("market: is the buyer a test workspace: %w", err)
	}
	if test {
		return nil // a test workspace pays in Stripe test mode: test money, whatever the key
	}
	r, err := s.tax.Buyers.Resolve(ctx, buyer)
	if err != nil {
		return fmt.Errorf("market: where the buyer is: %w", err)
	}
	if !r.Known {
		return fmt.Errorf("%w: nothing says where you are. Declare your country in your tax profile", ErrNotSoldHere)
	}
	if r.Business {
		return nil
	}
	marked, err := s.tax.Registrations.RegistrationFromFirstSale(ctx, r.Country)
	if err != nil {
		return fmt.Errorf("market: the tax rules of %s: %w", r.Country, err)
	}
	if !marked {
		return nil
	}
	_, registered, err := s.tax.Registrations.TaxRegistration(ctx, r.Country, s.now())
	if err != nil {
		return fmt.Errorf("market: Talyvor's tax registration in %s: %w", r.Country, err)
	}
	if !registered {
		return fmt.Errorf("%w: Talyvor cannot charge the tax due on a sale in %s", ErrNotSoldHere, r.Country)
	}
	return nil
}
