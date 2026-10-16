package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/sellertax"
)

// selfbill.go — B32.43: SELF-BILLED INVOICES — THE SELLER'S SUPPLY TO TALYVOR, WITH THEIR VAT.
//
// Under the deemed-supplier rules Talyvor buys from the seller and sells to the buyer, so a VAT-registered seller makes a
// supply to Talyvor. A seller who has agreed to self-billing (B32.41; the agreement is docs/terms/self-billing.md) raises
// no invoice of their own: each weekly payout in money is also a self-billed invoice from them to Talyvor for the earnings
// it pays, taxed through TaxPartner.Calculate with the seller as the supplier and Talyvor as the customer.
//
//   - A UK VAT-registered seller charges UK VAT on top: it is added to the payout, posted −VAT to the seller's available
//     balance and +VAT to tax:GB:input, the input VAT Talyvor reclaims.
//   - A business seller in another country gets the reverse-charge note and no VAT: Talyvor accounts for it.
//   - A seller not registered for VAT charges none.
//
// Off until Talyvor's accountant confirms the treatment: while LENS_SELF_BILLING_VAT is false the invoices are issued at
// zero VAT, "VAT on your supply: under review", and no VAT is posted. The invoice is numbered from a gap-free run per
// seller (live and TEST- runs kept apart), prints both sides as they were when it was issued, and is never changed.

// VATUnderReview is what a self-billed invoice and its statement say of the seller's VAT while LENS_SELF_BILLING_VAT is
// off.
const VATUnderReview = "VAT on your supply: under review"

// TaxUnderReview is the treatment of a self-billed invoice issued while the seller's VAT is under review.
const TaxUnderReview = "under_review"

// AccountTaxInput is the journal's account for the input VAT Talyvor reclaims from jurisdiction's tax authority.
func AccountTaxInput(jurisdiction string) string { return AccountTax(jurisdiction) + ":input" }

// SelfBilling is what the payout run issues self-billed invoices with.
type SelfBilling struct {
	Partners TaxPartners
	Customer Supplier // Talyvor, the customer the seller supplies: LENS_SUPPLIER_*
	VAT      bool     // LENS_SELF_BILLING_VAT: the seller's VAT is worked out and paid; off, every invoice is at zero
}

// SetSelfBilling turns self-billing on: every payout in money to a seller who agreed to it is also a self-billed invoice.
func (s *Store) SetSelfBilling(b SelfBilling) { s.selfBilling = &b }

// SelfBillParty is one side of a self-billed invoice, as it prints.
type SelfBillParty struct {
	Name      string `json:"name"`
	Address   string `json:"address"`
	Country   string `json:"country"`
	VATNumber string `json:"vat_number"` // only a number the tax partner found valid; "" when there is none
}

// SelfBill is a self-billed invoice: from the seller, to Talyvor, for the earnings one payout paid.
type SelfBill struct {
	ID               string        `json:"id"`
	Number           string        `json:"number"` // as printed: SB-000001, or TEST-SB-000001 for test money
	PayoutID         string        `json:"payout_id"`
	Period           string        `json:"period"`
	IssuedAt         time.Time     `json:"issued_at"`
	AgreementVersion string        `json:"agreement_version"`
	Supplier         SelfBillParty `json:"supplier"` // the seller
	Customer         SelfBillParty `json:"customer"` // Talyvor
	NetUSDMicros     int64         `json:"net_usd_micros"`
	VATUSDMicros     int64         `json:"vat_usd_micros"`
	GrossUSDMicros   int64         `json:"gross_usd_micros"`
	RateBps          int           `json:"rate_bps"`
	Jurisdiction     string        `json:"jurisdiction"`
	Treatment        string        `json:"treatment"`
	Note             string        `json:"note"`
	Partner          string        `json:"partner"`
	VATEnabled       bool          `json:"vat_enabled"`
	Preview          bool          `json:"preview"` // test money: "Preview — test money only"

	series string
}

// selfBillNumber is a self-billed invoice's number as printed.
func selfBillNumber(series string, n int) string {
	if series == "test" {
		return fmt.Sprintf("TEST-SB-%06d", n)
	}
	return fmt.Sprintf("SB-%06d", n)
}

