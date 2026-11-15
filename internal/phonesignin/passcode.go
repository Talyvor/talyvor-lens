package phonesignin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/argon2"
)

// B61.23 — the passcode: six digits a person sets at sign-up and Lens checks for the BFF. The passcode alone opens
// nothing: a check counts only together with a session of that person (the phone app's lock, CheckPasscode) or a
// fresh phone ticket (RedeemWithPasscode, signing in on a new device). It never approves a payment; approvals keep
// their keys.

// The passcode's limits. Wrong tries count for the person on every replica (passcode_events), since the last right
// try or the last set, in the last day.
const (
	PasscodeLength      = 6
	PasscodeWaitAfter   = 5                // after this many wrong tries, each try waits PasscodeWait after the last wrong one
	PasscodeWait        = 15 * time.Minute // a right passcode included
	PasscodeOffAfter    = 10               // this many wrong tries in a day turn it off
	PasscodeCountWindow = 24 * time.Hour
)

// The slow hash: argon2id at OWASP's smallest memory, of the passcode keyed by LENS_PASSCODE_SECRET.
const (
	passcodeTime    = 2
	passcodeMemory  = 19 * 1024 // KiB
	passcodeThreads = 1
	passcodeKeyLen  = 32
	passcodeParams  = "argon2id;v=19;t=2;m=19456;p=1;pepper=hmac-sha256"
)

var (
	// ErrPasscodeUnavailable: LENS_PASSCODE_SECRET is not set, so no passcode is set or checked.
	ErrPasscodeUnavailable = errors.New("phonesignin: the passcode cannot be set or checked now")
	// ErrPasscodeNeeded: the person has a passcode and the call carried none.
	ErrPasscodeNeeded = errors.New("phonesignin: enter the passcode")
	// ErrWrongPasscode: not the passcode. It never says how close.
	ErrWrongPasscode = errors.New("phonesignin: that passcode is not right")
	// ErrPasscodeOff: ten wrong tries in a day turned it off until the person signs in with a key and sets a new one.
	ErrPasscodeOff = errors.New("phonesignin: the passcode is off after too many wrong tries")
	// ErrNoPasscode: the person has not set one.
	ErrNoPasscode = errors.New("phonesignin: no passcode is set")
)

// PasscodeWaitError is a try refused, or a wrong try answered, with when the next may be made.
type PasscodeWaitError struct{ RetryAt time.Time }

func (e *PasscodeWaitError) Error() string {
	return "phonesignin: too many wrong passcodes; try again at " + e.RetryAt.UTC().Format(time.RFC3339)
}

// WeakPasscodeError is a passcode refused when set, with the words that say why.
type WeakPasscodeError struct{ Reason string }

func (e *WeakPasscodeError) Error() string { return e.Reason }

// CheckPasscodeShape refuses what is not six digits, one digit repeated, a run up or down, and the last six digits
// of the person's own number.
func CheckPasscodeShape(passcode, phone string) error {
	if len(passcode) != PasscodeLength || strings.Trim(passcode, "0123456789") != "" {
		return &WeakPasscodeError{"A passcode is 6 digits."}
	}
	if strings.Count(passcode, passcode[:1]) == PasscodeLength {
		return &WeakPasscodeError{"Choose a passcode that is not one digit repeated."}
	}
	up, down := true, true
	for i := 1; i < PasscodeLength; i++ {
		up = up && passcode[i] == passcode[i-1]+1
		down = down && passcode[i] == passcode[i-1]-1
	}
	if up || down {
		return &WeakPasscodeError{"Choose a passcode that is not a run like 123456 or 654321."}
	}
	if digits := strings.TrimPrefix(phone, "+"); len(digits) >= PasscodeLength && digits[len(digits)-PasscodeLength:] == passcode {
		return &WeakPasscodeError{"Choose a passcode that is not the end of your phone number."}
	}
	return nil
}

func (s *Store) hashPasscode(salt []byte, personID, passcode string) []byte {
	m := hmac.New(sha256.New, s.PasscodeSecret)
	m.Write([]byte("talyvor-passcode-v1"))
	m.Write([]byte{0})
	m.Write([]byte(personID))
	m.Write([]byte{0})
	m.Write([]byte(passcode))
	return argon2.IDKey(m.Sum(nil), salt, passcodeTime, passcodeMemory, passcodeThreads, passcodeKeyLen)
}

func (s *Store) passcodeEvent(ctx context.Context, q execer, personID, address, event string) error {
	_, err := q.Exec(ctx, `INSERT INTO passcode_events (person_id, caller_address, event, created_at) VALUES ($1, $2, $3, $4)`,
		personID, address, event, s.now())
	if err != nil {
		return fmt.Errorf("phonesignin: passcode audit: %w", err)
	}
	return nil
}

func hasPasscode(ctx context.Context, tx pgx.Tx, personID string) (bool, error) {
	var has bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM person_passcodes WHERE person_id = $1)`, personID).Scan(&has)
	return has, err
}

// setPasscode keeps passcode for the person, on again if it was off; event says how, and clears the count.
func (s *Store) setPasscode(ctx context.Context, tx pgx.Tx, personID, phone, passcode, address, event string) error {
	if len(s.PasscodeSecret) == 0 {
		return ErrPasscodeUnavailable
	}
	if err := CheckPasscodeShape(passcode, phone); err != nil {
		return err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO person_passcodes (person_id, salt, hash, hash_params, set_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (person_id) DO UPDATE SET salt = EXCLUDED.salt, hash = EXCLUDED.hash, hash_params = EXCLUDED.hash_params,
		set_at = EXCLUDED.set_at, off_at = NULL`, personID, salt, s.hashPasscode(salt, personID, passcode), passcodeParams, s.now()); err != nil {
		return err
	}
	return s.passcodeEvent(ctx, tx, personID, address, event)
}

