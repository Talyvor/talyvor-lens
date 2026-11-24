package phonesignin

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// B61.107 — a forgotten passcode, set again: the person confirms their number by text (the door's phone ticket) and
// answers a sign-in with one of their keys (the key ticket), and the two together set a new passcode with no old one.
// Neither ticket alone sets anything, nor two tickets of different people. A refusal spends neither ticket; a new
// passcode spends both, once.

var (
	// ErrKeyTicket: no key sign-in of the person stands with the call — none given, used, older than 5 minutes, or its
	// key removed.
	ErrKeyTicket = errors.New("phonesignin: confirm it is you with your key first")
	// ErrTwoPeople: the number and the key are two people's.
	ErrTwoPeople = errors.New("phonesignin: that number and that key are two different people's")
)

// ReplaceForgottenPasscode sets passcode for the person who holds the number phoneTicket confirmed, given a key
// ticket of the same person: ReplacePasscodeAfterKey's set (an off passcode comes back on), in the tickets' own
// transaction. It answers the person.
func (s *Store) ReplaceForgottenPasscode(ctx context.Context, phoneTicket, keyTicket, passcode, address string) (Person, error) {
	if len(s.PasscodeSecret) == 0 {
		return Person{}, ErrPasscodeUnavailable
	}
	now := s.now()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Person{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var (
		phone   string
		sandbox bool
	)
	err = tx.QueryRow(ctx, `SELECT phone, sandbox FROM phone_tickets WHERE ticket_hash = $1 AND used_at IS NULL AND expires_at > $2
		FOR UPDATE`, hashTicket(phoneTicket), now).Scan(&phone, &sandbox)
	if errors.Is(err, pgx.ErrNoRows) {
		return Person{}, s.refuse(ctx, tx, "", address, "a new passcode with a ticket used, old or never given", ErrTicket)
	}
	if err != nil {
		return Person{}, err
	}
	if sandbox && !s.Sender().Sandbox() {
		return Person{}, s.refuse(ctx, tx, phone, address, "a new passcode with a Sandbox confirmation under a real sender", ErrTicket)
	}
	if err := lock(ctx, tx, "phone:"+phone); err != nil {
		return Person{}, err
	}
	p, err := scanPerson(tx.QueryRow(ctx, `SELECT `+personColumns+` FROM people WHERE phone = $1 FOR UPDATE`, phone))
	if errors.Is(err, pgx.ErrNoRows) {
		return Person{}, s.refuse(ctx, tx, phone, address, "a new passcode for a number nobody holds", ErrNoPerson)
	}
	if err != nil {
		return Person{}, err
	}
	if sandbox && !p.PhoneSandbox {
		return Person{}, s.refuse(ctx, tx, phone, address, "a new passcode with a Sandbox confirmation for a number a real text confirmed", ErrTicket)
	}
	var keyPerson string
	err = tx.QueryRow(ctx, `SELECT t.person_id FROM person_key_tickets t JOIN person_keys k ON k.id = t.key_id
		WHERE t.ticket_hash = $1 AND t.used_at IS NULL AND t.expires_at > $2 AND k.removed_at IS NULL FOR UPDATE OF t`,
		hashTicket(keyTicket), now).Scan(&keyPerson)
	if errors.Is(err, pgx.ErrNoRows) {
		return Person{}, s.refuse(ctx, tx, phone, address, "a new passcode without a key sign-in", ErrKeyTicket)
	}
	if err != nil {
		return Person{}, err
	}
	if keyPerson != p.ID {
		return Person{}, s.refuse(ctx, tx, phone, address, "a new passcode with another person's key", ErrTwoPeople)
	}
	if _, err := tx.Exec(ctx, `UPDATE phone_tickets SET used_at = $2 WHERE ticket_hash = $1`, hashTicket(phoneTicket), now); err != nil {
		return Person{}, err
	}
	if err := useKeyTicket(ctx, tx, keyTicket, p.ID, now); err != nil {
		return Person{}, err
	}
	if err := s.setPasscode(ctx, tx, p.ID, phone, passcode, address, "set_after_key"); err != nil {
		return Person{}, err
	}
	p.HasPasscode = true
	return p, tx.Commit(ctx)
}
