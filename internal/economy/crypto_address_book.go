package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/money/smartaccount"
)

// crypto_address_book.go — B48.49: the crypto addresses a workspace's owner saved and named. An agent's send may name
// one instead of its 0x address (cryptoDestination); the quote shows the address it names. A name is the owner's own
// word for the address, so it can never be read as anything else a send's `to` takes: not a 0x address, an ENS name, a
// person's @name or email, or a phone number.

var (
	// ErrCryptoAddress: an address or a name that cannot be saved as asked.
	ErrCryptoAddress = errors.New("economy: invalid saved crypto address")
	// ErrCryptoAddressNotFound: no such saved address in this workspace.
	ErrCryptoAddressNotFound = errors.New("economy: no such saved crypto address")
)

// CryptoAddress is one saved address.
type CryptoAddress struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Address   string    `json:"address"`
	SavedBy   string    `json:"saved_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// cryptoAddressName is name as it is saved, or why it cannot be.
func cryptoAddressName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	lower := strings.ToLower(name)
	switch {
	case name == "" || len(name) > 64:
		return "", fmt.Errorf("%w: a name of 1 to 64 characters", ErrCryptoAddress)
	case strings.HasPrefix(lower, "0x"), strings.HasPrefix(name, "+"), strings.Contains(name, "@"), smartaccount.IsENSName(name):
		return "", fmt.Errorf("%w: a name that is not an address, an ENS name, an @name, an email or a phone number", ErrCryptoAddress)
	case strings.IndexFunc(name, unicode.IsLetter) < 0:
		return "", fmt.Errorf("%w: a name with a letter in it", ErrCryptoAddress)
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return "", fmt.Errorf("%w: a name of printable characters", ErrCryptoAddress)
	}
	return name, nil
}

const cryptoAddressCols = `id, name, address, saved_by, created_at, updated_at`

func scanCryptoAddress(row pgx.Row) (CryptoAddress, error) {
	var a CryptoAddress
	err := row.Scan(&a.ID, &a.Name, &a.Address, &a.SavedBy, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrCryptoAddressNotFound
	}
	if err != nil {
		return a, fmt.Errorf("economy: saved crypto address: %w", err)
	}
	return a, nil
}

// SaveCryptoAddress saves address under name for workspaceID, or renames it when it is saved already.
func (s *DualTokenStore) SaveCryptoAddress(ctx context.Context, workspaceID, by, name, address string) (CryptoAddress, error) {
	name, err := cryptoAddressName(name)
	if err != nil {
		return CryptoAddress{}, err
	}
	addr := smartaccount.Checksum(strings.TrimSpace(address))
	if addr == "" {
		return CryptoAddress{}, fmt.Errorf("%w: a 0x address", ErrCryptoAddress)
	}
	a, err := scanCryptoAddress(s.pool.QueryRow(ctx, `INSERT INTO workspace_crypto_addresses (id, workspace_id, name, address, saved_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (workspace_id, lower(address)) DO UPDATE SET name = EXCLUDED.name, saved_by = EXCLUDED.saved_by, updated_at = now()
		RETURNING `+cryptoAddressCols, "caddr_"+uuid.NewString(), workspaceID, name, addr, by))
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return CryptoAddress{}, fmt.Errorf("%w: another saved address is named %q", ErrCryptoAddress, name)
	}
	if err != nil {
		return CryptoAddress{}, err
	}
	return a, s.AppendAudit(ctx, workspaceID, by, "crypto_address.save", map[string]any{"id": a.ID, "name": a.Name, "address": a.Address})
}

// CryptoAddresses is every address workspaceID saved, by name.
func (s *DualTokenStore) CryptoAddresses(ctx context.Context, workspaceID string) ([]CryptoAddress, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cryptoAddressCols+` FROM workspace_crypto_addresses WHERE workspace_id = $1
		ORDER BY lower(name), id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: saved crypto addresses: %w", err)
	}
	defer rows.Close()
	out := []CryptoAddress{}
	for rows.Next() {
		a, err := scanCryptoAddress(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RemoveCryptoAddress removes the saved address id from workspaceID.
func (s *DualTokenStore) RemoveCryptoAddress(ctx context.Context, workspaceID, by, id string) error {
	a, err := scanCryptoAddress(s.pool.QueryRow(ctx, `DELETE FROM workspace_crypto_addresses WHERE id = $1 AND workspace_id = $2
		RETURNING `+cryptoAddressCols, id, workspaceID))
	if err != nil {
		return err
	}
	return s.AppendAudit(ctx, workspaceID, by, "crypto_address.remove", map[string]any{"id": a.ID, "name": a.Name, "address": a.Address})
}

// savedCryptoAddress is the address workspaceID saved as name; found false when it saved none so named.
func (s *DualTokenStore) savedCryptoAddress(ctx context.Context, workspaceID, name string) (a CryptoAddress, found bool, err error) {
	name = strings.Join(strings.Fields(name), " ")
	a, err = scanCryptoAddress(s.pool.QueryRow(ctx, `SELECT `+cryptoAddressCols+` FROM workspace_crypto_addresses
		WHERE workspace_id = $1 AND lower(name) = lower($2)`, workspaceID, name))
	if errors.Is(err, ErrCryptoAddressNotFound) {
		return a, false, nil
	}
	return a, err == nil, err
}