// checkPasscode tries passcode for the person inside tx, holding the person's lock so every replica counts the same
// tries. Whatever it answers, the caller commits what it wrote: a wrong try counts even when the call is refused.
func (s *Store) checkPasscode(ctx context.Context, tx pgx.Tx, personID, passcode, address string) error {
	if len(s.PasscodeSecret) == 0 {
		return ErrPasscodeUnavailable
	}
	if err := lock(ctx, tx, "passcode:"+personID); err != nil {
		return err
	}
	var (
		salt, hash []byte
		offAt      *time.Time
	)
	err := tx.QueryRow(ctx, `SELECT salt, hash, off_at FROM person_passcodes WHERE person_id = $1 FOR UPDATE`, personID).Scan(&salt, &hash, &offAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoPasscode
	}
	if err != nil {
		return err
	}
	if offAt != nil {
		if err := s.passcodeEvent(ctx, tx, personID, address, "refused_off"); err != nil {
			return err
		}
		return ErrPasscodeOff
	}
	now := s.now()
	var (
		wrong     int
		lastWrong *time.Time
	)
	if err := tx.QueryRow(ctx, `SELECT count(*), max(created_at) FROM passcode_events
		WHERE person_id = $1 AND event = 'wrong' AND created_at > $2
		  AND id > COALESCE((SELECT max(id) FROM passcode_events WHERE person_id = $1 AND event IN ('set', 'changed', 'set_after_key', 'right')), 0)`,
		personID, now.Add(-PasscodeCountWindow)).Scan(&wrong, &lastWrong); err != nil {
		return err
	}
	if wrong >= PasscodeWaitAfter && lastWrong != nil && lastWrong.Add(PasscodeWait).After(now) {
		if err := s.passcodeEvent(ctx, tx, personID, address, "waited"); err != nil {
			return err
		}
		return &PasscodeWaitError{RetryAt: lastWrong.Add(PasscodeWait)}
	}
	if hmac.Equal(hash, s.hashPasscode(salt, personID, passcode)) {
		return s.passcodeEvent(ctx, tx, personID, address, "right")
	}
	wrong++
	if err := s.passcodeEvent(ctx, tx, personID, address, "wrong"); err != nil {
		return err
	}
	switch {
	case wrong >= PasscodeOffAfter:
		if _, err := tx.Exec(ctx, `UPDATE person_passcodes SET off_at = $2 WHERE person_id = $1`, personID, now); err != nil {
			return err
		}
		if err := s.passcodeEvent(ctx, tx, personID, address, "off"); err != nil {
			return err
		}
		return ErrPasscodeOff
	case wrong >= PasscodeWaitAfter:
		return &PasscodeWaitError{RetryAt: now.Add(PasscodeWait)}
	}
	return ErrWrongPasscode
}

// CheckPasscode is the phone app's lock: the BFF holds a session of the person and asks whether passcode is theirs.
// nil is right; a wrong try is counted whatever the answer.
func (s *Store) CheckPasscode(ctx context.Context, personID, passcode, address string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.checkPasscode(ctx, tx, personID, passcode, address); err != nil {
		if errors.Is(err, ErrNoPasscode) || errors.Is(err, ErrPasscodeUnavailable) {
			return err
		}
		return commitThen(ctx, tx, err)
	}
	return tx.Commit(ctx)
}

// ChangePasscode sets passcode for a person who gave the old one. A weak new one is refused before the old is tried.
func (s *Store) ChangePasscode(ctx context.Context, personID, old, passcode, address string) error {
	return s.changePasscode(ctx, personID, passcode, address, "changed", func(tx pgx.Tx) error {
		return s.checkPasscode(ctx, tx, personID, old, address)
	})
}

// SetPasscodeAfterKey sets passcode for a person who has just signed in with a key — a passkey, or their number
// confirmed by text and a key (B61.24 calls it). It is the one way an off passcode comes back on, and the way to
// change a passcode without the old one.
func (s *Store) SetPasscodeAfterKey(ctx context.Context, personID, passcode, address string) error {
	return s.changePasscode(ctx, personID, passcode, address, "set_after_key", nil)
}

func (s *Store) changePasscode(ctx context.Context, personID, passcode, address, event string, prove func(pgx.Tx) error) error {
	if len(s.PasscodeSecret) == 0 {
		return ErrPasscodeUnavailable
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var phone string
	if err := tx.QueryRow(ctx, `SELECT phone FROM people WHERE id = $1`, personID).Scan(&phone); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoPasscode
		}
		return err
	}
	if err := CheckPasscodeShape(passcode, phone); err != nil {
		return err
	}
	if prove != nil {
		if err := prove(tx); err != nil {
			if errors.Is(err, ErrNoPasscode) {
				return err
			}
			return commitThen(ctx, tx, err)
		}
	}
	if err := s.setPasscode(ctx, tx, personID, phone, passcode, address, event); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
