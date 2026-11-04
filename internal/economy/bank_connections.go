package economy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/partners"
)

// bank_connections.go — B53.13: PAYMENTS MADE WITH OTHER BANKS' CARDS, THROUGH OPEN BANKING.
//
// The owner connects an account they hold at another bank, one account per consent, through the open-banking partner
// (partners.AccountInfoPartner). Lens reads its balance and transactions: when it connects, when the owner asks, and in
// the background no more often than BankBackgroundRefreshEvery, because a bank may allow only four reads a day without
// the person there. Every transaction read carries its bank and when it was read, so spending shows it as of that
// read, never as instant. The money out counts in spending (B53.3) on the owner's own wallet, so it gets its category
// and counts in budgets and unusual spending like any card charge. A consent runs out: within BankConsentRemindBefore
// of that the connection says its renewal is due. Disconnecting ends the consent and deletes everything read.

// BankBackgroundRefreshEvery is the least time between two background reads of one account: four a day.
const BankBackgroundRefreshEvery = 24 * time.Hour / 4

// bankHistoryDays is how far back the first read of an account goes.
const bankHistoryDays = 90

// BankConsentRemindBefore is how long before its consent runs out a connection says its renewal is due
// (LENS_BANK_CONSENT_REMIND_DAYS).
var BankConsentRemindBefore = 7 * 24 * time.Hour

var (
	// ErrBankConnection is a connection that cannot be made or used as asked.
	ErrBankConnection = errors.New("economy: bank connection")
	// ErrBankConnectionNotFound is a connection that is not the workspace's.
	ErrBankConnectionNotFound = errors.New("economy: no such bank connection")
	// ErrBankInfoUnavailable is no open-banking partner for this workspace yet.
	ErrBankInfoUnavailable = errors.New("economy: connecting an account at another bank is not available yet")
)

// SetBankInfo is the open-banking partner for a workspace, by whether it is a tester's: partners.Registry.AccountInfo.
func (s *DualTokenStore) SetBankInfo(p func(synthetic bool) partners.AccountInfoPartner) {
	s.bankInfo = p
}

// BankConnection is one account at another bank Lens reads.
type BankConnection struct {
	ID               string     `json:"id"`
	Provider         string     `json:"provider"`
	Bank             string     `json:"bank"`
	Account          string     `json:"account"`
	Currency         string     `json:"currency"`
	ConsentExpiresAt time.Time  `json:"consent_expires_at"`
	RenewalDue       bool       `json:"renewal_due"`
	BalanceMinor     *int64     `json:"balance_minor,omitempty"`
	BalanceAsOf      *time.Time `json:"balance_as_of,omitempty"`
	RefreshedAt      *time.Time `json:"refreshed_at,omitempty"` // the last read: what is shown is as of this
	Transactions     int        `json:"transactions"`
	CreatedAt        time.Time  `json:"created_at"`
}

func (s *DualTokenStore) bankPartner(ctx context.Context, workspaceID string) (partners.AccountInfoPartner, error) {
	if s.bankInfo == nil {
		return nil, ErrBankInfoUnavailable
	}
	synthetic, err := syntheticWorkspace(ctx, s.pool, workspaceID)
	if err != nil {
		return nil, err
	}
	p := s.bankInfo(synthetic)
	if p == nil {
		return nil, ErrBankInfoUnavailable
	}
	return p, nil
}

