package economy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
)

// money_payee_details.go — B41.112: CHANGING A SAVED PAYEE'S BANK DETAILS, LOCKED.
//
// Changed bank details are the main invoice-fraud route. So a change takes an assertion by one of the workspace's
// passkeys over a challenge issued for exactly those new details; it asks the payee's bank afresh whether the name is
// the new account's (a close match or no match is confirmed again before it is paid); and it holds every payment to the
// payee for PayeeDetailsHold, the time to call them on a number the owner already knows.

// PayeeDetailsHold is how long every payment to a payee is held after its bank details change.
const PayeeDetailsHold = 24 * time.Hour

// PayeeDetailsNotice is what the owner is told while a changed payee is held.
const PayeeDetailsNotice = "Call them on a number you already know — not one from the message that asked for the change — to check the new details are theirs."

// ErrPayeeDetailsHeld: the payee's bank details changed less than PayeeDetailsHold ago, so the payment is held.
var ErrPayeeDetailsHeld = errors.New("economy: this payee's bank details changed less than 24 hours ago, so payments to it are held")

// payeeHeld is ErrPayeeDetailsHeld until a time. It is ErrPayeeUnconfirmed too, so every payment path that refuses a
// payee not yet confirmed refuses a held one the same way.
type payeeHeld struct{ until time.Time }

func (e payeeHeld) Error() string {
	return fmt.Sprintf("%s until %s. %s", ErrPayeeDetailsHeld, e.until.UTC().Format(time.RFC3339), PayeeDetailsNotice)
}

func (e payeeHeld) Is(target error) bool {
	return target == ErrPayeeDetailsHeld || target == ErrPayeeUnconfirmed
}

// changedPayeeDetails is cur with in's account details (and country, when in gives one), checked and in stored form.
func changedPayeeDetails(cur, in OutsidePayee) (OutsidePayee, error) {
	next := OutsidePayee{ID: cur.ID, WorkspaceID: cur.WorkspaceID, Name: cur.Name, Country: in.Country, SortCode: in.SortCode,
		AccountNumber: in.AccountNumber, IBAN: in.IBAN, BIC: in.BIC, RoutingNumber: in.RoutingNumber, Network: in.Network, Address: in.Address}
	if strings.TrimSpace(next.Country) == "" {
		next.Country = cur.Country
	}
	next, err := payeeDetails(next)
	if err != nil {
		return next, err
	}
	if payeeAccount(next) == payeeAccount(cur) {
		return next, fmt.Errorf("%w: these are the payee's details already", ErrPayee)
	}
	return next, nil
}

func payeeAccount(p OutsidePayee) string {
	return strings.Join([]string{p.Country, p.SortCode, p.AccountNumber, p.IBAN, p.BIC, p.RoutingNumber, p.Network, p.Address}, "|")
}

// payeeDetailsPurpose binds a challenge to one payee and the exact details it is changed to, so an assertion signed
// for one set of details cannot change it to another.
func payeeDetailsPurpose(p OutsidePayee) string {
	h := sha256.Sum256([]byte(payeeAccount(p)))
	return "payee_details:" + p.ID + ":" + hex.EncodeToString(h[:])
}

// PayeeDetailsChallenge issues the challenge that changing payeeID's details to in is signed over, and the
// workspace's passkeys that may sign it.
func (s *DualTokenStore) PayeeDetailsChallenge(ctx context.Context, workspaceID, payeeID string, in OutsidePayee) (challenge string, credentialIDs []string, err error) {
	cur, err := s.outsidePayee(ctx, s.pool, workspaceID, payeeID, "")
	if err != nil {
		return "", nil, err
	}
	next, err := changedPayeeDetails(cur, in)
	if err != nil {
		return "", nil, err
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
	c, err := s.newChallenge(ctx, workspaceID, payeeDetailsPurpose(next))
	return c, credentialIDs, err
}

// ChangeOutsidePayeeDetails changes payeeID's account details to in's, with an assertion by one of the workspace's
// passkeys over PayeeDetailsChallenge's challenge for those details, which it spends. The name is checked afresh at the
// new account's bank — a close match or no match needs confirming again — and every payment to the payee is held for
// PayeeDetailsHold.
func (s *DualTokenStore) ChangeOutsidePayeeDetails(ctx context.Context, workspaceID, payeeID string, in OutsidePayee, a *passkey.Assertion) (OutsidePayee, error) {
	if a == nil || a.DeviceID != "" {
		return OutsidePayee{}, ErrPayeePasskey
	}
	if s.accountPartners == nil {
		return OutsidePayee{}, fmt.Errorf("%w: no account partner is set", ErrPayeeCheckUnavailable)
	}
	cur, err := s.outsidePayee(ctx, s.pool, workspaceID, payeeID, "")
	if err != nil {
		return cur, err
	}
	next, err := changedPayeeDetails(cur, in)
	if err != nil {
		return OutsidePayee{}, err
	}
	if next, err = s.checkPayeeName(ctx, workspaceID, next); err != nil {
		return OutsidePayee{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OutsidePayee{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := s.outsidePayee(ctx, tx, workspaceID, payeeID, " FOR UPDATE"); err != nil {
		return OutsidePayee{}, err
	}
	if err := s.spendAssertion(ctx, tx, workspaceID, payeeDetailsPurpose(next), *a); err != nil {
		return OutsidePayee{}, err
	}
	p, err := scanOutsidePayee(tx.QueryRow(ctx, `UPDATE money_payees SET country = $3, sort_code = $4, account_number = $5, iban = $6,
		bic = $7, routing_number = $8, network = $9, address = $10, check_result = $11, suggested_name = $12, checked_by = $13,
		checked_at = now(), confirmed_at = NULL, confirmed_by = '', details_changed_at = now()
		WHERE id = $1 AND workspace_id = $2 RETURNING `+payeeCols, payeeID, workspaceID, next.Country, next.SortCode, next.AccountNumber,
		next.IBAN, next.BIC, next.RoutingNumber, next.Network, next.Address, next.Check, next.SuggestedName, next.CheckedBy))
	if err != nil {
		return p, err
	}
	return p, tx.Commit(ctx)
}

// checkPayeeName is the check CreateOutsidePayee makes on save: it asks p's bank, through the account partner, whether
// p's name is its account's, and puts the answer on p; a USDC address has no bank to ask and is PayeeAddress.
func (s *DualTokenStore) checkPayeeName(ctx context.Context, workspaceID string, p OutsidePayee) (OutsidePayee, error) {
	p.Check, p.SuggestedName, p.CheckedBy = PayeeAddress, "", ""
	if p.Address != "" {
		return p, nil
	}
	partner, err := s.accountPartners.Account(ctx, CapabilityPaymentsOut)
	if err != nil {
		return p, fmt.Errorf("%w: %v", ErrPayeeCheckUnavailable, err)
	}
	check, err := partner.CheckPayee(partners.WithWorkspace(ctx, workspaceID), partners.Payee{Name: p.Name, SortCode: p.SortCode,
		AccountNumber: p.AccountNumber, IBAN: p.IBAN, BIC: p.BIC, RoutingNumber: p.RoutingNumber})
	if err != nil {
		return p, fmt.Errorf("%w: %v", ErrPayeeCheckUnavailable, err)
	}
	if check.Result != partners.PayeeExactMatch && check.Result != partners.PayeeCloseMatch && check.Result != partners.PayeeNoMatch {
		return p, fmt.Errorf("%w: the partner answered %q", ErrPayeeCheckUnavailable, check.Result)
	}
	p.Check, p.SuggestedName, p.CheckedBy = check.Result, check.SuggestedName, partner.Name()
	return p, nil
}