// selfBillFor is the self-billed invoice a payout of netUSDMicros of earnings to workspaceID is, its VAT worked out but
// not yet numbered: nil when self-billing is off or the seller has not agreed to it. Its VAT is in whole cents, as the
// payout is.
func (s *Store) selfBillFor(ctx context.Context, q queryRower, workspaceID, payoutID string, netUSDMicros int64, at time.Time) (*SelfBill, error) {
	if s.selfBilling == nil {
		return nil, nil
	}
	var sellerType, first, middle, last, legal string
	var b SelfBill
	var vatValid bool
	var vatNumber string
	err := q.QueryRow(ctx, `SELECT t.seller_type, t.first_name, t.middle_name, t.last_name, t.legal_name, t.address,
		       COALESCE(NULLIF(t.country, ''), upper((SELECT s.country FROM market_sellers s WHERE s.workspace_id = t.workspace_id)), ''),
		       t.vat_number, t.vat_valid, t.self_billing_agreed_version
		FROM seller_tax_profiles t WHERE t.workspace_id = $1`, workspaceID).
		Scan(&sellerType, &first, &middle, &last, &legal, &b.Supplier.Address, &b.Supplier.Country, &vatNumber, &vatValid, &b.AgreementVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("market: the seller's self-billing details: %w", err)
	}
	if b.AgreementVersion == "" {
		return nil, nil
	}
	b.Supplier.Name = legal
	if sellerType == sellertax.Individual {
		b.Supplier.Name = strings.Join(strings.Fields(first+" "+middle+" "+last), " ")
	}
	if b.Supplier.Name == "" {
		b.Supplier.Name = workspaceID
	}
	if vatValid {
		b.Supplier.VATNumber = vatNumber
	}
	cfg := s.selfBilling
	b.Customer = SelfBillParty{Name: cfg.Customer.LegalName, Address: cfg.Customer.Address, Country: SupplierCountry, VATNumber: cfg.Customer.VATNumber}
	b.PayoutID, b.Period, b.IssuedAt, b.NetUSDMicros, b.VATEnabled = payoutID, weekOf(at), at, netUSDMicros, cfg.VAT
	b.Jurisdiction = b.Supplier.Country
	switch {
	case !cfg.VAT:
		b.Treatment, b.Note = TaxUnderReview, VATUnderReview
	case b.Supplier.Country == "":
		b.Treatment, b.Note = TaxUnknownLocation, "No VAT: your country is missing from your tax details"
	default:
		partner := cfg.Partners.Tax()
		// The seller's VAT number is checked again now, in the country they supply from: VAT is paid only on a number
		// valid there when the invoice is issued, not on the answer given when it was saved.
		vatValid, b.Supplier.VATNumber = false, ""
		if vatNumber != "" {
			v, err := partner.ValidateTaxID(ctx, b.Supplier.Country, vatNumber)
			if err != nil {
				return nil, fmt.Errorf("market: check %s's VAT number: %w", workspaceID, err)
			}
			if v.Valid {
				vatValid, b.Supplier.VATNumber = true, v.Number
			}
		}
		customer := partners.TaxParty{ID: partners.SupplierTalyvor, Country: SupplierCountry, Business: true}
		if cfg.Customer.VATNumber != "" {
			v, err := partner.ValidateTaxID(ctx, SupplierCountry, cfg.Customer.VATNumber)
			if err != nil {
				return nil, fmt.Errorf("market: check Talyvor's VAT number: %w", err)
			}
			customer.TaxID, customer.TaxIDValid = v.Number, v.Valid
		}
		r, err := partner.Calculate(ctx, partners.TaxRequest{
			Supplier: partners.TaxParty{ID: workspaceID, Country: b.Supplier.Country, Business: sellerType == sellertax.Entity || vatValid,
				TaxID: b.Supplier.VATNumber, TaxIDValid: vatValid},
			Customer: customer,
			Lines:    []partners.TaxLine{{Ref: payoutID, TaxCode: TaxCodeDigitalService, AmountMicros: netUSDMicros, Currency: "USD"}},
			At:       at,
		})
		if err != nil {
			return nil, fmt.Errorf("market: the VAT on %s's supply: %w", workspaceID, err)
		}
		l := r.Lines[0]
		b.Jurisdiction, b.RateBps, b.Treatment, b.Note, b.Partner = l.Jurisdiction, l.RateBps, string(l.Treatment), l.Note, r.Partner
		// The payout is in cents, so its VAT is too: rounded half-up to the cent.
		b.VATUSDMicros = (l.TaxMicros + usdMicrosPerCent/2) / usdMicrosPerCent * usdMicrosPerCent
	}
	b.GrossUSDMicros = b.NetUSDMicros + b.VATUSDMicros
	return &b, nil
}

