package economy

import (
	"context"
	"errors"
	"fmt"
)

// identity_gate.go — B61.52: AN ACCOUNT THAT HAS NOT CONFIRMED WHO IT IS MOVES NO MONEY, SANDBOX MONEY INCLUDED.
//
// An account confirms who it is by reaching the level its type needs (NeededLevel): a Personal account its owner's
// identity, L2; a Business its company too, L3 — as the apps' neededLevel has it. Until then it can look — balances,
// statements, rules, agents, settings and the identity check itself — and every AMBER or RED capability refuses its
// money, test or live, with one sentence (ConfirmIdentitySentence). Sandbox money is judged by the level shown, which a
// Test-provider pass reaches; live money stays judged by the live level as well (levelRefusal, unchanged).
//
// The gate stands beside the freeze's and the terms' wherever a capability is asked for the account that moves the
// money: PostMoney, credits spent on a capability (spendForCapability), a Stripe bill (requireBilledCapability), a
// currency account and its details, a payment to another owner's agent, a loan and an on-chain send. A transfer or payment
// between agents is asked whatever its class, one owner's own agents included (transferTx, payAgentTx). The other GREEN capabilities —
// Talyvor's own services — are not asked here; the money routes refuse an unconfirmed account at the door any
// movement of money they make (cmd/lens identityGate). Talyvor's own workspace, which pays the
// marketplace's sellers, is no person and is never asked.

// ConfirmIdentitySentence is the one refusal an unconfirmed account gets for any movement of money.
const ConfirmIdentitySentence = "Confirm who you are to move money."

// ErrIdentityUnconfirmed: the account has not reached the level its type needs, so it moves no money.
var ErrIdentityUnconfirmed = errors.New("economy: refused: confirm who you are to move money")

// IdentityNeeded is where an unconfirmed account stands: the level its type needs, and the one it has.
type IdentityNeeded struct {
	AccountType string            `json:"account_type"`
	Needed      VerificationLevel `json:"level_needed"`
	Have        VerificationLevel `json:"level"`
}

// NeededLevel is the level that confirms an account of type accountType: a person's identity, and for a Business its
// company too.
func NeededLevel(accountType string) VerificationLevel {
	if accountType == AccountTypePersonal {
		return LevelIdentity
	}
	return LevelCompany
}

// identityUnconfirmed is where workspaceID stands when it has not confirmed who it is, and nil when it has. A
// workspace with no row is what the column's default makes it, a Business.
func identityUnconfirmed(ctx context.Context, q pgxDB, workspaceID string) (*IdentityNeeded, error) {
	if workspaceID == TalyvorWorkspace {
		return nil, nil
	}
	var accountType string
	if err := q.QueryRow(ctx, `SELECT COALESCE((SELECT account_type FROM workspaces WHERE id = $1), $2)`,
		workspaceID, AccountTypeBusiness).Scan(&accountType); err != nil {
		return nil, fmt.Errorf("economy: identity gate: %w", err)
	}
	checks, err := readVerificationChecks(ctx, q, workspaceID)
	if err != nil {
		return nil, err
	}
	have, _ := reachedLevels(checks)
	if need := NeededLevel(accountType); have < need {
		return &IdentityNeeded{AccountType: accountType, Needed: need, Have: have}, nil
	}
	return nil, nil
}

// IdentityUnconfirmed is where workspaceID stands when it has not confirmed who it is, and nil when it has: the money
// routes ask it before any movement (cmd/lens identityGate).
func (s *DualTokenStore) IdentityUnconfirmed(ctx context.Context, workspaceID string) (*IdentityNeeded, error) {
	return identityUnconfirmed(ctx, s.pool, workspaceID)
}

// refuseUnconfirmed refuses capability c's money for workspaceID until it has confirmed who it is — c AMBER or RED,
// test money or live — and answers nil for a GREEN one. Money arriving from outside (payments_in) is not the account
// moving money: it arrives at details only a confirmed account is given, or comes back from a payment it made, and
// refusing it would leave it at the partner.
func refuseUnconfirmed(ctx context.Context, q pgxDB, workspaceID string, c Capability) error {
	if c.Class == ClassGreen || c.Key == CapabilityPaymentsIn {
		return nil
	}
	n, err := identityUnconfirmed(ctx, q, workspaceID)
	if err != nil || n == nil {
		return err
	}
	return &CapabilityRefusal{Capability: c, Identity: n}
}

// refuseUnconfirmedAnyClass refuses c's money for workspaceID until it has confirmed who it is whatever c's class: a
// transfer between agents, one owner's own included, is a movement of money however it is asked — through a route,
// the connector or a schedule.
func refuseUnconfirmedAnyClass(ctx context.Context, q pgxDB, workspaceID string, c Capability) error {
	n, err := identityUnconfirmed(ctx, q, workspaceID)
	if err != nil || n == nil {
		return err
	}
	return &CapabilityRefusal{Capability: c, Identity: n}
}