// ConnectBank asks the bank, through the partner, for the owner's consent to read account, then reads it.
func (s *DualTokenStore) ConnectBank(ctx context.Context, workspaceID, bank, account, by string) (BankConnection, error) {
	bank, account = strings.TrimSpace(bank), strings.TrimSpace(account)
	if bank == "" || account == "" || len(bank) > 100 || len(account) > 100 {
		return BankConnection{}, fmt.Errorf("%w: name the bank and the account (at most 100 characters each)", ErrBankConnection)
	}
	p, err := s.bankPartner(ctx, workspaceID)
	if err != nil {
		return BankConnection{}, err
	}
	id := "bnk_" + uuid.NewString()
	c, err := p.Consent(ctx, partners.ConsentRequest{ID: id, Bank: bank, Account: account})
	if err != nil {
		return BankConnection{}, fmt.Errorf("economy: bank consent: %w", err)
	}
	if c.Status != partners.StatusCompleted {
		return BankConnection{}, fmt.Errorf("%w: %s did not give consent (%s) %s", ErrBankConnection, bank, c.Status, c.Detail)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO bank_connections (id, workspace_id, provider, bank, account, currency, consent_ref, consent_expires_at, connected_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, id, workspaceID, p.Name(), bank, account, c.Currency, c.Ref, c.ExpiresAt, by)
	if err != nil {
		_ = p.Revoke(ctx, c.Ref)
		if pe := (*pgconn.PgError)(nil); errors.As(err, &pe) && pe.Code == "23505" {
			return BankConnection{}, fmt.Errorf("%w: %s at %s is already connected", ErrBankConnection, account, bank)
		}
		return BankConnection{}, fmt.Errorf("economy: bank connection: %w", err)
	}
	return s.RefreshBank(ctx, workspaceID, id)
}

// RefreshBank reads connection id's balance and its transactions since the last one read.
// shortcut: reads from the newest booked date, so a transaction the bank books later with an earlier date is missed;
// read again from a few days back once a real partner shows that happens.
func (s *DualTokenStore) RefreshBank(ctx context.Context, workspaceID, id string) (BankConnection, error) {
	p, err := s.bankPartner(ctx, workspaceID)
	if err != nil {
		return BankConnection{}, err
	}
	var ref string
	var since time.Time
	err = s.pool.QueryRow(ctx, `SELECT c.consent_ref, COALESCE((SELECT max(t.booked_at) FROM bank_transactions t WHERE t.connection_id = c.id), $3)
		FROM bank_connections c WHERE c.id = $1 AND c.workspace_id = $2`, id, workspaceID, time.Now().UTC().AddDate(0, 0, -bankHistoryDays)).Scan(&ref, &since)
	if errors.Is(err, pgx.ErrNoRows) {
		return BankConnection{}, ErrBankConnectionNotFound
	}
	if err != nil {
		return BankConnection{}, fmt.Errorf("economy: bank refresh: %w", err)
	}
	bal, err := p.Balance(ctx, ref)
	if err != nil {
		return BankConnection{}, fmt.Errorf("%w: reading the balance: %v", ErrBankConnection, err)
	}
	txs, err := p.Transactions(ctx, ref, since)
	if err != nil {
		return BankConnection{}, fmt.Errorf("%w: reading the transactions: %v", ErrBankConnection, err)
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, t := range txs {
			if _, err := tx.Exec(ctx, `INSERT INTO bank_transactions (id, workspace_id, connection_id, external_id, amount_minor, currency, merchant, mcc, booked_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (connection_id, external_id) DO NOTHING`,
				"btx_"+uuid.NewString(), workspaceID, id, t.ID, t.Amount.Minor, t.Amount.Currency, t.Merchant, t.MCC, t.BookedAt); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE bank_connections SET balance_minor = $3, balance_as_of = $4, refreshed_at = now() WHERE id = $1 AND workspace_id = $2`,
			id, workspaceID, bal.Minor, bal.AsOf)
		return err
	})
	if err != nil {
		return BankConnection{}, fmt.Errorf("economy: bank refresh: %w", err)
	}
	return s.bankConnection(ctx, workspaceID, id)
}

// RenewBankConsent asks the bank to extend connection id's consent.
func (s *DualTokenStore) RenewBankConsent(ctx context.Context, workspaceID, id string) (BankConnection, error) {
	p, err := s.bankPartner(ctx, workspaceID)
	if err != nil {
		return BankConnection{}, err
	}
	var ref string
	err = s.pool.QueryRow(ctx, `SELECT consent_ref FROM bank_connections WHERE id = $1 AND workspace_id = $2`, id, workspaceID).Scan(&ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return BankConnection{}, ErrBankConnectionNotFound
	}
	if err != nil {
		return BankConnection{}, fmt.Errorf("economy: bank consent: %w", err)
	}
	c, err := p.Renew(ctx, ref)
	if err != nil {
		return BankConnection{}, fmt.Errorf("%w: renewing the consent: %v", ErrBankConnection, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE bank_connections SET consent_expires_at = $3 WHERE id = $1 AND workspace_id = $2`, id, workspaceID, c.ExpiresAt); err != nil {
		return BankConnection{}, fmt.Errorf("economy: bank consent: %w", err)
	}
	return s.bankConnection(ctx, workspaceID, id)
}

// DisconnectBank ends connection id's consent and deletes it and every transaction read from it. When the partner
// cannot end the consent nothing is deleted, so the owner can try again.
func (s *DualTokenStore) DisconnectBank(ctx context.Context, workspaceID, id string) error {
	p, err := s.bankPartner(ctx, workspaceID)
	if err != nil {
		return err
	}
	var ref string
	err = s.pool.QueryRow(ctx, `SELECT consent_ref FROM bank_connections WHERE id = $1 AND workspace_id = $2`, id, workspaceID).Scan(&ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrBankConnectionNotFound
	}
	if err != nil {
		return fmt.Errorf("economy: bank disconnect: %w", err)
	}
	if err := p.Revoke(ctx, ref); err != nil && !errors.Is(err, partners.ErrNotFound) {
		return fmt.Errorf("%w: ending the consent: %v", ErrBankConnection, err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM bank_connections WHERE id = $1 AND workspace_id = $2`, id, workspaceID); err != nil {
		return fmt.Errorf("economy: bank disconnect: %w", err)
	}
	return nil
}

const bankConnectionColumns = `c.id, c.provider, c.bank, c.account, c.currency, c.consent_expires_at, c.balance_minor, c.balance_as_of, c.refreshed_at,
	(SELECT count(*) FROM bank_transactions t WHERE t.connection_id = c.id), c.created_at`

func scanBankConnection(row pgx.Row, now time.Time) (BankConnection, error) {
	var c BankConnection
	err := row.Scan(&c.ID, &c.Provider, &c.Bank, &c.Account, &c.Currency, &c.ConsentExpiresAt, &c.BalanceMinor, &c.BalanceAsOf, &c.RefreshedAt,
		&c.Transactions, &c.CreatedAt)
	c.RenewalDue = now.Add(BankConsentRemindBefore).After(c.ConsentExpiresAt)
	return c, err
}

func (s *DualTokenStore) bankConnection(ctx context.Context, workspaceID, id string) (BankConnection, error) {
	c, err := scanBankConnection(s.pool.QueryRow(ctx, `SELECT `+bankConnectionColumns+` FROM bank_connections c WHERE c.id = $1 AND c.workspace_id = $2`,
		id, workspaceID), time.Now())
	if errors.Is(err, pgx.ErrNoRows) {
		return BankConnection{}, ErrBankConnectionNotFound
	}
	return c, err
}

// BankConnections is workspaceID's connected accounts, oldest first, each saying whether its renewal is due as of now.
func (s *DualTokenStore) BankConnections(ctx context.Context, workspaceID string, now time.Time) ([]BankConnection, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+bankConnectionColumns+` FROM bank_connections c WHERE c.workspace_id = $1 ORDER BY c.created_at, c.id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: bank connections: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (BankConnection, error) { return scanBankConnection(row, now) })
	if err != nil {
		return nil, fmt.Errorf("economy: bank connections: %w", err)
	}
	return out, nil
}

// SweepBankConnections reads every connection last read BankBackgroundRefreshEvery or more before now, and answers how
// many it read. One that cannot be read is logged and left for the next sweep.
func (s *DualTokenStore) SweepBankConnections(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT workspace_id, id FROM bank_connections WHERE consent_expires_at > $1
		AND (refreshed_at IS NULL OR refreshed_at <= $2) ORDER BY refreshed_at NULLS FIRST`, now, now.Add(-BankBackgroundRefreshEvery))
	if err != nil {
		return 0, fmt.Errorf("economy: bank sweep: %w", err)
	}
	due, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (c [2]string, err error) { return c, row.Scan(&c[0], &c[1]) })
	if err != nil {
		return 0, fmt.Errorf("economy: bank sweep: %w", err)
	}
	n := 0
	for _, c := range due {
		if _, err := s.RefreshBank(ctx, c[0], c[1]); err != nil {
			slog.Warn("bank connections: read failed", slog.String("connection", c[1]), slog.String("err", err.Error()))
			continue
		}
		n++
	}
	return n, nil
}
