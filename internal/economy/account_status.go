package economy

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/passkey"
)

// account_status.go — B30.24: FREEZE, UNFREEZE AND CLOSE AN ACCOUNT.
//
// The owner freezes an agent's or the company's currency account: nothing goes out of it — the ledger refuses the
// debit, so no payment, transfer or hold leaves it — while money coming in still lands. A frozen company account holds
// its agents' sub-accounts with it. The owner unfreezes it, with a passkey once the workspace has one, as an approval
// is decided. Closing pays the balance away first — an agent's into the company's account in its currency, the
// company's to a saved payee through the ordinary payment out — and the account reads closed at zero; a closed
// account leaves room for a new one in the currency (migration 0240).

var (
	// ErrAccountStatus: a freeze, unfreeze or close the account's state does not allow.
	ErrAccountStatus = errors.New("economy: the account's state does not allow this")
	// ErrAccountNotEmpty: a close while the account, or what hangs off it, still holds or moves money.
	ErrAccountNotEmpty = errors.New("economy: the account cannot close yet")
)

// FreezeCurrencyAccount freezes workspaceID's open company or agent account accountID.
func (s *DualTokenStore) FreezeCurrencyAccount(ctx context.Context, workspaceID, accountID string) (CurrencyAccount, error) {
	return s.setAccountStatus(ctx, workspaceID, accountID, MoneyOpen, MoneyFrozen, "", nil)
}

// UnfreezeChallenge issues the challenge an unfreeze of accountID is signed over, and the workspace's passkeys that
// may sign it — none when it has none, and then the unfreeze needs no assertion.
func (s *DualTokenStore) UnfreezeChallenge(ctx context.Context, workspaceID, accountID string) (challenge string, credentialIDs []string, err error) {
	a, err := currencyAccount(ctx, s.pool, workspaceID, accountID)
	if err != nil {
		return "", nil, err
	}
	if a.Status != MoneyFrozen {
		return "", nil, fmt.Errorf("%w: %s is %s, not frozen", ErrAccountStatus, accountID, a.Status)
	}
	keys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return "", nil, err
	}
	credentialIDs = []string{}
	for _, k := range keys {
		credentialIDs = append(credentialIDs, k.CredentialID)
	}
	c, err := s.newChallenge(ctx, workspaceID, "unfreeze:"+accountID)
	return c, credentialIDs, err
}

// UnfreezeCurrencyAccount reopens workspaceID's frozen account accountID: at once for a workspace with no passkey, and
// otherwise only with an assertion by one of its passkeys over the account's unfreeze challenge, which it spends.
func (s *DualTokenStore) UnfreezeCurrencyAccount(ctx context.Context, workspaceID, accountID string, a *passkey.Assertion) (CurrencyAccount, error) {
	return s.setAccountStatus(ctx, workspaceID, accountID, MoneyFrozen, MoneyOpen, "unfreeze:"+accountID, a)
}

