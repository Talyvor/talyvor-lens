package economy

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
)

// money_payees.go — B30.16: SAVED PAYEES OUTSIDE TALYVOR, WITH CONFIRMATION OF PAYEE.
//
// A workspace saves the people and companies outside Talyvor it pays: a name, one set of account details and a
// country. Saving one asks the payee's bank, through the account partner (the Test partner until payments_out is
// cleared), whether the name is the account's, keeps the answer on the payee, and screens the name (B30.6): a name on
// a sanctions list is not saved, and one close to it is saved but waits for the operator. An exact match may be paid
// at once; a close match or no match only once the owner has confirmed it with one of the workspace's passkeys.
// PayableOutsidePayee is the gate a payment out (B30.17) passes through.

// PayeeScreener screens a payee by name when it is saved and before it is paid: *screening.Screener.
type PayeeScreener interface {
	ScreenPayee(ctx context.Context, workspaceID, payeeID, name, capability string) error
}

// SetPayeeScreener is what screens payees (B30.6). Until it is set, no payee is saved or paid.
func (s *DualTokenStore) SetPayeeScreener(sc PayeeScreener) { s.payeeScreener = sc }

var (
	// ErrPayee: a payee without a name, a country or one complete, well-formed set of account details.
	ErrPayee = errors.New("economy: invalid payee")
	// ErrPayeeNotFound: no such payee in this workspace.
	ErrPayeeNotFound = errors.New("economy: no such payee")
	// ErrPayeeCheckUnavailable: the payee's bank could not check the name, so nothing was saved.
	ErrPayeeCheckUnavailable = errors.New("economy: the payee's bank could not check the name against the account")
	// ErrPayeeUnconfirmed: a close match or no match the owner has not yet confirmed.
	ErrPayeeUnconfirmed = errors.New("economy: this payee's name did not match its account: the owner confirms it with a passkey before the first payment")
	// ErrPayeeConfirmed: a payee with nothing left to confirm.
	ErrPayeeConfirmed = errors.New("economy: this payee needs no confirmation")
	// ErrPayeePasskey: a payee is confirmed only with an assertion by one of the workspace's passkeys.
	ErrPayeePasskey = errors.New("economy: a payee is confirmed with one of the workspace's passkeys, signing its challenge — register one first")
)

// OutsidePayee is a saved payee outside Talyvor, and what its bank said of its name.
type OutsidePayee struct {
	ID            string `json:"id"`
	WorkspaceID   string `json:"workspace_id"`
	Name          string `json:"name"`
	Country       string `json:"country"`
	SortCode      string `json:"sort_code,omitempty"`
	AccountNumber string `json:"account_number,omitempty"`
	IBAN          string `json:"iban,omitempty"`
	BIC           string `json:"bic,omitempty"`
	RoutingNumber string `json:"routing_number,omitempty"`
	// Check is exact_match, close_match or no_match; SuggestedName is the holder's name on a close match.
	Check         string    `json:"check"`
	SuggestedName string    `json:"suggested_name,omitempty"`
	CheckedBy     string    `json:"checked_by"` // the partner that answered: "test" for the Test partner
	CheckedAt     time.Time `json:"checked_at"`
	// NeedsConfirmation: a close match or no match not yet confirmed — it cannot be paid.
	NeedsConfirmation bool       `json:"needs_confirmation"`
	ConfirmedAt       *time.Time `json:"confirmed_at,omitempty"`
	ConfirmedBy       string     `json:"confirmed_by,omitempty"` // the passkey that confirmed it
	CreatedAt         time.Time  `json:"created_at"`
	// Screening is set when the name waits for the operator (B30.6); it is answered on save only.
	Screening string `json:"screening,omitempty"`
}

var (
	ibanPattern = regexp.MustCompile(`^[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}$`)
	bicPattern  = regexp.MustCompile(`^[A-Z]{6}[A-Z0-9]{2}([A-Z0-9]{3})?$`)
	allDigits   = regexp.MustCompile(`^[0-9]+$`)
)

