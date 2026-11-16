package phonesignin

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// B61.104 — a person changes their own number, from Settings, with a session of them the BFF holds: they confirm the
// new number by text (Start and Finish, the door's own limits and Sandbox code) and give their passcode. Unlike an
// operator's move (move.go) nothing is taken: the passcode, the keys and the accounts stay, and nothing is held. The
// old number signs the person in no more. The change is a person_number_moves row, changed_by 'person', so the BFF
// tells the email addresses as it does for a move.

// NumberChanged is a person's number changed.
type NumberChanged struct {
	MoveID string `json:"move_id"`
	Person Person `json:"person"`
}

// ChangeNumber moves personID onto the number ticket confirmed, given their passcode. A refused change leaves the
// ticket unused; a wrong passcode stays counted, at the lock, as every passcode given with a session is.
func (s *Store) ChangeNumber(ctx context.Context, personID, ticket, passcode, address string) (NumberChanged, error) {
	now := s.now()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return NumberChanged{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var (
		newPhone string
		sandbox  bool
	)
	err = tx.QueryRow(ctx, `SELECT phone, sandbox FROM phone_tickets WHERE ticket_hash = $1 AND used_at IS NULL AND expires_at > $2
		FOR UPDATE`, hashTicket(ticket), now).Scan(&newPhone, &sandbox)
	if errors.Is(err, pgx.ErrNoRows) {
		return NumberChanged{}, s.refuse(ctx, tx, "", address, "a number change with a ticket used, old or never given", ErrTicket)
	}
	if err != nil {
		return NumberChanged{}, err
	}
	if sandbox && !s.Sender().Sandbox() {
		return NumberChanged{}, s.refuse(ctx, tx, newPhone, address, "a number change with a Sandbox confirmation under a real sender", ErrTicket)
	}
	if err := lock(ctx, tx, "person-number:"+personID); err != nil {
		return NumberChanged{}, err
	}
	var oldPhone string
	if err := tx.QueryRow(ctx, `SELECT phone FROM people WHERE id = $1`, personID).Scan(&oldPhone); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return NumberChanged{}, ErrNoPerson
		}
		return NumberChanged{}, err
	}
	// The two numbers' locks in one order, as a move takes them.
	first, second := oldPhone, newPhone
	if second < first {
		first, second = second, first
	}
	for _, key := range []string{"phone:" + first, "phone:" + second} {
		if err := lock(ctx, tx, key); err != nil {
			return NumberChanged{}, err
		}
	}
	var locked string
	if err := tx.QueryRow(ctx, `SELECT phone FROM people WHERE id = $1 FOR UPDATE`, personID).Scan(&locked); err != nil {
		return NumberChanged{}, err
	}
	if locked != oldPhone {
		return NumberChanged{}, ErrNotThatPerson // an operator moved the number meanwhile
	}
	has, err := hasPasscode(ctx, tx, personID)
	if err != nil {
		return NumberChanged{}, err
	}
	if !has {
		return NumberChanged{}, s.refuse(ctx, tx, newPhone, address, "a number change by a person with no passcode", ErrNoPasscode)
	}
	if err := s.checkPasscode(ctx, tx, personID, passcode, address, passcodeAtLock); err != nil {
		if errors.Is(err, ErrPasscodeUnavailable) || errors.Is(err, ErrNoPasscode) {
			return NumberChanged{}, err
		}
		// The ticket stays unused; the wrong try stays counted.
		return NumberChanged{}, commitThen(ctx, tx, err)
	}
	if newPhone == oldPhone {
		return NumberChanged{}, s.refuse(ctx, tx, newPhone, address, "a number change onto the person's own number", ErrSameNumber)
	}
	var held bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM people WHERE phone = $1)`, newPhone).Scan(&held); err != nil {
		return NumberChanged{}, err
	}
	if held {
		return NumberChanged{}, s.refuse(ctx, tx, newPhone, address, "a number change onto another person's number", ErrHeld)
	}
	if _, err := tx.Exec(ctx, `UPDATE phone_tickets SET used_at = $2 WHERE ticket_hash = $1`, hashTicket(ticket), now); err != nil {
		return NumberChanged{}, err
	}
	// Every ticket still open for either number is spent: the old number's would sign in somebody new on it.
	if _, err := tx.Exec(ctx, `UPDATE phone_tickets SET used_at = $3 WHERE phone IN ($1, $2) AND used_at IS NULL`, oldPhone, newPhone, now); err != nil {
		return NumberChanged{}, err
	}
	p, err := scanPerson(tx.QueryRow(ctx, `UPDATE people SET phone = $2, phone_sandbox = $3, phone_confirmed_at = $4 WHERE id = $1
		RETURNING `+personColumns, personID, newPhone, sandbox, now))
	if err != nil {
		return NumberChanged{}, err
	}
	p.HasPasscode = true
	id, err := randomToken(16)
	if err != nil {
		return NumberChanged{}, err
	}
	out := NumberChanged{MoveID: "nmv_" + id, Person: p}
	if _, err := tx.Exec(ctx, `INSERT INTO person_number_moves (id, person_id, old_phone, new_phone, operator, changed_by, moved_at, held_until)
		VALUES ($1, $2, $3, $4, '', 'person', $5, $5)`, out.MoveID, personID, oldPhone, newPhone, now); err != nil {
		return NumberChanged{}, err
	}
	for _, phone := range []string{oldPhone, newPhone} {
		if err := audit(ctx, tx, phone, address, "number_moved", "by the person, move "+out.MoveID); err != nil {
			return NumberChanged{}, err
		}
	}
	return out, tx.Commit(ctx)
}
