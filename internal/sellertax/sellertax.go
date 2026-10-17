// Package sellertax is a marketplace seller's tax details (B32.41): who is paid, where they live, their tax numbers
// and the account they are paid to — what the UK reporting rules and DAC7 have a platform collect from every seller
// it pays — and the reminders and payout hold that follow a seller who has not given them.
//
//   - An individual gives first, middle and last names and a date of birth; an entity its legal name and company
//     registration number. Both give a primary address, a country of residence, at least one TIN with the
//     jurisdiction that issued it, and the financial account they are paid to with its holder.
//   - A VAT number is checked with TaxPartner.ValidateTaxID when it is saved. One that is not valid is kept, marked
//     invalid, and leaves the details incomplete until it is corrected or removed.
//   - The TINs, the date of birth and the account identifier are sealed with internal/envelope and read back masked:
//     each TIN and the account identifier to its last four characters, the date of birth not at all. Only Identify
//     opens them, for the annual platform-reporting export (B32.44), the one reader they are kept for.
//   - A seller with earnings and incomplete details is asked for them at their first earning and reminded twice
//     more, the reminder interval apart. The second reminder starts the payout hold: the payout run skips the seller
//     while their earnings keep clearing, until the details are complete.
package sellertax

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/envelope"
	"github.com/talyvor/lens/internal/partners"
)

var (
	// ErrInvalid is details that cannot be saved as given.
	ErrInvalid = errors.New("sellertax: invalid tax details")
	// ErrNoCustody is a Lens with no key to seal the details under: they are never stored in the clear.
	ErrNoCustody = errors.New("sellertax: seller tax details cannot be stored here yet: LENS_PROVIDER_SECRET_KEK is not set")
)

// Taxes hands out the tax partner: *partners.Registry.
type Taxes interface {
	Tax() partners.TaxPartner
}

// The kinds of seller.
const (
	Individual = "individual"
	Entity     = "entity"
)

// Requests is how many times a seller with earnings is asked for incomplete details: at their first earning, then
// twice more. The last of them starts the payout hold.
const Requests = 3

// defaultReminderEvery is the time between the requests: LENS_SELLER_TAX_REMINDER_DAYS, default 30 — a proposal.
const defaultReminderEvery = 30 * 24 * time.Hour

// HoldReason is what a withheld seller is told.
const HoldReason = "Your payouts are on hold until your tax details are complete. Your earnings keep clearing and are paid at the next payout after you complete them."

// The masks the sealed values read back as.
const (
	maskPrefix      = "••••"
	maskDateOfBirth = "••••-••-••"
)

// TIN is a taxpayer identification number and the jurisdiction that issued it. Read back, Number is masked to its
// last four characters.
type TIN struct {
	Jurisdiction string `json:"jurisdiction"`
	Number       string `json:"number"`
}

// Input is what a seller saves. TINs, DateOfBirth and AccountIdentifier left out keep what is stored — they read
// back masked, so a seller correcting an address does not give them again; given empty, they are removed.
type Input struct {
	SellerType                string  `json:"seller_type"`
	FirstName                 string  `json:"first_name"`
	MiddleName                string  `json:"middle_name"`
	LastName                  string  `json:"last_name"`
	LegalName                 string  `json:"legal_name"`
	Address                   string  `json:"address"`
	Country                   string  `json:"country"`
	TINs                      *[]TIN  `json:"tins"`
	DateOfBirth               *string `json:"date_of_birth"` // YYYY-MM-DD, an individual's
	CompanyRegistrationNumber string  `json:"company_registration_number"`
	VATNumber                 string  `json:"vat_number"`
	AccountIdentifier         *string `json:"account_identifier"` // an IBAN or an account number
	AccountHolder             string  `json:"account_holder"`
	SelfBillingAgreedVersion  string  `json:"self_billing_agreed_version"`
}