// payeeDetails checks p and puts it in its stored form: one set of account details — a sort code and account number,
// an IBAN (and BIC), or a routing and account number — and an ISO 3166-1 alpha-2 country.
func payeeDetails(p OutsidePayee) (OutsidePayee, error) {
	squash := func(v string) string {
		return strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(v)))
	}
	p.Name = strings.Join(strings.Fields(p.Name), " ")
	p.Country = strings.ToUpper(strings.TrimSpace(p.Country))
	p.SortCode, p.AccountNumber, p.IBAN, p.BIC, p.RoutingNumber = squash(p.SortCode), squash(p.AccountNumber), squash(p.IBAN), squash(p.BIC), squash(p.RoutingNumber)
	bad := func(msg string) (OutsidePayee, error) { return OutsidePayee{}, fmt.Errorf("%w: %s", ErrPayee, msg) }
	switch {
	case p.Name == "" || len(p.Name) > 140:
		return bad("a payee has a name, up to 140 characters")
	case p.Country == "":
		return bad("a payee has a country (ISO 3166-1 alpha-2, e.g. GB)")
	}
	if _, err := countryCodes([]string{p.Country}); err != nil {
		return bad(err.Error())
	}
	switch {
	case p.IBAN != "":
		if p.SortCode != "" || p.AccountNumber != "" || p.RoutingNumber != "" {
			return bad("give one set of account details: an IBAN, a sort code and account number, or a routing and account number")
		}
		if !ibanPattern.MatchString(p.IBAN) || !ibanChecks(p.IBAN) {
			return bad(fmt.Sprintf("%q is not a valid IBAN", p.IBAN))
		}
		if p.BIC != "" && !bicPattern.MatchString(p.BIC) {
			return bad(fmt.Sprintf("%q is not a BIC", p.BIC))
		}
	case p.SortCode != "":
		if p.RoutingNumber != "" || p.BIC != "" {
			return bad("give one set of account details: an IBAN, a sort code and account number, or a routing and account number")
		}
		if len(p.SortCode) != 6 || !allDigits.MatchString(p.SortCode) || len(p.AccountNumber) != 8 || !allDigits.MatchString(p.AccountNumber) {
			return bad("a sort code is 6 digits and its account number 8")
		}
		p.SortCode = p.SortCode[:2] + "-" + p.SortCode[2:4] + "-" + p.SortCode[4:]
	case p.RoutingNumber != "":
		if p.BIC != "" {
			return bad("give one set of account details: an IBAN, a sort code and account number, or a routing and account number")
		}
		if len(p.RoutingNumber) != 9 || !allDigits.MatchString(p.RoutingNumber) || len(p.AccountNumber) < 4 || len(p.AccountNumber) > 17 || !allDigits.MatchString(p.AccountNumber) {
			return bad("a routing number is 9 digits and its account number 4 to 17")
		}
	default:
		return bad("a payee has account details: an IBAN, a sort code and account number, or a routing and account number")
	}
	return p, nil
}

// ibanChecks says whether an IBAN's check digits are right (ISO 13616: the number mod 97 is 1).
func ibanChecks(iban string) bool {
	var digits strings.Builder
	for _, r := range iban[4:] + iban[:4] {
		if r >= 'A' && r <= 'Z' {
			fmt.Fprint(&digits, int(r-'A')+10)
		} else {
			digits.WriteRune(r)
		}
	}
	n, ok := new(big.Int).SetString(digits.String(), 10)
	return ok && new(big.Int).Mod(n, big.NewInt(97)).Int64() == 1
}

