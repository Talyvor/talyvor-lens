package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// B61.7: one person holds several accounts. A person is the identity the BFF presents to POST /v1/provision; their
// first account is the workspace provision derives from that identity, and every account opened after it is a row of
// person_accounts (migration 0391) naming a workspace of its own.

// MaxPersonAccounts is how many accounts one person may hold, the first included (B61.7).
const MaxPersonAccounts = 10

// ErrPersonAccountLimit refuses an account past MaxPersonAccounts.
var ErrPersonAccountLimit = fmt.Errorf("you already hold %d accounts, the most one person can hold", MaxPersonAccounts)

// PersonAccount is one account a person holds, as the account switcher shows it.
type PersonAccount struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Type     string    `json:"type"`
	Kind     string    `json:"kind"`
	OpenedAt time.Time `json:"opened_at"`
}

// PersonKey is the key person_accounts holds for an identity: a hash, so the table never holds the identity itself,
// and a different one from the first account's workspace id.
func PersonKey(identity string) string {
	sum := sha256.Sum256([]byte("talyvor-person\x00" + identity))
	return "p" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))[:26]
}

// FirstAccountID is the id of the person's first account, the workspace POST /v1/provision derives from their
// identity.
func FirstAccountID(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return "u" + strings.ToLower(enc)[:26]
}

// NewAccountID is a fresh workspace id for an account opened after a person's first, in the shape of the derived ones.
func NewAccountID(random [16]byte) string {
	return "u" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(random[:]))[:26]
}

// personAccountsSQL lists the person's first account (firstID, when its workspace exists) and every account they opened
// after it, oldest first. The join on workspaces drops a row whose workspace was never made or has been erased.
const personAccountsSQL = `SELECT w.id, w.name, w.account_type, w.money_kind, w.created_at
FROM workspaces w
WHERE w.id = $2 OR w.id IN (SELECT workspace_id FROM person_accounts WHERE person_key = $1)
ORDER BY (w.id = $2) DESC, w.created_at, w.id`

// PersonAccounts lists the accounts a person holds, their first account first.
func (m *Manager) PersonAccounts(ctx context.Context, personKey, firstID string) ([]PersonAccount, error) {
	if m.pool == nil {
		return nil, errors.New("workspace: accounts need a database")
	}
	rows, err := m.pool.Query(ctx, personAccountsSQL, personKey, firstID)
	if err != nil {
		return nil, fmt.Errorf("workspace: list a person's accounts: %w", err)
	}
	defer rows.Close()
	out := []PersonAccount{}
	for rows.Next() {
		var a PersonAccount
		if err := rows.Scan(&a.ID, &a.Name, &a.Type, &a.Kind, &a.OpenedAt); err != nil {
			return nil, fmt.Errorf("workspace: read a person's account: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PersonOwnsAccount says whether accountID is one of the person's accounts and its workspace exists.
func (m *Manager) PersonOwnsAccount(ctx context.Context, personKey, firstID, accountID string) (bool, error) {
	if m.pool == nil {
		return false, errors.New("workspace: accounts need a database")
	}
	var owned bool
	err := m.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspaces WHERE id = $3
		AND ($3 = $2 OR EXISTS (SELECT 1 FROM person_accounts WHERE person_key = $1 AND workspace_id = $3)))`,
		personKey, firstID, accountID).Scan(&owned)
	if err != nil {
		return false, fmt.Errorf("workspace: check a person's account: %w", err)
	}
	return owned, nil
}

type beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ReservePersonAccount records accountID as another account of the person, before its workspace is made, refusing it
// with ErrPersonAccountLimit when the person already holds MaxPersonAccounts. The count and the insert run under a lock
// on the person, so two opens at once cannot both take the last place;
// a reservation still being made counts, and one whose workspace failed is released.
func (m *Manager) ReservePersonAccount(ctx context.Context, personKey, firstID, accountID string) error {
	b, ok := m.pool.(beginner)
	if !ok {
		return errors.New("workspace: accounts need a database")
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return fmt.Errorf("workspace: open another account: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('person_accounts:' || $1, 0))`, personKey); err != nil {
		return fmt.Errorf("workspace: lock the person's accounts: %w", err)
	}
	var held int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM workspaces WHERE id = $2)
		+ (SELECT count(*) FROM person_accounts WHERE person_key = $1)`,
		personKey, firstID).Scan(&held); err != nil {
		return fmt.Errorf("workspace: count the person's accounts: %w", err)
	}
	if held >= MaxPersonAccounts {
		return ErrPersonAccountLimit
	}
	if _, err := tx.Exec(ctx, `INSERT INTO person_accounts (workspace_id, person_key) VALUES ($1, $2)`, accountID, personKey); err != nil {
		return fmt.Errorf("workspace: record the new account: %w", err)
	}
	return tx.Commit(ctx)
}

// ReleasePersonAccount drops a reservation whose workspace could not be made.
func (m *Manager) ReleasePersonAccount(ctx context.Context, personKey, accountID string) error {
	if m.pool == nil {
		return nil
	}
	_, err := m.pool.Exec(ctx, `DELETE FROM person_accounts WHERE person_key = $1 AND workspace_id = $2`, personKey, accountID)
	return err
}