// Details is a seller's tax details as they are shown: the sealed values masked, what is still missing, and where
// the reminders and the payout hold stand.
type Details struct {
	WorkspaceID               string     `json:"workspace_id"`
	SellerType                string     `json:"seller_type"`
	FirstName                 string     `json:"first_name"`
	MiddleName                string     `json:"middle_name"`
	LastName                  string     `json:"last_name"`
	LegalName                 string     `json:"legal_name"`
	Address                   string     `json:"address"`
	Country                   string     `json:"country"`
	TINs                      []TIN      `json:"tins"`          // each number masked to its last four characters
	DateOfBirth               string     `json:"date_of_birth"` // masked; "" when not given
	CompanyRegistrationNumber string     `json:"company_registration_number"`
	VATNumber                 string     `json:"vat_number"` // as the tax partner normalised it
	VATValid                  bool       `json:"vat_valid"`
	VATDetail                 string     `json:"vat_detail,omitempty"` // why it is not valid
	VATPartner                string     `json:"vat_partner,omitempty"`
	VATCheckedAt              *time.Time `json:"vat_checked_at,omitempty"`
	AccountIdentifier         string     `json:"account_identifier"` // masked to its last four characters
	AccountHolder             string     `json:"account_holder"`
	SelfBillingAgreedVersion  string     `json:"self_billing_agreed_version"`
	Complete                  bool       `json:"complete"`
	Missing                   []string   `json:"missing"` // the fields still to give
	CompletedAt               *time.Time `json:"completed_at,omitempty"`
	RemindersSent             int        `json:"reminders_sent"`
	LastRemindedAt            *time.Time `json:"last_reminded_at,omitempty"`
	NextReminderAt            *time.Time `json:"next_reminder_at,omitempty"`
	WithheldSince             *time.Time `json:"withheld_since,omitempty"`
	Hold                      string     `json:"hold,omitempty"` // why payouts are held
	Accepting                 bool       `json:"accepting"`      // false: they cannot be stored here yet
}

// Store keeps the details in Postgres, sealed under ring, their VAT numbers checked with taxes' partner.
type Store struct {
	pool  *pgxpool.Pool
	ring  *envelope.Keyring
	taxes Taxes
	every time.Duration
	now   func() time.Time
}

// NewStore is the details in pool. A nil ring is a Lens with no custody: nothing is stored, no seller is asked for
// details they could not give, and none is held.
func NewStore(pool *pgxpool.Pool, ring *envelope.Keyring, taxes Taxes) *Store {
	return &Store{pool: pool, ring: ring, taxes: taxes, every: defaultReminderEvery, now: time.Now}
}

// SetReminderEvery is the time between the requests to a seller.
func (s *Store) SetReminderEvery(d time.Duration) {
	if d > 0 {
		s.every = d
	}
}

// aad binds a sealed value to its seller and field, so one copied into another row or column does not open.
func aad(workspaceID, field string) []byte {
	return []byte("seller_tax|" + workspaceID + "|" + field)
}

const detailsColumns = `workspace_id, seller_type, first_name, middle_name, last_name, legal_name, address, country,
	tins_masked, date_of_birth_sealed IS NOT NULL, company_registration_number, vat_number, vat_valid, vat_detail,
	vat_partner, vat_checked_at, account_identifier_last4, account_holder, self_billing_agreed_version, completed_at,
	reminders_sent, last_reminded_at, withheld_since`

func (s *Store) scan(row pgx.Row) (Details, error) {
	var d Details
	var masked []byte
	var dob bool
	err := row.Scan(&d.WorkspaceID, &d.SellerType, &d.FirstName, &d.MiddleName, &d.LastName, &d.LegalName, &d.Address,
		&d.Country, &masked, &dob, &d.CompanyRegistrationNumber, &d.VATNumber, &d.VATValid, &d.VATDetail, &d.VATPartner,
		&d.VATCheckedAt, &d.AccountIdentifier, &d.AccountHolder, &d.SelfBillingAgreedVersion, &d.CompletedAt,
		&d.RemindersSent, &d.LastRemindedAt, &d.WithheldSince)
	if err != nil {
		return Details{}, err
	}
	if err := json.Unmarshal(masked, &d.TINs); err != nil {
		return Details{}, fmt.Errorf("sellertax: read the masked TINs: %w", err)
	}
	if dob {
		d.DateOfBirth = maskDateOfBirth
	}
	if d.AccountIdentifier != "" {
		d.AccountIdentifier = maskPrefix + d.AccountIdentifier
	}
	return s.annotate(d), nil
}

