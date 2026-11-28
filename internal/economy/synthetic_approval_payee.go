package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrApprovalNotApproved: the approval is not approved and unused, so there is no approved payment to change.
var ErrApprovalNotApproved = errors.New("economy: this approval is not approved and unused")

// RepointTestPaymentApproval is B62.12: on a test (synthetic) workspace only, the approved and unused approval of an
// agent's payment out is bound to the same payment to another of the workspace's saved payees, as a sheet changed after
// the owner approved it would be, so the testers reach B62.1's stop. What the owner was shown — the payee and the
// signed statement — is kept, so the agent's payment to payeeID is found by the approval and stopped as not the one
// approved. Only the synthetic route calls it. ErrApprovalNotFound for a workspace that is not a test one, an approval
// of another workspace or one that is not of a payment out; ErrPayeeNotFound for a payee not the workspace's;
// ErrApprovalNotApproved for one that is not approved and unused, or whose payment to payeeID already has an approval.
func (s *DualTokenStore) RepointTestPaymentApproval(ctx context.Context, workspaceID, approvalID, payeeID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var p OutsidePayment
		var fingerprint, status, payeeKind, shownPayee, payeeName, memo, paidFrom string
		var amount *int64
		err := tx.QueryRow(ctx, `SELECT a.agent_id, a.fingerprint, a.status, COALESCE(a.payee_kind, ''), COALESCE(a.payee_id, ''),
				COALESCE(a.payee_name, ''), COALESCE(a.memo, ''), COALESCE(a.paid_from_kind, ''), a.paid_from_account_id,
				a.paid_from_amount_minor, a.paid_from_currency, a.paid_from_funding
			FROM agent_approvals a JOIN workspaces w ON w.id = a.workspace_id AND w.synthetic
			WHERE a.id = $1 AND a.workspace_id = $2 FOR UPDATE OF a`, approvalID, workspaceID).
			Scan(&p.AgentID, &fingerprint, &status, &payeeKind, &shownPayee, &payeeName, &memo, &paidFrom, &p.AccountID,
				&amount, &p.Currency, &p.Funding)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrApprovalNotFound
		}
		if err != nil {
			return fmt.Errorf("economy: re-point test approval: %w", err)
		}
		if payeeKind != "outside" || paidFrom != "account" || amount == nil {
			return ErrApprovalNotFound
		}
		// The reference is the end of the memo; the fingerprint it gives back, with the payee shown, proves this
		// approval is of exactly that payment out.
		p.AmountMinor = *amount
		head := paymentOutMemo(p, payeeName)
		switch {
		case memo == head:
		case strings.HasPrefix(memo, head+" — "):
			p.Reference = strings.TrimPrefix(memo, head+" — ")
		default:
			return ErrApprovalNotFound
		}
		if paymentOutFingerprint(p, shownPayee) != fingerprint {
			return ErrApprovalNotFound
		}
		if status != "approved" {
			return ErrApprovalNotApproved
		}
		var saved bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM money_payees WHERE id = $1 AND workspace_id = $2)`, payeeID, workspaceID).
			Scan(&saved); err != nil {
			return fmt.Errorf("economy: re-point test approval: %w", err)
		}
		if !saved {
			return ErrPayeeNotFound
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_approvals SET fingerprint = $2 WHERE id = $1`, approvalID, paymentOutFingerprint(p, payeeID)); err != nil {
			if pe := (*pgconn.PgError)(nil); errors.As(err, &pe) && pe.Code == "23505" {
				return ErrApprovalNotApproved
			}
			return fmt.Errorf("economy: re-point test approval: %w", err)
		}
		return nil
	})
}
