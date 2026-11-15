// Package phonesignin is the front door by phone number (B61.22): a texted code confirms a number, and the ticket
// it gives finds or makes the person who holds it. A person holds the identity string POST /v1/provision is given
// for them, so their accounts hang off the person whichever way they signed in.
//
// Until the code is right nothing here says whether a number has a person: starting a check never looks, and a
// wrong or old code is refused in the same words for either.
package phonesignin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/partners"
)

// The rules of a check.
const (
	CodeLife      = 10 * time.Minute // a code is good for this long, once
	TicketLife    = 5 * time.Minute  // a ticket is good for this long, once
	MaxWrongTries = 5                // this many wrong codes end the check
	TextGap       = 30 * time.Second // at most one text to a number in this long
	TextsPerHour  = 5                // and this many in an hour
)

// DefaultTextsPerAddressHour is the texts one caller's address may ask for in an hour until Nicolai sets
// LENS_PHONE_CHECKS_PER_ADDRESS_HOUR: the same as one number may get.
const DefaultTextsPerAddressHour = TextsPerHour

var (
	// ErrWrongCode: the code is not the one sent. The check stands until its fifth wrong try.
	ErrWrongCode = errors.New("phonesignin: that code is not right")
	// ErrNoCode: no code is waiting for this number — none was sent, it was used, it is older than 10 minutes, a
	// newer one replaced it, or five wrong tries ended it.
	ErrNoCode = errors.New("phonesignin: that code has ended; ask for a new one")
	// ErrTicket: the ticket is not one this gave, was used, or is older than 5 minutes.
	ErrTicket = errors.New("phonesignin: that confirmation has ended; confirm the number again")
	// ErrHeld: another person holds the number or the identity.
	ErrHeld = errors.New("phonesignin: another person holds that number or identity")
)

// LimitError is a text refused by a limit, with when one may be sent.
type LimitError struct{ RetryAt time.Time }

func (e *LimitError) Error() string {
	return "phonesignin: too many codes asked for; try again at " + e.RetryAt.UTC().Format(time.RFC3339)
}