// CreateOutsidePayee saves a payee for workspaceID: it checks the name against the account at the payee's bank, keeps
// the answer, and screens the name. A name on a sanctions list is refused and nothing is saved.
func (s *DualTokenStore) CreateOutsidePayee(ctx context.Context, workspaceID string, in OutsidePayee) (OutsidePayee, error) {
	p, err := payeeDetails(in)
	if err != nil {
		return p, err
	}
	switch {
	case s.accountPartners == nil:
		return OutsidePayee{}, fmt.Errorf("%w: no account partner is set", ErrPayeeCheckUnavailable)
	case s.payeeScreener == nil:
		return OutsidePayee{}, fmt.Errorf("%w: no screener is set, so no payee is saved", partners.ErrScreeningUnavailable)
	}
	partner, err := s.accountPartners.Account(ctx, CapabilityPaymentsOut)
	if err != nil {
		return OutsidePayee{}, fmt.Errorf("%w: %v", ErrPayeeCheckUnavailable, err)
	}
	check, err := partner.CheckPayee(partners.WithWorkspace(ctx, workspaceID), partners.Payee{Name: p.Name, SortCode: p.SortCode,
		AccountNumber: p.AccountNumber, IBAN: p.IBAN, BIC: p.BIC, RoutingNumber: p.RoutingNumber})
	if err != nil {
		return OutsidePayee{}, fmt.Errorf("%w: %v", ErrPayeeCheckUnavailable, err)
	}
	if check.Result != partners.PayeeExactMatch && check.Result != partners.PayeeCloseMatch && check.Result != partners.PayeeNoMatch {
		return OutsidePayee{}, fmt.Errorf("%w: the partner answered %q", ErrPayeeCheckUnavailable, check.Result)
	}
	p.ID, p.WorkspaceID, p.Check, p.SuggestedName, p.CheckedBy = "payee_"+uuid.NewString(), workspaceID, check.Result, check.SuggestedName, partner.Name()
	held := s.payeeScreener.ScreenPayee(ctx, workspaceID, p.ID, p.Name, CapabilityPaymentsOut)
	if held != nil && !errors.Is(held, screening.ErrHeld) {
		return OutsidePayee{}, held
	}
	saved, err := scanOutsidePayee(s.pool.QueryRow(ctx, `INSERT INTO money_payees (id, workspace_id, name, country, sort_code, account_number,
		iban, bic, routing_number, check_result, suggested_name, checked_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING `+payeeCols, p.ID, workspaceID, p.Name, p.Country, p.SortCode, p.AccountNumber, p.IBAN, p.BIC, p.RoutingNumber,
		p.Check, p.SuggestedName, p.CheckedBy))
	if err != nil {
		return OutsidePayee{}, fmt.Errorf("economy: save payee: %w", err)
	}
	if held != nil {
		saved.Screening = held.Error()
	}
	return saved, nil
}

const payeeCols = `id, workspace_id, name, country, sort_code, account_number, iban, bic, routing_number, check_result, suggested_name,
	checked_by, checked_at, confirmed_at, confirmed_by, created_at`

func scanOutsidePayee(row pgx.Row) (OutsidePayee, error) {
	var p OutsidePayee
	err := row.Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Country, &p.SortCode, &p.AccountNumber, &p.IBAN, &p.BIC, &p.RoutingNumber,
		&p.Check, &p.SuggestedName, &p.CheckedBy, &p.CheckedAt, &p.ConfirmedAt, &p.ConfirmedBy, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrPayeeNotFound
	}
	p.NeedsConfirmation = p.Check != partners.PayeeExactMatch && p.ConfirmedAt == nil
	return p, err
}