// annotate fills in what is missing, the next reminder and the hold.
func (s *Store) annotate(d Details) Details {
	if d.TINs == nil {
		d.TINs = []TIN{}
	}
	d.Missing = missing(d)
	d.Complete = len(d.Missing) == 0
	d.Accepting = s.ring != nil
	if !d.Complete && d.RemindersSent > 0 && d.RemindersSent < Requests && d.LastRemindedAt != nil {
		next := d.LastRemindedAt.Add(s.every)
		d.NextReminderAt = &next
	}
	if d.WithheldSince != nil {
		d.Hold = HoldReason
	}
	return d
}

// missing is the fields a seller still has to give, in the order a form asks for them.
func missing(d Details) []string {
	out := []string{}
	need := func(name string, given bool) {
		if !given {
			out = append(out, name)
		}
	}
	need("seller_type", d.SellerType != "")
	switch d.SellerType {
	case Individual:
		need("first_name", d.FirstName != "")
		need("last_name", d.LastName != "")
		need("date_of_birth", d.DateOfBirth != "")
	case Entity:
		need("legal_name", d.LegalName != "")
		need("company_registration_number", d.CompanyRegistrationNumber != "")
	}
	need("address", d.Address != "")
	need("country", d.Country != "")
	need("tins", len(d.TINs) > 0)
	need("vat_number", d.VATNumber == "" || d.VATValid) // only an invalid one: a VAT number is given where there is one
	need("account_identifier", d.AccountIdentifier != "")
	need("account_holder", d.AccountHolder != "")
	return out
}

// Get is the seller's details — empty, with everything missing, before they give any.
func (s *Store) Get(ctx context.Context, workspaceID string) (Details, error) {
	d, err := s.scan(s.pool.QueryRow(ctx, `SELECT `+detailsColumns+` FROM seller_tax_profiles WHERE workspace_id = $1`, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return s.annotate(Details{WorkspaceID: workspaceID}), nil
	}
	if err != nil {
		return Details{}, fmt.Errorf("sellertax: read %s: %w", workspaceID, err)
	}
	return d, nil
}

var (
	countryCode = regexp.MustCompile(`^[A-Z]{2}$`)
	tinNumber   = regexp.MustCompile(`^[A-Z0-9]{5,32}$`)
	accountID   = regexp.MustCompile(`^[A-Z0-9]{4,34}$`)
)

// compact is an identifier uppercased, without the spaces, dots, dashes and slashes people write it with.
func compact(v string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '.', '-', '/':
			return -1
		}
		return r
	}, strings.ToUpper(strings.TrimSpace(v)))
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// sealedField is one sealed column a save sets: present is whether it was given at all, sealed its envelope as
// JSON (nil to remove it).
type sealedField struct {
	present bool
	sealed  []byte
}

// value binds the envelope to its jsonb column as text (a []byte goes out as bytea on PgBouncer's simple protocol,
// #133), and nil as NULL.
func (f sealedField) value() any {
	if f.sealed == nil {
		return nil
	}
	return string(f.sealed)
}

func (s *Store) seal(workspaceID, field string, plaintext []byte) ([]byte, error) {
	sealed, err := s.ring.Seal(plaintext, aad(workspaceID, field))
	if err != nil {
		return nil, fmt.Errorf("sellertax: seal %s: %w", field, err)
	}
	return json.Marshal(sealed)
}