// Store keeps people, checks and tickets in Postgres, so every replica counts the same texts and tries.
type Store struct {
	DB *pgxpool.Pool
	// Sender is the text sender in force now (partners.Registry.TextMessages).
	Sender func() partners.TextSender
	// Key is mixed into every code's hash, so the database alone cannot try the million codes.
	Key []byte
	// PerAddressHour is the texts one caller's address may ask for in an hour.
	PerAddressHour int
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Started is a code sent.
type Started struct {
	ExpiresAt time.Time
	Sender    string
	// SandboxCode is the code when the Sandbox sender sent nothing, for the screen to show under the Sandbox label;
	// empty under a real sender.
	SandboxCode string
}

// Finished is a right code: a ticket that this number was confirmed just now, and whether a person holds it.
type Finished struct {
	Ticket    string
	ExpiresAt time.Time
	HasPerson bool
	Sandbox   bool
}

// Person is one person, by their number.
type Person struct {
	ID               string    `json:"id"`
	Phone            string    `json:"phone"`
	Identity         string    `json:"identity"`
	PhoneSandbox     bool      `json:"phone_sandbox"`
	PhoneConfirmedAt time.Time `json:"phone_confirmed_at"`
	CreatedAt        time.Time `json:"created_at"`
}

// Counts is what an operator may read: how many, never whose.
type Counts struct {
	People          int64 `json:"people"`
	PeopleSandbox   int64 `json:"people_confirmed_by_sandbox_only"`
	ChecksLastDay   int64 `json:"codes_sent_last_24h"`
	JoinedLastDay   int64 `json:"joined_last_24h"`
	LimitedLastDay  int64 `json:"limits_reached_last_24h"`
	RefusedLastDay  int64 `json:"refused_last_24h"`
	SignedInLastDay int64 `json:"signed_in_last_24h"`
}

func (s *Store) hashCode(salt []byte, phone, code string) []byte {
	m := hmac.New(sha256.New, s.Key)
	m.Write(salt)
	m.Write([]byte{0})
	m.Write([]byte(phone))
	m.Write([]byte{0})
	m.Write([]byte(code))
	return m.Sum(nil)
}

func hashTicket(t string) []byte {
	sum := sha256.Sum256([]byte(t))
	return sum[:]
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func newCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func audit(ctx context.Context, q execer, phone, address, event, detail string) error {
	_, err := q.Exec(ctx, `INSERT INTO phone_signin_events (phone, caller_address, event, detail) VALUES ($1, $2, $3, $4)`,
		phone, address, event, detail)
	if err != nil {
		return fmt.Errorf("phonesignin: audit: %w", err)
	}
	return nil
}

// Refused records a refusal before any check, such as a number in no country.
func (s *Store) Refused(ctx context.Context, phone, address, detail string) error {
	return audit(ctx, s.DB, phone, address, "refused", detail)
}

func lock(ctx context.Context, tx pgx.Tx, key string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key)
	return err
}

// limitFor is when the next text may go, or zero when one may go now: at most one in gap and max in an hour among
// the texts to the number value, or from the caller's address value when byAddress.
func limitFor(ctx context.Context, tx pgx.Tx, byAddress bool, value string, now time.Time, gap time.Duration, max int) (time.Time, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if byAddress {
		rows, err = tx.Query(ctx, `SELECT created_at FROM phone_checks WHERE caller_address = $1 AND created_at > $2 ORDER BY created_at DESC`,
			value, now.Add(-time.Hour))
	} else {
		rows, err = tx.Query(ctx, `SELECT created_at FROM phone_checks WHERE phone = $1 AND created_at > $2 ORDER BY created_at DESC`,
			value, now.Add(-time.Hour))
	}
	if err != nil {
		return time.Time{}, err
	}
	sent, err := pgx.CollectRows(rows, pgx.RowTo[time.Time])
	if err != nil {
		return time.Time{}, err
	}
	var at time.Time
	if gap > 0 && len(sent) > 0 && sent[0].Add(gap).After(now) {
		at = sent[0].Add(gap)
	}
	if len(sent) >= max {
		if h := sent[max-1].Add(time.Hour); h.After(at) {
			at = h
		}
	}
	return at, nil
}

// Start texts a new code to phone (E.164, from NormalisePhone), asked from address. It ends any code sent before.
func (s *Store) Start(ctx context.Context, phone, address string) (Started, error) {
	sender := s.Sender()
	now := s.now()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Started{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lock(ctx, tx, "phone:"+phone); err != nil {
		return Started{}, err
	}
	if err := lock(ctx, tx, "phone-address:"+address); err != nil {
		return Started{}, err
	}
	retry, err := limitFor(ctx, tx, false, phone, now, TextGap, TextsPerHour)
	if err != nil {
		return Started{}, err
	}
	detail := "this number"
	if a, err := limitFor(ctx, tx, true, address, now, 0, max(1, s.PerAddressHour)); err != nil {
		return Started{}, err
	} else if a.After(retry) {
		retry, detail = a, "this address"
	}
	if !retry.IsZero() {
		if err := audit(ctx, tx, phone, address, "limited", detail+" until "+retry.UTC().Format(time.RFC3339)); err != nil {
			return Started{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Started{}, err
		}
		return Started{}, &LimitError{RetryAt: retry}
	}
	code, err := newCode()
	if err != nil {
		return Started{}, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return Started{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE phone_checks SET ended_at = $2 WHERE phone = $1 AND ended_at IS NULL`, phone, now); err != nil {
		return Started{}, err
	}
	expires := now.Add(CodeLife)
	if _, err := tx.Exec(ctx, `INSERT INTO phone_checks (phone, caller_address, code_salt, code_hash, sender, sandbox, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, phone, address, salt, s.hashCode(salt, phone, code), sender.Name(), sender.Sandbox(), now, expires); err != nil {
		return Started{}, err
	}
	if err := audit(ctx, tx, phone, address, "started", "sender "+sender.Name()); err != nil {
		return Started{}, err
	}
	if err := sender.SendCode(ctx, phone, code); err != nil {
		return Started{}, fmt.Errorf("phonesignin: send the code: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Started{}, err
	}
	out := Started{ExpiresAt: expires, Sender: sender.Name()}
	if sender.Sandbox() {
		out.SandboxCode = code
	}
	return out, nil
}

// Finish checks code against the one last sent to phone. A right code ends the check and gives a ticket.
func (s *Store) Finish(ctx context.Context, phone, code, address string) (Finished, error) {
	now := s.now()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Finished{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lock(ctx, tx, "phone:"+phone); err != nil {
		return Finished{}, err
	}
	var (
		id      int64
		salt    []byte
		hash    []byte
		sandbox bool
		wrong   int
	)
	err = tx.QueryRow(ctx, `SELECT id, code_salt, code_hash, sandbox, wrong_tries FROM phone_checks
		WHERE phone = $1 AND ended_at IS NULL AND expires_at > $2 ORDER BY id DESC LIMIT 1`, phone, now).Scan(&id, &salt, &hash, &sandbox, &wrong)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := audit(ctx, tx, phone, address, "refused", "no code waiting"); err != nil {
			return Finished{}, err
		}
		return Finished{}, commitThen(ctx, tx, ErrNoCode)
	}
	if err != nil {
		return Finished{}, err
	}
	// A code the Sandbox sent counts for nothing once a real sender is set.
	if sandbox && !s.Sender().Sandbox() {
		if _, err := tx.Exec(ctx, `UPDATE phone_checks SET ended_at = $2 WHERE id = $1`, id, now); err != nil {
			return Finished{}, err
		}
		if err := audit(ctx, tx, phone, address, "ended", "a Sandbox code under a real sender"); err != nil {
			return Finished{}, err
		}
		return Finished{}, commitThen(ctx, tx, ErrNoCode)
	}
	if !hmac.Equal(hash, s.hashCode(salt, phone, code)) {
		wrong++
		if _, err := tx.Exec(ctx, `UPDATE phone_checks SET wrong_tries = $2, ended_at = CASE WHEN $3 THEN $4::timestamptz END WHERE id = $1`,
			id, wrong, wrong >= MaxWrongTries, now); err != nil {
			return Finished{}, err
		}
		if err := audit(ctx, tx, phone, address, "wrong_code", fmt.Sprintf("try %d of %d", wrong, MaxWrongTries)); err != nil {
			return Finished{}, err
		}
		if wrong >= MaxWrongTries {
			if err := audit(ctx, tx, phone, address, "ended", "five wrong codes"); err != nil {
				return Finished{}, err
			}
		}
		return Finished{}, commitThen(ctx, tx, ErrWrongCode)
	}
	if _, err := tx.Exec(ctx, `UPDATE phone_checks SET ended_at = $2 WHERE id = $1`, id, now); err != nil {
		return Finished{}, err
	}
	var hasPerson bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM people WHERE phone = $1)`, phone).Scan(&hasPerson); err != nil {
		return Finished{}, err
	}
	ticket, err := randomToken(32)
	if err != nil {
		return Finished{}, err
	}
	expires := now.Add(TicketLife)
	if _, err := tx.Exec(ctx, `INSERT INTO phone_tickets (ticket_hash, phone, sandbox, had_person, created_at, expires_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		hashTicket(ticket), phone, sandbox, hasPerson, now, expires); err != nil {
		return Finished{}, err
	}
	if err := audit(ctx, tx, phone, address, "finished", ""); err != nil {
		return Finished{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Finished{}, err
	}
	return Finished{Ticket: ticket, ExpiresAt: expires, HasPerson: hasPerson, Sandbox: sandbox}, nil
}

// refuse undoes tx, so a refused ticket is not used up, and records the refusal.
func (s *Store) refuse(ctx context.Context, tx pgx.Tx, phone, address, detail string, refusal error) error {
	_ = tx.Rollback(ctx)
	if err := audit(ctx, s.DB, phone, address, "refused", detail); err != nil {
		return err
	}
	return refusal
}

// commitThen keeps what the refusal wrote (a wrong try, its audit row) and answers refusal.
func commitThen(ctx context.Context, tx pgx.Tx, refusal error) error {
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return refusal
}

const personColumns = `id, phone, identity, phone_sandbox, phone_confirmed_at, created_at`

func scanPerson(row pgx.Row) (Person, error) {
	var p Person
	err := row.Scan(&p.ID, &p.Phone, &p.Identity, &p.PhoneSandbox, &p.PhoneConfirmedAt, &p.CreatedAt)
	return p, err
}

// Redeem uses ticket, once: the person who holds its number, or a new person for it holding identity (a new
// identity when empty). created says the person is new. A number confirmed by a real text clears its Sandbox mark.
func (s *Store) Redeem(ctx context.Context, ticket, identity, address string) (Person, bool, error) {
	now := s.now()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Person{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var (
		phone   string
		sandbox bool
	)
	err = tx.QueryRow(ctx, `UPDATE phone_tickets SET used_at = $2 WHERE ticket_hash = $1 AND used_at IS NULL AND expires_at > $2
		RETURNING phone, sandbox`, hashTicket(ticket), now).Scan(&phone, &sandbox)
	if errors.Is(err, pgx.ErrNoRows) {
		return Person{}, false, s.refuse(ctx, tx, "", address, "a ticket used, old or never given", ErrTicket)
	}
	if err != nil {
		return Person{}, false, err
	}
	if sandbox && !s.Sender().Sandbox() {
		if err := audit(ctx, tx, phone, address, "refused", "a Sandbox confirmation under a real sender"); err != nil {
			return Person{}, false, err
		}
		return Person{}, false, commitThen(ctx, tx, ErrTicket)
	}
	if err := lock(ctx, tx, "phone:"+phone); err != nil {
		return Person{}, false, err
	}
	p, err := scanPerson(tx.QueryRow(ctx, `SELECT `+personColumns+` FROM people WHERE phone = $1 FOR UPDATE`, phone))
	switch {
	case err == nil:
		if identity != "" && identity != p.Identity {
			return Person{}, false, s.refuse(ctx, tx, phone, address, "the number is another identity's", ErrHeld)
		}
		// A code anyone can read off the Sandbox's answer never signs in a person a real text confirmed.
		if sandbox && !p.PhoneSandbox {
			return Person{}, false, s.refuse(ctx, tx, phone, address, "a Sandbox confirmation for a number a real text confirmed", ErrTicket)
		}
		p, err = scanPerson(tx.QueryRow(ctx, `UPDATE people SET phone_confirmed_at = $2, phone_sandbox = phone_sandbox AND $3
			WHERE id = $1 RETURNING `+personColumns, p.ID, now, sandbox))
		if err != nil {
			return Person{}, false, err
		}
		if err := audit(ctx, tx, phone, address, "signed_in", ""); err != nil {
			return Person{}, false, err
		}
		return p, false, tx.Commit(ctx)
	case !errors.Is(err, pgx.ErrNoRows):
		return Person{}, false, err
	}
	id, err := randomToken(16)
	if err != nil {
		return Person{}, false, err
	}
	if identity == "" {
		identity = "person:" + id
	}
	p, err = scanPerson(tx.QueryRow(ctx, `INSERT INTO people (id, phone, identity, phone_sandbox, phone_confirmed_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $5) RETURNING `+personColumns, "per_"+id, phone, identity, sandbox, now))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Person{}, false, s.refuse(ctx, tx, phone, address, "the identity is another number's", ErrHeld)
	}
	if err != nil {
		return Person{}, false, err
	}
	if err := audit(ctx, tx, phone, address, "joined", ""); err != nil {
		return Person{}, false, err
	}
	return p, true, tx.Commit(ctx)
}

// PhoneCheckDue is whether p's number must be confirmed again by text: it was confirmed only by the Sandbox sender
// and a real sender is set now.
func (s *Store) PhoneCheckDue(p Person) bool { return p.PhoneSandbox && !s.Sender().Sandbox() }

// PersonByPhone is the person who holds phone.
func (s *Store) PersonByPhone(ctx context.Context, phone string) (Person, bool, error) {
	p, err := scanPerson(s.DB.QueryRow(ctx, `SELECT `+personColumns+` FROM people WHERE phone = $1`, phone))
	if errors.Is(err, pgx.ErrNoRows) {
		return Person{}, false, nil
	}
	return p, err == nil, err
}

// Counts is how many people there are and what the door did in the last day.
func (s *Store) Counts(ctx context.Context) (Counts, error) {
	var c Counts
	since := s.now().Add(-24 * time.Hour)
	err := s.DB.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM people),
		(SELECT count(*) FROM people WHERE phone_sandbox),
		(SELECT count(*) FROM phone_checks WHERE created_at > $1),
		(SELECT count(*) FROM phone_signin_events WHERE event = 'joined' AND created_at > $1),
		(SELECT count(*) FROM phone_signin_events WHERE event = 'limited' AND created_at > $1),
		(SELECT count(*) FROM phone_signin_events WHERE event = 'refused' AND created_at > $1),
		(SELECT count(*) FROM phone_signin_events WHERE event = 'signed_in' AND created_at > $1)`, since).
		Scan(&c.People, &c.PeopleSandbox, &c.ChecksLastDay, &c.JoinedLastDay, &c.LimitedLastDay, &c.RefusedLastDay, &c.SignedInLastDay)
	return c, err
}