// OutsidePayees is workspaceID's saved payees, oldest first.
func (s *DualTokenStore) OutsidePayees(ctx context.Context, workspaceID string) ([]OutsidePayee, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+payeeCols+` FROM money_payees WHERE workspace_id = $1 ORDER BY created_at, id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: payees: %w", err)
	}
	defer rows.Close()
	out := []OutsidePayee{}
	for rows.Next() {
		p, err := scanOutsidePayee(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PayeesNamed is workspaceID's saved payees whose name holds name as whole words, in any case ("mike" finds "Mike
// Jones", never "Mikel"), oldest first (B45.14).
func (s *DualTokenStore) PayeesNamed(ctx context.Context, workspaceID, name string) ([]OutsidePayee, error) {
	q := " " + strings.ToLower(strings.Join(strings.Fields(name), " ")) + " "
	if strings.TrimSpace(q) == "" {
		return nil, fmt.Errorf("%w: name the saved contact", ErrPayee)
	}
	// shortcut: reads every payee of the workspace; a SQL filter when a workspace keeps thousands.
	all, err := s.OutsidePayees(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := []OutsidePayee{}
	for _, p := range all {
		if strings.Contains(" "+strings.ToLower(p.Name)+" ", q) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *DualTokenStore) outsidePayee(ctx context.Context, q pgxDB, workspaceID, payeeID, lock string) (OutsidePayee, error) {
	return scanOutsidePayee(q.QueryRow(ctx, `SELECT `+payeeCols+` FROM money_payees WHERE id = $1 AND workspace_id = $2`+lock, payeeID, workspaceID))
}

// PayeeChallenge issues the challenge confirming payeeID is signed over, and the workspace's passkeys that may sign it.
func (s *DualTokenStore) PayeeChallenge(ctx context.Context, workspaceID, payeeID string) (challenge string, credentialIDs []string, err error) {
	p, err := s.outsidePayee(ctx, s.pool, workspaceID, payeeID, "")
	if err != nil {
		return "", nil, err
	}
	if !p.NeedsConfirmation {
		return "", nil, ErrPayeeConfirmed
	}
	keys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return "", nil, err
	}
	if len(keys) == 0 {
		return "", nil, ErrPayeePasskey
	}
	for _, k := range keys {
		credentialIDs = append(credentialIDs, k.CredentialID)
	}
	c, err := s.newChallenge(ctx, workspaceID, "payee:"+payeeID)
	return c, credentialIDs, err
}

// ConfirmOutsidePayee confirms a close match or no match with an assertion by one of the workspace's passkeys over
// the payee's challenge, which it spends. From then on the payee may be paid.
func (s *DualTokenStore) ConfirmOutsidePayee(ctx context.Context, workspaceID, payeeID string, a *passkey.Assertion) (OutsidePayee, error) {
	if a == nil {
		return OutsidePayee{}, ErrPayeePasskey
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OutsidePayee{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	p, err := s.outsidePayee(ctx, tx, workspaceID, payeeID, " FOR UPDATE")
	if err != nil {
		return p, err
	}
	if !p.NeedsConfirmation {
		return p, ErrPayeeConfirmed
	}
	if err := s.spendAssertion(ctx, tx, workspaceID, "payee:"+payeeID, *a); err != nil {
		return OutsidePayee{}, err
	}
	p, err = scanOutsidePayee(tx.QueryRow(ctx, `UPDATE money_payees SET confirmed_at = now(), confirmed_by = $3
		WHERE id = $1 AND workspace_id = $2 RETURNING `+payeeCols, payeeID, workspaceID, a.CredentialID))
	if err != nil {
		return p, err
	}
	return p, tx.Commit(ctx)
}

// PayableOutsidePayee is workspaceID's payee when it may be paid: its name matched its account or the owner confirmed
// it, and screening lets it through. A payment out pays only a payee this returns.
func (s *DualTokenStore) PayableOutsidePayee(ctx context.Context, workspaceID, payeeID string) (OutsidePayee, error) {
	p, err := s.outsidePayee(ctx, s.pool, workspaceID, payeeID, "")
	switch {
	case err != nil:
		return p, err
	case p.NeedsConfirmation:
		return p, ErrPayeeUnconfirmed
	case s.payeeScreener == nil:
		return p, fmt.Errorf("%w: no screener is set, so no payee is paid", partners.ErrScreeningUnavailable)
	}
	return p, s.payeeScreener.ScreenPayee(ctx, workspaceID, p.ID, p.Name, CapabilityPaymentsOut)
}