// issueSelfBillTx numbers b, the next of the seller's run in its series, and writes it on tx — the payout's transaction,
// which holds the seller's lock — with its VAT on the journal: −VAT to the seller's available balance, +VAT to the input
// VAT Talyvor reclaims.
func issueSelfBillTx(ctx context.Context, tx pgx.Tx, workspaceID string, b *SelfBill, live, test bool) error {
	b.series = funding(live, test)
	b.Preview = b.series == "test"
	var n int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(number), 0) + 1 FROM market_self_bills WHERE workspace_id = $1 AND series = $2`,
		workspaceID, b.series).Scan(&n); err != nil {
		return fmt.Errorf("market: number the self-billed invoice: %w", err)
	}
	b.ID, b.Number = "msb_"+uuid.NewString(), selfBillNumber(b.series, n)
	supplier, err := json.Marshal(b.Supplier)
	if err != nil {
		return err
	}
	customer, err := json.Marshal(b.Customer)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO market_self_bills (id, payout_id, workspace_id, series, number, period, issued_at, agreement_version,
		supplier, customer, net_usd_micros, vat_usd_micros, rate_bps, jurisdiction, treatment, note, partner, vat_enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		b.ID, b.PayoutID, workspaceID, b.series, n, b.Period, b.IssuedAt, b.AgreementVersion, string(supplier), string(customer), b.NetUSDMicros,
		b.VATUSDMicros, b.RateBps, b.Jurisdiction, b.Treatment, b.Note, b.Partner, b.VATEnabled); err != nil {
		return fmt.Errorf("market: issue the self-billed invoice: %w", err)
	}
	_, err = PostJournalTx(ctx, tx, JournalSelfBill, b.PayoutID, b.series, b.IssuedAt,
		Posting{Account: SellerAvailable(workspaceID), AmountUSDMicros: -b.VATUSDMicros},
		Posting{Account: AccountTaxInput(b.Jurisdiction), AmountUSDMicros: b.VATUSDMicros})
	return err
}

// SelfBillOf reads the self-billed invoice a payout of workspaceID's is; pgx.ErrNoRows when it is none.
func (s *Store) SelfBillOf(ctx context.Context, workspaceID, payoutID string) (SelfBill, error) {
	var b SelfBill
	var n int
	var supplier, customer []byte
	err := s.pool.QueryRow(ctx, `SELECT id, series, number, payout_id, period, issued_at, agreement_version, supplier, customer,
		       net_usd_micros, vat_usd_micros, rate_bps, jurisdiction, treatment, note, partner, vat_enabled
		FROM market_self_bills WHERE workspace_id = $1 AND payout_id = $2`, workspaceID, payoutID).
		Scan(&b.ID, &b.series, &n, &b.PayoutID, &b.Period, &b.IssuedAt, &b.AgreementVersion, &supplier, &customer,
			&b.NetUSDMicros, &b.VATUSDMicros, &b.RateBps, &b.Jurisdiction, &b.Treatment, &b.Note, &b.Partner, &b.VATEnabled)
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(supplier, &b.Supplier); err != nil {
		return b, fmt.Errorf("market: self-billed invoice %s: %w", b.ID, err)
	}
	if err := json.Unmarshal(customer, &b.Customer); err != nil {
		return b, fmt.Errorf("market: self-billed invoice %s: %w", b.ID, err)
	}
	b.Number, b.Preview, b.GrossUSDMicros = selfBillNumber(b.series, n), b.series == "test", b.NetUSDMicros+b.VATUSDMicros
	return b, nil
}
