package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/partners"
)

// number_move.go — B61.30: THE WAY BACK IN FOR A PERSON WHO HAS NEITHER THEIR NUMBER NOR A KEY. An operator moves the
// person's number (internal/phonesignin.MoveNumber) only after the person's identity check has passed again against
// the name and date of birth on record (RecheckPersonIdentity). For a day after it each of the person's accounts can
// look and cannot move money (number_move_holds): every money capability that refuses a frozen workspace refuses a
// held one too (freezeRefusal), and Talyvor's own services go on. Every device of the person's accounts is told
// (TellNumberMoved).

// NumberMoveHoldFor is how long after a number move the person's accounts cannot move money.
const NumberMoveHoldFor = 24 * time.Hour

var (
	// ErrNumberMoveHold: the account's number was moved by Talyvor's help team less than a day ago.
	ErrNumberMoveHold = errors.New("economy: refused: this account's phone number was changed less than a day ago")
	// ErrNoIdentityOnRecord: none of the person's accounts holds a passed identity check with a date of birth.
	ErrNoIdentityOnRecord = errors.New("economy: there is no identity check on record for this person to check against")
	// ErrNotPersonOnRecord: the name, date of birth or country given are not those on record for this person.
	ErrNotPersonOnRecord = errors.New("economy: the name, date of birth and country are not those on record for this person")
	// ErrIdentityRecheckFailed: the identity check asked again did not pass, or passed only by the Test provider for a
	// person a real provider checked.
	ErrIdentityRecheckFailed = errors.New("economy: the identity check did not pass again")
)

// NumberMoveHold is a hold on an account's money after its person's number was moved.
type NumberMoveHold struct {
	WorkspaceID string    `json:"workspace_id"`
	HeldUntil   time.Time `json:"held_until"`
}

// numberMoveHold is the hold on workspaceID now, or nil.
func numberMoveHold(ctx context.Context, q pgxDB, workspaceID string) (*NumberMoveHold, error) {
	h := NumberMoveHold{WorkspaceID: workspaceID}
	err := q.QueryRow(ctx, `SELECT held_until FROM number_move_holds WHERE workspace_id = $1 AND held_until > now()`, workspaceID).
		Scan(&h.HeldUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("economy: number move hold: %w", err)
	}
	return &h, nil
}

// HoldAccountsAfterNumberMove holds each of accounts until heldUntil, inside tx: it takes each account's freeze lock
// exclusively, so money already moving finishes first and every movement after it sees the hold.
func HoldAccountsAfterNumberMove(ctx context.Context, tx pgx.Tx, moveID string, accounts []string, heldUntil time.Time) error {
	for _, ws := range accounts {
		if _, err := tx.Exec(ctx, FreezeLockExclusiveSQL, ws); err != nil {
			return fmt.Errorf("economy: number move hold: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO number_move_holds (workspace_id, move_id, held_until) VALUES ($1, $2, $3)
			ON CONFLICT (workspace_id) DO UPDATE SET move_id = EXCLUDED.move_id, held_until = EXCLUDED.held_until`, ws, moveID, heldUntil); err != nil {
			return fmt.Errorf("economy: number move hold: %w", err)
		}
	}
	return nil
}

func sameName(a, b string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(a), " "), strings.Join(strings.Fields(b), " "))
}

// RecheckPersonIdentity asks the identity check again for the person who holds accounts (their first account
// first), for an operator about to move their number. The record is the newest identity check passed in any of the
// accounts that kept a date of birth. The name, date of birth and country in must be the record's before any provider
// is asked, so a check of somebody else is never written into the person's account; then the check runs in the
// record's account, by who asked, and must pass — by a real provider when a real one passed the record.
func (s *DualTokenStore) RecheckPersonIdentity(ctx context.Context, verifiers partners.Verifiers, accounts []string, by string,
	in VerificationRequest) (VerificationCheck, error) {
	in.Level = LevelIdentity
	if err := checkVerificationRequest(&in); err != nil {
		return VerificationCheck{}, err
	}
	var (
		record   *VerificationCheck
		recordWS string
	)
	for _, ws := range accounts {
		checks, err := readVerificationChecks(ctx, s.pool, ws)
		if err != nil {
			return VerificationCheck{}, err
		}
		for i := range checks { // newest first
			c := checks[i]
			if c.Level == LevelIdentity && c.Status == string(partners.StatusCompleted) && c.DateOfBirthDigest != "" {
				if record == nil || c.CheckedAt.After(record.CheckedAt) {
					record, recordWS = &checks[i], ws
				}
				break
			}
		}
	}
	if record == nil {
		return VerificationCheck{}, ErrNoIdentityOnRecord
	}
	if !sameName(in.Name, record.VerifiedName) || in.Country != record.Country ||
		dateOfBirthDigest(recordWS, in.DateOfBirth) != record.DateOfBirthDigest {
		return VerificationCheck{}, ErrNotPersonOnRecord
	}
	c, err := s.StartVerification(ctx, verifiers, recordWS, by, in)
	if err != nil {
		return VerificationCheck{}, err
	}
	switch {
	case c.Status != string(partners.StatusCompleted):
		return c, fmt.Errorf("%w: the check is %s", ErrIdentityRecheckFailed, c.Status)
	case c.Test && !record.Test:
		return c, fmt.Errorf("%w: a real provider checked this person, and only the Test provider answered now", ErrIdentityRecheckFailed)
	}
	return c, nil
}

// TellNumberMoved pushes, to every device of each of accounts, that the person's number was changed.
func (s *DualTokenStore) TellNumberMoved(ctx context.Context, accounts []string) {
	for _, ws := range accounts {
		s.pushDevices(ctx, ws, nativepush.Message{
			Body: "Your phone number on Talyvor was changed by our help team. If this was not you, contact help now.",
			Data: map[string]string{"type": "number_moved", "workspace_id": ws}})
	}
}

// TellNumberChanged pushes, to every device of each of accounts, that the person changed their own number (B61.104).
func (s *DualTokenStore) TellNumberChanged(ctx context.Context, accounts []string) {
	for _, ws := range accounts {
		s.pushDevices(ctx, ws, nativepush.Message{
			Body: "Your phone number on Talyvor was changed. If this was not you, contact help now.",
			Data: map[string]string{"type": "number_changed", "workspace_id": ws}})
	}
}
