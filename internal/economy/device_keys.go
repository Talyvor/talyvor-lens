package economy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/passkey"
)

// device_keys.go — B40.4: THE PHONE APP'S HARDWARE KEY SIGNS APPROVALS AND MANDATES, AND IS REVOKED AT ONCE.
//
// The owner registers a phone's key (devicekey.Verifier checks its App Attest or Android key attestation) over a
// single-use challenge. From then on, wherever a passkey assertion is accepted for an approval or a mandate, an
// assertion with device_id, challenge and the key's signature over devicekey.Message(challenge) is accepted too. A
// revoked key signs nothing from the moment it is revoked: the signing path locks the key's row, so a revocation
// waits for a signature already being checked and every later one sees it.

// ErrDeviceKeyNotFound: no such device key in the workspace.
var ErrDeviceKeyNotFound = errors.New("economy: no such device key in this workspace")

// DeviceKey is one of a workspace's registered phone keys.
type DeviceKey struct {
	ID         string     `json:"id"`
	Platform   string     `json:"platform"`
	Name       string     `json:"name"`
	Person     string     `json:"person"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	RevokedBy  string     `json:"revoked_by,omitempty"`
}

// SetDeviceKeys sets how device key registrations are checked.
func (s *DualTokenStore) SetDeviceKeys(v devicekey.Verifier) { s.deviceVerifier = v }

// DeviceRegistrationChallenge issues the challenge a new device key signs and binds its attestation to.
func (s *DualTokenStore) DeviceRegistrationChallenge(ctx context.Context, workspaceID string) (string, error) {
	return s.newChallenge(ctx, workspaceID, "device-register")
}

// RegisterDeviceKey verifies r against the challenge it answers and keeps the key as person's.
func (s *DualTokenStore) RegisterDeviceKey(ctx context.Context, workspaceID, person, name string, r devicekey.Registration) (DeviceKey, error) {
	if err := s.deviceVerifier.VerifyRegistration(r); err != nil {
		return DeviceKey{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeviceKey{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := useChallenge(ctx, tx, workspaceID, "device-register", r.Challenge); err != nil {
		return DeviceKey{}, fmt.Errorf("%w: %v", devicekey.ErrInvalid, err)
	}
	d := DeviceKey{ID: "dev_" + uuid.NewString(), Platform: r.Platform, Name: name, Person: person}
	if err := tx.QueryRow(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, person, platform, name, public_key, attestation)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`, d.ID, workspaceID, person, r.Platform, name, r.PublicKey,
		append([]byte{}, r.Attestation...)).Scan(&d.CreatedAt); err != nil {
		return d, fmt.Errorf("economy: register device key: %w", err)
	}
	return d, tx.Commit(ctx)
}

const deviceKeyCols = `id, platform, name, person, created_at, last_used_at, revoked_at, revoked_by`

func scanDeviceKey(row pgx.Row) (DeviceKey, error) {
	var d DeviceKey
	err := row.Scan(&d.ID, &d.Platform, &d.Name, &d.Person, &d.CreatedAt, &d.LastUsedAt, &d.RevokedAt, &d.RevokedBy)
	return d, err
}

// ListDeviceKeys reads a workspace's device keys, revoked ones included, oldest first.
func (s *DualTokenStore) ListDeviceKeys(ctx context.Context, workspaceID string) ([]DeviceKey, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+deviceKeyCols+` FROM workspace_device_keys WHERE workspace_id = $1 ORDER BY created_at, id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: device keys: %w", err)
	}
	defer rows.Close()
	out := []DeviceKey{}
	for rows.Next() {
		d, err := scanDeviceKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RevokeDeviceKey revokes a device key at once; revoking it again changes nothing.
func (s *DualTokenStore) RevokeDeviceKey(ctx context.Context, workspaceID, id, by string) (DeviceKey, error) {
	if _, err := s.pool.Exec(ctx, `UPDATE workspace_device_keys SET revoked_at = now(), revoked_by = $3
		WHERE id = $1 AND workspace_id = $2 AND revoked_at IS NULL`, id, workspaceID, by); err != nil {
		return DeviceKey{}, fmt.Errorf("economy: revoke device key: %w", err)
	}
	d, err := scanDeviceKey(s.pool.QueryRow(ctx, `SELECT `+deviceKeyCols+` FROM workspace_device_keys WHERE id = $1 AND workspace_id = $2`, id, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrDeviceKeyNotFound
	}
	return d, err
}

// lockDeviceKey reads the public key of a's device, inside tx, holding its row until tx ends; a key that is revoked
// or another workspace's is refused.
func lockDeviceKey(ctx context.Context, tx pgx.Tx, workspaceID string, a passkey.Assertion) ([]byte, error) {
	var spki []byte
	err := tx.QueryRow(ctx, `SELECT public_key FROM workspace_device_keys WHERE id = $1 AND workspace_id = $2 AND revoked_at IS NULL FOR UPDATE`,
		a.DeviceID, workspaceID).Scan(&spki)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: not a device key of this workspace, or it was revoked", passkey.ErrInvalid)
	}
	if err != nil {
		return nil, fmt.Errorf("economy: device key: %w", err)
	}
	return spki, nil
}

// signDevice checks a is its device's signature over challenge, inside tx, and records the key's use.
func signDevice(ctx context.Context, tx pgx.Tx, workspaceID, challenge string, a passkey.Assertion) error {
	spki, err := lockDeviceKey(ctx, tx, workspaceID, a)
	if err != nil {
		return err
	}
	if err := devicekey.Verify(spki, challenge, a.Signature); err != nil {
		return fmt.Errorf("%w: %v", passkey.ErrInvalid, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE workspace_device_keys SET last_used_at = now() WHERE id = $1`, a.DeviceID); err != nil {
		return fmt.Errorf("economy: device key: %w", err)
	}
	return nil
}

// spendDeviceSignature checks a, by one of workspaceID's device keys, over an open challenge for purpose, and spends
// the challenge, inside tx.
func spendDeviceSignature(ctx context.Context, tx pgx.Tx, workspaceID, purpose string, a passkey.Assertion) error {
	if err := signDevice(ctx, tx, workspaceID, a.Challenge, a); err != nil {
		return err
	}
	return useChallenge(ctx, tx, workspaceID, purpose, a.Challenge)
}
