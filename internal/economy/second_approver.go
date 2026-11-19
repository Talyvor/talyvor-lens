package economy

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/passkey"
)

// second_approver.go — B28.315: ABOVE A SECOND, HIGHER AMOUNT A BUSINESS'S PAYMENT NEEDS TWO APPROVERS (launch pack 3.4).
//
// A Business workspace names its second approver — a person, by their user id — and the amount above which a payment
// needs them, in one currency. An agent's approval worth more than that line is approved by two different people, one
// of them the second approver: the first approval leaves it pending ("Approved by you. Waiting for a second approver."),
// so a retry still waits and moves nothing; the second, by the other person, approves it, and the retry goes through
// once, as any approved request does. The same person approving twice is refused, and so is a pair in which neither is
// the second approver. An approver agent (B45.28) never approves above the line. An approval is valued in the line's
// currency rounded up; one whose value cannot be found counts as above it. A Personal workspace has none (ErrNotBusiness).

// SecondApprover is a Business workspace's second-approver line: who, and the amount above which a payment needs them.
type SecondApprover struct {
	UserID     string     `json:"user_id"`     // "" with no second approver
	AboveMinor int64      `json:"above_minor"` // in Currency's minor units; 0 with no second approver
	Currency   string     `json:"currency"`    // GBP, EUR, USD or USDC; "" with no second approver
	SetBy      string     `json:"set_by,omitempty"`
	SetAt      *time.Time `json:"set_at,omitempty"`
}

func (r SecondApprover) set() bool { return r.UserID != "" && r.AboveMinor > 0 }

var (
	// ErrSecondApproverInvalid: a second-approver line without a person, an amount above 0 or a currency.
	ErrSecondApproverInvalid = errors.New("economy: invalid second approver")
	// ErrSameApprover: the person who gave an approval's first approval tried to give its second.
	ErrSameApprover = errors.New("economy: approved by you; waiting for a second approver")
	// ErrSecondApproverNeeded: two people approved a payment over the line, and neither is the second approver.
	ErrSecondApproverNeeded = errors.New("economy: this payment is over the second approver line, so the second approver must be one of its two approvers")
	// ErrApproverUnknown: an approval over the line decided by someone Lens cannot tell apart from another approver.
	ErrApproverUnknown = errors.New("economy: this payment is over the second approver line, and only a signed-in person's approval counts towards its two")
)

type approverKey struct{}

// WithApprover carries who decides an approval to the decisions: the signed-in person's user id, or else how they
// signed in, as storedanswers.OwnerOrAdmin names it. Two approvals of one payment count only from two of them.
func WithApprover(ctx context.Context, userID, who string) context.Context {
	if userID != "" {
		who = "user:" + userID
	}
	return context.WithValue(ctx, approverKey{}, who)
}

func approverFrom(ctx context.Context) string {
	who, _ := ctx.Value(approverKey{}).(string)
	return who
}

func scanSecondApprover(row pgx.Row) (accountType string, r SecondApprover, err error) {
	err = row.Scan(&accountType, &r.UserID, &r.AboveMinor, &r.Currency, &r.SetBy, &r.SetAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", r, ErrWorkspaceNotFound
	}
	if err != nil {
		return "", r, fmt.Errorf("economy: second approver: %w", err)
	}
	return accountType, r, nil
}

const secondApproverColumns = `account_type, second_approver_user_id, second_approval_above_minor, second_approval_currency,
	second_approver_set_by, second_approver_set_at`

// SecondApprover reads a Business workspace's second-approver line; empty when none is set.
func (s *DualTokenStore) SecondApprover(ctx context.Context, workspaceID string) (SecondApprover, error) {
	kind, r, err := scanSecondApprover(s.pool.QueryRow(ctx, `SELECT `+secondApproverColumns+` FROM workspaces WHERE id = $1`, workspaceID))
	if err == nil && kind != AccountTypeBusiness {
		return SecondApprover{}, ErrNotBusiness
	}
	return r, err
}

// checkSecondApprover refuses a line that is neither cleared (no person, amount 0) nor whole.
func checkSecondApprover(in SecondApprover) (SecondApprover, error) {
	out := SecondApprover{UserID: in.UserID, AboveMinor: in.AboveMinor, Currency: in.Currency}
	switch {
	case out.UserID == "" && out.AboveMinor == 0:
		return SecondApprover{}, nil
	case out.UserID == "":
		return out, fmt.Errorf("%w: name the person, by user_id", ErrSecondApproverInvalid)
	case out.AboveMinor <= 0:
		return out, fmt.Errorf("%w: above_minor must be above 0", ErrSecondApproverInvalid)
	}
	if _, ok := MoneyCurrencies[out.Currency]; !ok {
		return out, fmt.Errorf("%w: currency must be GBP, EUR, USD or USDC", ErrSecondApproverInvalid)
	}
	return out, nil
}

func secondApproverPurpose(r SecondApprover) string {
	return "second_approver:" + r.UserID + ":" + strconv.FormatInt(r.AboveMinor, 10) + ":" + r.Currency
}