// Put saves what the seller gives, replacing their details — except the sealed values left out, which are kept.
// A VAT number is checked with the tax partner first. Saving complete details ends a payout hold; saving incomplete
// ones after the last request starts it again.
func (s *Store) Put(ctx context.Context, workspaceID string, in Input) (Details, error) {
	if s.ring == nil {
		return Details{}, ErrNoCustody
	}
	d := Details{WorkspaceID: workspaceID, SellerType: strings.TrimSpace(in.SellerType)}
	if d.SellerType != Individual && d.SellerType != Entity {
		return Details{}, invalid("seller_type is %q or %q, not %q", Individual, Entity, in.SellerType)
	}
	for _, f := range []struct {
		name string
		v    string
		max  int
		into *string
	}{
		{"first_name", in.FirstName, 100, &d.FirstName},
		{"middle_name", in.MiddleName, 100, &d.MiddleName},
		{"last_name", in.LastName, 100, &d.LastName},
		{"legal_name", in.LegalName, 200, &d.LegalName},
		{"address", in.Address, 500, &d.Address},
		{"company_registration_number", in.CompanyRegistrationNumber, 32, &d.CompanyRegistrationNumber},
		{"account_holder", in.AccountHolder, 200, &d.AccountHolder},
		{"self_billing_agreed_version", in.SelfBillingAgreedVersion, 32, &d.SelfBillingAgreedVersion},
	} {
		*f.into = strings.TrimSpace(f.v)
		if len([]rune(*f.into)) > f.max {
			return Details{}, invalid("%s is at most %d characters", f.name, f.max)
		}
	}
	if d.Country = strings.ToUpper(strings.TrimSpace(in.Country)); d.Country != "" && !countryCode.MatchString(d.Country) {
		return Details{}, invalid("country is two letters (ISO 3166-1), such as GB or DE, not %q", in.Country)
	}

	var tins, dob, account sealedField
	masked := []TIN{}
	if in.TINs != nil {
		tins.present = true
		given := *in.TINs
		if len(given) > 5 {
			return Details{}, invalid("give at most 5 TINs")
		}
		clean := make([]TIN, 0, len(given))
		for _, t := range given {
			c := TIN{Jurisdiction: strings.ToUpper(strings.TrimSpace(t.Jurisdiction)), Number: compact(t.Number)}
			if !countryCode.MatchString(c.Jurisdiction) {
				return Details{}, invalid("a TIN's jurisdiction is two letters (ISO 3166-1), not %q", t.Jurisdiction)
			}
			if !tinNumber.MatchString(c.Number) {
				return Details{}, invalid("a TIN is 5 to 32 letters and digits")
			}
			clean = append(clean, c)
			masked = append(masked, TIN{Jurisdiction: c.Jurisdiction, Number: maskPrefix + c.Number[len(c.Number)-4:]})
		}
		if len(clean) > 0 {
			plain, err := json.Marshal(clean)
			if err != nil {
				return Details{}, err
			}
			if tins.sealed, err = s.seal(workspaceID, "tins", plain); err != nil {
				return Details{}, err
			}
		}
	}
	// An entity has no date of birth: one stored while it was an individual is removed.
	if in.DateOfBirth != nil || d.SellerType == Entity {
		dob.present = true
		if v := strings.TrimSpace(deref(in.DateOfBirth)); v != "" {
			if d.SellerType == Entity {
				return Details{}, invalid("an entity has no date of birth")
			}
			born, err := time.Parse(time.DateOnly, v)
			if err != nil || !born.Before(s.now()) || born.Year() < 1900 {
				return Details{}, invalid("date_of_birth is a past date written YYYY-MM-DD")
			}
			if dob.sealed, err = s.seal(workspaceID, "date_of_birth", []byte(v)); err != nil {
				return Details{}, err
			}
		}
	}
	var last4 string
	if in.AccountIdentifier != nil {
		account.present = true
		if v := compact(*in.AccountIdentifier); v != "" {
			if !accountID.MatchString(v) {
				return Details{}, invalid("account_identifier is an IBAN or an account number: 4 to 34 letters and digits")
			}
			last4 = v[len(v)-4:]
			var err error
			if account.sealed, err = s.seal(workspaceID, "account_identifier", []byte(v)); err != nil {
				return Details{}, err
			}
		}
	}

	if vat := strings.TrimSpace(in.VATNumber); vat != "" {
		if d.Country == "" {
			return Details{}, invalid("give your country to check your VAT number against")
		}
		partner := s.taxes.Tax()
		res, err := partner.ValidateTaxID(partners.WithWorkspace(ctx, workspaceID), d.Country, vat)
		if errors.Is(err, partners.ErrInvalid) {
			return Details{}, invalid("%v", err)
		}
		if err != nil {
			return Details{}, fmt.Errorf("sellertax: check the VAT number: %w", err)
		}
		checked := s.now().UTC()
		d.VATNumber, d.VATValid, d.VATDetail, d.VATPartner, d.VATCheckedAt = res.Number, res.Valid, res.Detail, partner.Name(), &checked
	}

	maskedJSON, err := json.Marshal(masked)
	if err != nil {
		return Details{}, err
	}
	var out Details
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		now := s.now().UTC()
		saved, err := s.scan(tx.QueryRow(ctx, `INSERT INTO seller_tax_profiles (workspace_id, seller_type, first_name, middle_name,
			last_name, legal_name, address, country, tins_sealed, tins_masked, date_of_birth_sealed, company_registration_number,
			vat_number, vat_valid, vat_detail, vat_partner, vat_checked_at, account_identifier_sealed, account_identifier_last4,
			account_holder, self_billing_agreed_version, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
			ON CONFLICT (workspace_id) DO UPDATE SET seller_type = EXCLUDED.seller_type, first_name = EXCLUDED.first_name,
				middle_name = EXCLUDED.middle_name, last_name = EXCLUDED.last_name, legal_name = EXCLUDED.legal_name,
				address = EXCLUDED.address, country = EXCLUDED.country,
				tins_sealed = CASE WHEN $23 THEN EXCLUDED.tins_sealed ELSE seller_tax_profiles.tins_sealed END,
				tins_masked = CASE WHEN $23 THEN EXCLUDED.tins_masked ELSE seller_tax_profiles.tins_masked END,
				date_of_birth_sealed = CASE WHEN $24 THEN EXCLUDED.date_of_birth_sealed ELSE seller_tax_profiles.date_of_birth_sealed END,
				company_registration_number = EXCLUDED.company_registration_number, vat_number = EXCLUDED.vat_number,
				vat_valid = EXCLUDED.vat_valid, vat_detail = EXCLUDED.vat_detail, vat_partner = EXCLUDED.vat_partner,
				vat_checked_at = EXCLUDED.vat_checked_at,
				account_identifier_sealed = CASE WHEN $25 THEN EXCLUDED.account_identifier_sealed ELSE seller_tax_profiles.account_identifier_sealed END,
				account_identifier_last4 = CASE WHEN $25 THEN EXCLUDED.account_identifier_last4 ELSE seller_tax_profiles.account_identifier_last4 END,
				account_holder = EXCLUDED.account_holder, self_billing_agreed_version = EXCLUDED.self_billing_agreed_version,
				updated_at = EXCLUDED.updated_at
			RETURNING `+detailsColumns,
			workspaceID, d.SellerType, d.FirstName, d.MiddleName, d.LastName, d.LegalName, d.Address, d.Country,
			tins.value(), string(maskedJSON), dob.value(), d.CompanyRegistrationNumber, d.VATNumber, d.VATValid, d.VATDetail,
			d.VATPartner, d.VATCheckedAt, account.value(), last4, d.AccountHolder, d.SelfBillingAgreedVersion, now,
			tins.present, dob.present, account.present))
		if err != nil {
			return err
		}
		// Complete details end a hold; incomplete ones after the last request are held again.
		out, err = s.scan(tx.QueryRow(ctx, `UPDATE seller_tax_profiles SET
				completed_at = CASE WHEN $2 THEN $3::timestamptz END,
				withheld_since = CASE WHEN $2 THEN NULL WHEN reminders_sent >= $4 THEN COALESCE(withheld_since, $3) END
			WHERE workspace_id = $1 RETURNING `+detailsColumns, workspaceID, saved.Complete, now, Requests))
		return err
	})
	if err != nil {
		return Details{}, fmt.Errorf("sellertax: save %s: %w", workspaceID, err)
	}
	return out, nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Remind sends every request that is due to a seller with earnings and incomplete details, and starts the payout
