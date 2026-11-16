package phonesignin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/economy"
)

// B61.30 — the way back in for a person who has neither their number nor a key: lost phone, new number, or a
// forgotten passcode with no key. There is no way to do it alone in the first version; an operator does it, behind
// step-up and only after the person's identity check has passed again (cmd/lens/number_move_handler.go). The move
// takes every way in the person had: the passcode is cleared and every sign-in key removed, so the lost phone signs
// nobody in, and every ticket still open for either number or from a key is spent. The person then confirms the new
// number by text and, with that ticket, sets a new passcode (RedeemWithPasscode) and adds a key with it (AddKey). For a
// day the person's accounts can look and cannot move money (economy.NumberMoveHoldFor).

var (
	// ErrSameNumber: the new number is the one the person has.
	ErrSameNumber = errors.New("phonesignin: that is the number the person has already")
	// ErrNotThatPerson: the person who holds the number is not the one the move was checked for.
	ErrNotThatPerson = errors.New("phonesignin: that number is not held by the person checked")
)

// NumberMove is what an operator asks: move the person PersonID from OldPhone to NewPhone, both E.164, holding
// Accounts, the person's accounts (their first account first).
type NumberMove struct {
	PersonID         string
	OldPhone         string
	NewPhone         string
	Operator         string
	IdentityCheckRef string
	Accounts         []string
	Address          string
}

// Moved is a number moved.
type Moved struct {
	ID          string    `json:"id"`
	PersonID    string    `json:"person_id"`
	NewPhone    string    `json:"new_phone"`
	MovedAt     time.Time `json:"moved_at"`
	HeldUntil   time.Time `json:"held_until"`
	HeldAccount int       `json:"accounts_held"`
	KeysRemoved int64     `json:"keys_removed"`
}

// NumberMoveNotice is a move whose person's email addresses are still to be told. By is who moved it: "operator", or
// "person" for a person's own change (B61.104).
type NumberMoveNotice struct {
	ID       string    `json:"id"`
	PersonID string    `json:"person_id"`
	Identity string    `json:"identity"`
	OldPhone string    `json:"old_phone"`
	NewPhone string    `json:"new_phone"`
	MovedAt  time.Time `json:"moved_at"`
	By       string    `json:"by"`
}

// MoveNumber moves m.PersonID's number, in one transaction, as the package comment says.
func (s *Store) MoveNumber(ctx context.Context, m NumberMove) (Moved, error) {
	if m.OldPhone == m.NewPhone {
		return Moved{}, ErrSameNumber
	}
	now := s.now()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Moved{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The two numbers' locks in one order, so two moves between them cannot wait on each other.
	first, second := m.OldPhone, m.NewPhone
	if second < first {
		first, second = second, first
	}
	for _, key := range []string{"phone:" + first, "phone:" + second, "person-keys:" + m.PersonID, "passcode:" + m.PersonID} {
		if err := lock(ctx, tx, key); err != nil {
			return Moved{}, err
		}
	}
	var holder string
	err = tx.QueryRow(ctx, `SELECT id FROM people WHERE phone = $1 FOR UPDATE`, m.OldPhone).Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && holder != m.PersonID {
		return Moved{}, ErrNotThatPerson
	}
	if err != nil {
		return Moved{}, err
	}
	var held bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM people WHERE phone = $1)`, m.NewPhone).Scan(&held); err != nil {
		return Moved{}, err
	}
	if held {
		return Moved{}, s.refuse(ctx, tx, m.NewPhone, m.Address, "a number move onto another person's number", ErrHeld)
	}
	// Not confirmed by a real text yet: the person's next sign-in by text confirms it.
	if _, err := tx.Exec(ctx, `UPDATE people SET phone = $2, phone_sandbox = true WHERE id = $1`, m.PersonID, m.NewPhone); err != nil {
		return Moved{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM person_passcodes WHERE person_id = $1`, m.PersonID); err != nil {
		return Moved{}, err
	}
	tag, err := tx.Exec(ctx, `UPDATE person_keys SET removed_at = $2 WHERE person_id = $1 AND removed_at IS NULL`, m.PersonID, now)
	if err != nil {
		return Moved{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE person_key_tickets SET used_at = $2 WHERE person_id = $1 AND used_at IS NULL`, m.PersonID, now); err != nil {
		return Moved{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE phone_tickets SET used_at = $3 WHERE phone IN ($1, $2) AND used_at IS NULL`, m.OldPhone, m.NewPhone, now); err != nil {
		return Moved{}, err
	}
	id, err := randomToken(16)
	if err != nil {
		return Moved{}, err
	}
	out := Moved{ID: "nmv_" + id, PersonID: m.PersonID, NewPhone: m.NewPhone, MovedAt: now, HeldUntil: now.Add(economy.NumberMoveHoldFor),
		HeldAccount: len(m.Accounts), KeysRemoved: tag.RowsAffected()}
	if _, err := tx.Exec(ctx, `INSERT INTO person_number_moves (id, person_id, old_phone, new_phone, operator, identity_check_ref, moved_at, held_until)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, out.ID, m.PersonID, m.OldPhone, m.NewPhone, m.Operator, m.IdentityCheckRef, now, out.HeldUntil); err != nil {
		return Moved{}, err
	}
	if err := economy.HoldAccountsAfterNumberMove(ctx, tx, out.ID, m.Accounts, out.HeldUntil); err != nil {
		return Moved{}, err
	}
	detail := fmt.Sprintf("by operator %s, move %s", m.Operator, out.ID)
	for _, phone := range []string{m.OldPhone, m.NewPhone} {
		if err := audit(ctx, tx, phone, m.Address, "number_moved", detail); err != nil {
			return Moved{}, err
		}
	}
	return out, tx.Commit(ctx)
}

// ClaimNumberMoveNotices takes, once, the moves whose person's email addresses are still to be told, the oldest first:
// the BFF, which holds the addresses, tells them.
func (s *Store) ClaimNumberMoveNotices(ctx context.Context, limit int) ([]NumberMoveNotice, error) {
	rows, err := s.DB.Query(ctx, `UPDATE person_number_moves m SET emails_claimed_at = $2 FROM people p
		WHERE p.id = m.person_id AND m.id IN (SELECT id FROM person_number_moves WHERE emails_claimed_at IS NULL
			ORDER BY moved_at, id LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING m.id, m.person_id, p.identity, m.old_phone, m.new_phone, m.moved_at, m.changed_by`, limit, s.now())
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[NumberMoveNotice])
	if out == nil {
		out = []NumberMoveNotice{}
	}
	return out, err
}