// SecondApproverChallenge issues the challenge the owner signs to set in, or to clear the line with an empty one, and
// the workspace's passkeys.
func (s *DualTokenStore) SecondApproverChallenge(ctx context.Context, workspaceID string, in SecondApprover) (string, []string, error) {
	in, err := checkSecondApprover(in)
	if err != nil {
		return "", nil, err
	}
	if _, err := s.SecondApprover(ctx, workspaceID); err != nil {
		return "", nil, err
	}
	keys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return "", nil, err
	}
	creds := []string{}
	for _, k := range keys {
		creds = append(creds, k.CredentialID)
	}
	c, err := s.newChallenge(ctx, workspaceID, secondApproverPurpose(in))
	return c, creds, err
}

// SetSecondApprover sets a Business workspace's second-approver line, or clears it with an empty one, signed by the
// owner with a over SecondApproverChallenge's challenge once the workspace has a passkey or device key. by is who set it.
// It holds for approvals still waiting as well as new ones.
func (s *DualTokenStore) SetSecondApprover(ctx context.Context, workspaceID, by string, in SecondApprover, a *passkey.Assertion) (SecondApprover, error) {
	in, err := checkSecondApprover(in)
	if err != nil {
		return in, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return in, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	kind, _, err := scanSecondApprover(tx.QueryRow(ctx, `SELECT `+secondApproverColumns+` FROM workspaces WHERE id = $1 FOR UPDATE`, workspaceID))
	if err != nil {
		return in, err
	}
	if kind != AccountTypeBusiness {
		return in, ErrNotBusiness
	}
	if err := s.authorizeSignedTx(ctx, tx, workspaceID, secondApproverPurpose(in), a, ErrPasskeyRequired); err != nil {
		return in, err
	}
	_, out, err := scanSecondApprover(tx.QueryRow(ctx, `UPDATE workspaces SET second_approver_user_id = $2, second_approval_above_minor = $3,
		second_approval_currency = $4, second_approver_set_by = $5, second_approver_set_at = now() WHERE id = $1 RETURNING `+secondApproverColumns,
		workspaceID, in.UserID, in.AboveMinor, in.Currency, by))
	if err != nil {
		return in, err
	}
	return out, tx.Commit(ctx)
}

// overSecondLine is whether approval ap, of workspaceID, needs two approvers under the line in force, read in q, and
// the line.
func (s *DualTokenStore) overSecondLine(ctx context.Context, q pgxDB, workspaceID string, ap AgentApproval) (bool, SecondApprover, error) {
	kind, line, err := scanSecondApprover(q.QueryRow(ctx, `SELECT `+secondApproverColumns+` FROM workspaces WHERE id = $1`, workspaceID))
	if errors.Is(err, ErrWorkspaceNotFound) {
		return false, line, nil
	}
	if err != nil || kind != AccountTypeBusiness || !line.set() {
		return false, line, err
	}
	var value int64
	if p := ap.PaidFrom; p != nil && MoneyCurrencies[p.Currency] > 0 {
		value, err = s.convertMinor(ctx, workspaceID, p.AmountMinor, p.Currency, line.Currency, true)
	} else {
		value, err = s.ulxcIn(ctx, workspaceID, ap.AmountULXC, line.Currency, true)
	}
	if err != nil {
		return true, line, nil // a payment that cannot be valued in the line's currency counts as over it
	}
	return value > line.AboveMinor, line, nil
}

// secondApproval runs inside tx before approvalID, pending, is approved by the person in ctx. Over the line, it records
// the first approval and returns waiting true with the approval as it now reads, still pending; for the second, it checks
// the two are different people and one of them the second approver, and records who. Under the line, or for an approval
// that is not pending, it does nothing.
func (s *DualTokenStore) secondApproval(ctx context.Context, tx pgx.Tx, workspaceID, approvalID string) (bool, AgentApproval, error) {
	ap, err := scanAgentApproval(tx.QueryRow(ctx, `SELECT `+agentApprovalColumns+` FROM agent_approvals
		WHERE id = $1 AND workspace_id = $2 AND status = 'pending' AND expires_at > now() AND kind = '' FOR UPDATE`, approvalID, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ap, nil
	}
	if err != nil {
		return false, ap, fmt.Errorf("economy: approval: %w", err)
	}
	over, line, err := s.overSecondLine(ctx, tx, workspaceID, ap)
	if err != nil || !over {
		return false, ap, err
	}
	who := approverFrom(ctx)
	switch {
	case who == "":
		return false, ap, ErrApproverUnknown
	case ap.FirstApprovedBy == "":
		ap, err = scanAgentApproval(tx.QueryRow(ctx, `UPDATE agent_approvals SET first_approved_by = $2, first_approved_at = now()
			WHERE id = $1 RETURNING `+agentApprovalColumns, approvalID, who))
		if err != nil {
			return false, ap, fmt.Errorf("economy: first approval: %w", err)
		}
		return true, ap, nil
	case ap.FirstApprovedBy == who:
		return false, ap, ErrSameApprover
	case ap.FirstApprovedBy != "user:"+line.UserID && who != "user:"+line.UserID:
		return false, ap, ErrSecondApproverNeeded
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_approvals SET second_approved_by = $2 WHERE id = $1`, approvalID, who); err != nil {
		return false, ap, fmt.Errorf("economy: second approval: %w", err)
	}
	return false, ap, nil
}