// setAccountStatus moves accountID from one state to another, after the passkey gate for purpose when it has one.
// The row is locked, so the change waits for money already moving through the account (money_ledger.go).
func (s *DualTokenStore) setAccountStatus(ctx context.Context, workspaceID, accountID, from, to, purpose string, a *passkey.Assertion) (CurrencyAccount, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CurrencyAccount{}, fmt.Errorf("economy: account status: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM money_accounts WHERE id = $1 AND workspace_id = $2 AND purpose IN ('company', 'agent') FOR UPDATE`,
		accountID, workspaceID).Scan(&status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return CurrencyAccount{}, fmt.Errorf("%w: %s", ErrMoneyAccountNotFound, accountID)
	case err != nil:
		return CurrencyAccount{}, fmt.Errorf("economy: account status: %w", err)
	case status != from:
		return CurrencyAccount{}, fmt.Errorf("%w: %s is %s, not %s", ErrAccountStatus, accountID, status, from)
	}
	if purpose != "" {
		if err := s.requireSigned(ctx, tx, workspaceID, purpose, a); err != nil {
			return CurrencyAccount{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE money_accounts SET status = $2 WHERE id = $1`, accountID, to); err != nil {
		return CurrencyAccount{}, fmt.Errorf("economy: account status: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CurrencyAccount{}, fmt.Errorf("economy: account status: %w", err)
	}
	return currencyAccount(ctx, s.pool, workspaceID, accountID)
}

// CloseCurrencyAccount closes workspaceID's open account accountID at zero. What the company's account holds goes to
// the saved payee payeeID first, through PayOutside, under its own lock; then, with the account's row locked, what an
// agent's account holds goes into the company's account in its currency, and the account closes in the same
// transaction. A frozen account is unfrozen first, and a company account closes only after its agents' sub-accounts
// have and no payment from it is still with the partner.
func (s *DualTokenStore) CloseCurrencyAccount(ctx context.Context, workspaceID, accountID, payeeID string) (CurrencyAccount, error) {
	a, err := currencyAccount(ctx, s.pool, workspaceID, accountID)
	if err != nil {
		return a, err
	}
	switch {
	case a.Status == MoneyClosed:
		return a, fmt.Errorf("%w: %s is already closed", ErrAccountStatus, accountID)
	case a.Status == MoneyFrozen:
		return a, fmt.Errorf("%w: %s is frozen: unfreeze it first", ErrAccountStatus, accountID)
	case a.Purpose == MoneyAgent && payeeID != "":
		return a, fmt.Errorf("%w: an agent's account closes into the company's %s account; pay a payee from there", ErrAccountStatus, a.Currency)
	}
	if err := closeBlockers(ctx, s.pool, accountID); err != nil {
		return a, err
	}
	if a.Purpose == MoneyCompany {
		for _, held := range []struct {
			funding string
			minor   int64
		}{{FundingTest, a.TestMinor}, {FundingLive, a.LiveMinor}} {
			switch {
			case held.minor < 0:
				return a, fmt.Errorf("%w: it is %s overdrawn", ErrAccountNotEmpty, minorString(-held.minor, a.Currency))
			case held.minor == 0:
				continue
			case payeeID == "":
				return a, fmt.Errorf("%w: it holds %s of %s money: name payee_id to pay it to", ErrAccountNotEmpty, minorString(held.minor, a.Currency), held.funding)
			}
			// Under PayOutside's own lock and balance read, so a balance that moved since it was read here pays nothing twice.
			p, err := s.PayOutside(ctx, workspaceID, OutsidePayment{AccountID: accountID, PayeeID: payeeID, AmountMinor: held.minor, Currency: a.Currency,
				Funding: held.funding, Reference: "Account closed", IdempotencyKey: "account_close:" + uuid.NewString()})
			if err != nil {
				return a, err
			}
			if p.Status != PaymentCompleted {
				return a, fmt.Errorf("%w: the payment of %s to %s is %s %s", ErrAccountNotEmpty, minorString(held.minor, a.Currency), p.PayeeName, p.Status, p.Detail)
			}
		}
	}

	// Closed at zero with the row locked: money still moving through it waits, and what arrived as it closed keeps it
	// open. An agent's balance, read under the lock, moves in this transaction.
	// shortcut: the partner account mirroring a company account stays open for reconciliation; close it at the partner
	// when a real account partner closes accounts.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return a, fmt.Errorf("economy: close account: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM money_accounts WHERE id = $1 AND workspace_id = $2 FOR UPDATE`, accountID, workspaceID).Scan(&status); err != nil {
		return a, fmt.Errorf("economy: close account: %w", err)
	}
	if status != MoneyOpen {
		return a, fmt.Errorf("%w: %s is %s", ErrAccountStatus, accountID, status)
	}
	if err := closeBlockers(ctx, tx, accountID); err != nil {
		return a, err
	}
	rows, err := tx.Query(ctx, `SELECT funding, balance_minor FROM money_account_balances WHERE account_id = $1 AND balance_minor <> 0`, accountID)
	if err != nil {
		return a, fmt.Errorf("economy: close account: %w", err)
	}
	held := map[string]int64{}
	for rows.Next() {
		var funding string
		var minor int64
		if err := rows.Scan(&funding, &minor); err != nil {
			rows.Close()
			return a, fmt.Errorf("economy: close account: %w", err)
		}
		held[funding] = minor
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return a, fmt.Errorf("economy: close account: %w", err)
	}
	for _, funding := range []string{FundingTest, FundingLive} {
		minor := held[funding]
		switch {
		case minor < 0:
			return a, fmt.Errorf("%w: it is %s overdrawn", ErrAccountNotEmpty, minorString(-minor, a.Currency))
		case minor == 0:
			continue
		case a.Purpose != MoneyAgent:
			return a, fmt.Errorf("%w: it holds %s of %s money", ErrAccountNotEmpty, minorString(minor, a.Currency), funding)
		}
		if _, err := postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: workspaceID, Capability: CapabilityCurrencyAccounts, Kind: "account_close",
			IdempotencyKey: "account_close:" + accountID + ":" + funding, Funding: funding, Memo: "Account closed", Postings: []MoneyPosting{
				{AccountID: accountID, AmountMinor: -minor}, {AccountID: a.ParentAccountID, AmountMinor: minor}}}, s.screener); err != nil {
			return a, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE money_accounts SET status = 'closed' WHERE id = $1`, accountID); err != nil {
		return a, fmt.Errorf("economy: close account: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return a, fmt.Errorf("economy: close account: %w", err)
	}
	return currencyAccount(ctx, s.pool, workspaceID, accountID)
}

// closeBlockers is why accountID cannot close yet: an agent's sub-account of it still open, or a payment out of it
// still with the partner, whose money would have nowhere to come back to.
func closeBlockers(ctx context.Context, q pgxDB, accountID string) error {
	var open, pending int
	if err := q.QueryRow(ctx, `SELECT (SELECT count(*) FROM money_accounts WHERE parent_account_id = $1 AND status <> 'closed'),
		(SELECT count(*) FROM money_payments WHERE account_id = $1 AND status IN ('pending', 'sent'))`, accountID).Scan(&open, &pending); err != nil {
		return fmt.Errorf("economy: close account: %w", err)
	}
	switch {
	case open > 0:
		return fmt.Errorf("%w: %d of its agents' accounts are open: close them first", ErrAccountNotEmpty, open)
	case pending > 0:
		return fmt.Errorf("%w: %d payments from it are still with the partner: close it once they have completed", ErrAccountNotEmpty, pending)
	}
	return nil
}