// hold the last of them leads to. It answers the sellers it asked, each with which request it was (1 to Requests).
// Without custody nobody is asked, and nobody is held: there is nowhere to give the details.
func (s *Store) Remind(ctx context.Context) (map[string]int, error) {
	sent := map[string]int{}
	if s.ring == nil {
		return sent, nil
	}
	now := s.now().UTC()
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// A seller who has earned and never opened their tax details has a row, so their requests are counted.
		if _, err := tx.Exec(ctx, `INSERT INTO seller_tax_profiles (workspace_id)
			SELECT DISTINCT e.seller_workspace_id FROM market_earnings e
			WHERE NOT EXISTS (SELECT 1 FROM seller_tax_profiles t WHERE t.workspace_id = e.seller_workspace_id)
			ON CONFLICT (workspace_id) DO NOTHING`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `UPDATE seller_tax_profiles t SET reminders_sent = reminders_sent + 1, last_reminded_at = $1,
				withheld_since = CASE WHEN reminders_sent + 1 >= $3 THEN $1 ELSE withheld_since END
			WHERE completed_at IS NULL AND reminders_sent < $3 AND (last_reminded_at IS NULL OR last_reminded_at <= $2)
			  AND EXISTS (SELECT 1 FROM market_earnings e WHERE e.seller_workspace_id = t.workspace_id)
			RETURNING workspace_id, reminders_sent`, now, now.Add(-s.every), Requests)
		if err != nil {
			return err
		}
		for rows.Next() {
			var ws string
			var n int
			if err := rows.Scan(&ws, &n); err != nil {
				rows.Close()
				return err
			}
			sent[ws] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// Details made incomplete again after the last request are held again.
		_, err = tx.Exec(ctx, `UPDATE seller_tax_profiles SET withheld_since = $1
			WHERE completed_at IS NULL AND reminders_sent >= $2 AND withheld_since IS NULL`, now, Requests)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("sellertax: remind sellers: %w", err)
	}
	return sent, nil
}

// BringRemindersDue sends one test seller every request still to come now, as though the reminder interval had passed
// after each (B32.99), so a seller with earnings and incomplete details is withheld exactly as after the last request.
// The caller has checked the workspace is a test one. Without custody, as Remind, nobody is asked or held.
func (s *Store) BringRemindersDue(ctx context.Context, workspaceID string) error {
	if s.ring == nil {
		return nil
	}
	now := s.now().UTC()
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO seller_tax_profiles (workspace_id)
			SELECT $1 WHERE EXISTS (SELECT 1 FROM market_earnings WHERE seller_workspace_id = $1)
			ON CONFLICT (workspace_id) DO NOTHING`, workspaceID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE seller_tax_profiles SET reminders_sent = $3, last_reminded_at = $2, withheld_since = COALESCE(withheld_since, $2)
			WHERE workspace_id = $1 AND completed_at IS NULL
			  AND EXISTS (SELECT 1 FROM market_earnings WHERE seller_workspace_id = $1)`, workspaceID, now, Requests)
		return err
	})
	if err != nil {
		return fmt.Errorf("sellertax: bring %s's requests due: %w", workspaceID, err)
	}
	return nil
}
