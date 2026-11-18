package phonesignin

// B61.24 — a passkey signs its person in. The key made at sign-up's key step (the web passkey or the phone app's
// device key) is kept for the person too, and answers a sign-in challenge with one Face ID touch: the web's is a
// passkey the browser offers with no number typed, so a sign-in challenge names no person unless the BFF has one.
//
// A sign-in challenge is used once, within 5 minutes, and is bound to the origin it was asked from (none for the
// phone's key). The answer must have the user verified, come from that origin and move the key's counter forward, as
// approvals check them (internal/passkey); a removed key signs nobody in. It gives the person and a one-use ticket.
//
// These keys only sign a person in. A workspace's approval keys stay its own (workspace_passkeys and
// workspace_device_keys): nothing here reads or writes them, so a sign-in gives no approval power.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/workspace"
)

// The rules of a key sign-in.
const (
	KeyChallengeLife = 5 * time.Minute // a challenge is good for this long, once
	KeyTicketLife    = 5 * time.Minute // a ticket is good for this long, once
	KeyRowsKept      = 24 * time.Hour  // a challenge or ticket is deleted this long after its time
)

var (
	// ErrKeysOff: this server has no relying party for passkeys or no checker for device keys.
	ErrKeysOff = errors.New("phonesignin: sign-in keys are not set up on this server")
	// ErrKeyChallenge: the challenge was not given here, was used, is older than 5 minutes, or is for another kind of
	// answer.
	ErrKeyChallenge = errors.New("phonesignin: that sign-in has ended; start again")
	// ErrKeyOrigin: the sign-in was asked from an origin passkeys may not come from, or answered from another one.
	ErrKeyOrigin = errors.New("phonesignin: that sign-in came from somewhere it may not")
	// ErrNoKey: no kept key answers — never one, removed, or another person's.
	ErrNoKey = errors.New("phonesignin: that key signs nobody in")
	// ErrKeyRefused: the key's answer does not verify (the signature, the user not verified, the counter).
	ErrKeyRefused = errors.New("phonesignin: that key's answer is not valid")
	// ErrKeyKept: the key is kept already, or was removed and is never kept again.
	ErrKeyKept = errors.New("phonesignin: that key is kept already")
	// ErrKeyProof: adding a key, once the person has a key or a passcode, or removing one, needs the passcode or a key
	// they have.
	ErrKeyProof = errors.New("phonesignin: adding a key needs your passcode or a key you already have")
	// ErrLastWayIn: the key is the person's last way in and they have no passcode.
	ErrLastWayIn = errors.New("phonesignin: that key is your last way in")
	// ErrNoPerson: no such person.
	ErrNoPerson = errors.New("phonesignin: no such person")
)

// PasscodeRule is the person's passcode as the key rules ask about it (B61.23). Check runs inside tx and the caller
// keeps what it wrote, so a wrong try counts.
type PasscodeRule interface {
	HasPasscode(ctx context.Context, tx pgx.Tx, personID string) (bool, error)
	CheckPasscode(ctx context.Context, tx pgx.Tx, personID, passcode, address string) error
}

// KeyChallenge is a challenge for a key to answer.
type KeyChallenge struct {
	Challenge string    `json:"challenge"`
	RPID      string    `json:"rp_id,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	// UserHandle is the person's handle for a new passkey (WebAuthn user.id): the browser keeps it with the key, so
	// it can offer the key with no number typed. Registration only.
	UserHandle string `json:"user_handle,omitempty"`
}

// PersonKey is one of a person's sign-in keys.
type PersonKey struct {
	ID           string     `json:"id"`
	Kind         string     `json:"kind"` // passkey or device
	CredentialID string     `json:"credential_id,omitempty"`
	Platform     string     `json:"platform,omitempty"`
	Name         string     `json:"name"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
}

// KeySignIn is a key that answered: its person, and a one-use ticket saying they signed in with it just now.
type KeySignIn struct {
	Person    Person
	KeyID     string
	Ticket    string
	ExpiresAt time.Time
}

// NewKey is a key to keep for a person: a passkey's registration or a device key's, one of them.
type NewKey struct {
	Name    string
	Passkey *passkey.Registration
	Device  *devicekey.Registration
}

// KeyProof is how a person adding a key shows it is them: a ticket from signing in with a key they have, or the
// passcode.
type KeyProof struct {
	Ticket   string
	Passcode string
}

// passcodes is the passcode rule the key rules ask: Store.Passcodes, or the door's own when none is wired (B50.52),
// so a person with a passcode is never read as having none.
func (s *Store) passcodes() PasscodeRule {
	if s.Passcodes != nil {
		return s.Passcodes
	}
	return s.PasscodeRule()
}

func (s *Store) passcodeSet(ctx context.Context, tx pgx.Tx, personID string) (bool, error) {
	return s.passcodes().HasPasscode(ctx, tx, personID)
}

func (s *Store) newKeyChallenge(ctx context.Context, q execer, purpose, personID, origin, challenge string) (KeyChallenge, error) {
	if challenge == "" {
		c, err := randomToken(32)
		if err != nil {
			return KeyChallenge{}, err
		}
		challenge = c
	}
	if _, err := q.Exec(ctx, `DELETE FROM person_key_challenges WHERE expires_at < $1`, s.now().Add(-KeyRowsKept)); err != nil {
		return KeyChallenge{}, fmt.Errorf("phonesignin: old key challenges: %w", err)
	}
	expires := s.now().Add(KeyChallengeLife)
	var person any
	if personID != "" {
		person = personID
	}
	if _, err := q.Exec(ctx, `INSERT INTO person_key_challenges (challenge, purpose, person_id, origin, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, challenge, purpose, person, origin, s.now(), expires); err != nil {
		return KeyChallenge{}, fmt.Errorf("phonesignin: key challenge: %w", err)
	}
	out := KeyChallenge{Challenge: challenge, ExpiresAt: expires}
	if s.RP != nil {
		out.RPID = s.RP.ID
	}
	return out, nil
}

func (s *Store) personExists(ctx context.Context, q pgx.Row) error {
	var one int
	if err := q.Scan(&one); errors.Is(err, pgx.ErrNoRows) {
		return ErrNoPerson
	} else if err != nil {
		return err
	}
	return nil
}

// SignInChallenge is a challenge a key answers to sign in. origin is where the web asks from, one passkeys may come
// from; "" is the phone app, whose device key answers. personID, when the BFF knows the person, lets only their keys
// answer; "" lets any person's key answer, so the browser can offer one with no number typed.
func (s *Store) SignInChallenge(ctx context.Context, origin, personID string) (KeyChallenge, error) {
	if origin != "" {
		if s.RP == nil {
			return KeyChallenge{}, ErrKeysOff
		}
		if !slices.Contains(s.RP.Origins, origin) {
			return KeyChallenge{}, ErrKeyOrigin
		}
	}
	if personID != "" {
		if err := s.personExists(ctx, s.DB.QueryRow(ctx, `SELECT 1 FROM people WHERE id = $1`, personID)); err != nil {
			return KeyChallenge{}, err
		}
	}
	return s.newKeyChallenge(ctx, s.DB, "signin", personID, origin, "")
}

// RegisterChallenge is the challenge a new key for personID is made over. challenge, when not empty, is one Lens has
// just issued for the person's account's approval key, so the one ceremony at sign-up's key step makes both.
func (s *Store) RegisterChallenge(ctx context.Context, personID, challenge string) (KeyChallenge, error) {
	if err := s.personExists(ctx, s.DB.QueryRow(ctx, `SELECT 1 FROM people WHERE id = $1`, personID)); err != nil {
		return KeyChallenge{}, err
	}
	c, err := s.newKeyChallenge(ctx, s.DB, "register", personID, "", challenge)
	if err != nil {
		return KeyChallenge{}, err
	}
	c.UserHandle = base64.RawURLEncoding.EncodeToString([]byte(personID))
	return c, nil
}

type keyRow struct {
	id, personID string
	kind         string
	spki         []byte
	count        int64
	removed      bool
}

// keyRefused undoes tx and records why a key signed nobody in.
func (s *Store) keyRefused(ctx context.Context, tx pgx.Tx, phone, address, detail string, refusal error) error {
	return s.refuseEvent(ctx, tx, phone, address, "key_refused", detail, refusal)
}

func (s *Store) refuseEvent(ctx context.Context, tx pgx.Tx, phone, address, event, detail string, refusal error) error {
	if tx != nil {
		_ = tx.Rollback(ctx)
	}
	if err := audit(ctx, s.DB, phone, address, event, detail); err != nil {
		return err
	}
	return refusal
}

func phoneOf(ctx context.Context, tx pgx.Tx, personID string) string {
	var phone string
	_ = tx.QueryRow(ctx, `SELECT phone FROM people WHERE id = $1`, personID).Scan(&phone)
	return phone
}

// SignInWithKey checks a's answer to a sign-in challenge and gives the person whose key it is, with a ticket. The
// challenge is used by any answer to it, right or wrong.
func (s *Store) SignInWithKey(ctx context.Context, a passkey.Assertion, address string) (KeySignIn, error) {
	device := a.DeviceID != ""
	var (
		challenge, answeredFrom string
		cdj                     []byte
	)
	if device {
		challenge = a.Challenge
	} else {
		if s.RP == nil {
			return KeySignIn{}, ErrKeysOff
		}
		var err error
		if cdj, err = passkey.Decode64(a.ClientDataJSON); err != nil {
			return KeySignIn{}, s.keyRefused(ctx, nil, "", address, "an answer that is not base64url", ErrKeyRefused)
		}
		challenge = passkey.ChallengeOf(cdj)
		var cd struct {
			Origin string `json:"origin"`
		}
		_ = json.Unmarshal(cdj, &cd)
		answeredFrom = cd.Origin
	}
	if challenge == "" {
		return KeySignIn{}, s.keyRefused(ctx, nil, "", address, "an answer to no challenge", ErrKeyChallenge)
	}
	now := s.now()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return KeySignIn{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var (
		forPerson *string
		origin    string
	)
	err = tx.QueryRow(ctx, `UPDATE person_key_challenges SET used_at = $2
		WHERE challenge = $1 AND purpose = 'signin' AND used_at IS NULL AND expires_at > $2 RETURNING person_id, origin`,
		challenge, now).Scan(&forPerson, &origin)
	if errors.Is(err, pgx.ErrNoRows) {
		return KeySignIn{}, s.keyRefused(ctx, tx, "", address, "a challenge used, old or never given", ErrKeyChallenge)
	}
	if err != nil {
		return KeySignIn{}, err
	}
	// From here on the challenge is spent whatever the answer: a refusal commits that, and its audit row.
	refuse := func(phone, detail string, refusal error) error {
		if err := audit(ctx, tx, phone, address, "key_refused", detail); err != nil {
			return err
		}
		return commitThen(ctx, tx, refusal)
	}
	if device != (origin == "") {
		return KeySignIn{}, refuse("", "an answer of the other kind of key", ErrKeyChallenge)
	}
	var k keyRow
	if device {
		err = tx.QueryRow(ctx, `SELECT id, person_id, kind, public_key, sign_count, removed_at IS NOT NULL FROM person_keys
			WHERE id = $1 AND kind = 'device' FOR UPDATE`, a.DeviceID).Scan(&k.id, &k.personID, &k.kind, &k.spki, &k.count, &k.removed)
	} else {
		err = tx.QueryRow(ctx, `SELECT id, person_id, kind, public_key, sign_count, removed_at IS NOT NULL FROM person_keys
			WHERE credential_id = $1 AND kind = 'passkey' FOR UPDATE`, a.CredentialID).Scan(&k.id, &k.personID, &k.kind, &k.spki, &k.count, &k.removed)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return KeySignIn{}, refuse("", "a key never kept", ErrNoKey)
	}
	if err != nil {
		return KeySignIn{}, err
	}
	phone := phoneOf(ctx, tx, k.personID)
	switch {
	case k.removed:
		return KeySignIn{}, refuse(phone, "a removed key", ErrNoKey)
	case forPerson != nil && *forPerson != k.personID:
		return KeySignIn{}, refuse(phone, "another person's key", ErrNoKey)
	}
	next := k.count
	if device {
		if err := devicekey.Verify(k.spki, challenge, a.Signature); err != nil {
			return KeySignIn{}, refuse(phone, "a device signature that does not verify", ErrKeyRefused)
		}
	} else {
		if answeredFrom != origin {
			return KeySignIn{}, refuse(phone, "answered from another origin than it was asked from", ErrKeyOrigin)
		}
		n, err := s.RP.VerifyAssertion(a, challenge, k.spki, uint32(k.count))
		if err != nil {
			return KeySignIn{}, refuse(phone, strings.TrimPrefix(err.Error(), passkey.ErrInvalid.Error()+": "), ErrKeyRefused)
		}
		next = int64(n)
	}
	if _, err := tx.Exec(ctx, `UPDATE person_keys SET sign_count = $2, last_used_at = $3 WHERE id = $1`, k.id, next, now); err != nil {
		return KeySignIn{}, err
	}
	ticket, err := randomToken(32)
	if err != nil {
		return KeySignIn{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM person_key_tickets WHERE expires_at < $1`, now.Add(-KeyRowsKept)); err != nil {
		return KeySignIn{}, err
	}
	expires := now.Add(KeyTicketLife)
	if _, err := tx.Exec(ctx, `INSERT INTO person_key_tickets (ticket_hash, person_id, key_id, created_at, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		hashTicket(ticket), k.personID, k.id, now, expires); err != nil {
		return KeySignIn{}, err
	}
	p, err := scanPerson(tx.QueryRow(ctx, `SELECT `+personColumns+` FROM people WHERE id = $1`, k.personID))
	if err != nil {
		return KeySignIn{}, err
	}
	if err := audit(ctx, tx, phone, address, "key_signed_in", k.kind); err != nil {
		return KeySignIn{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return KeySignIn{}, err
	}
	return KeySignIn{Person: p, KeyID: k.id, Ticket: ticket, ExpiresAt: expires}, nil
}

// useKeyTicket spends a ticket personID got from signing in with a key, inside tx. It is good only while that key
// is not removed.
func useKeyTicket(ctx context.Context, tx pgx.Tx, ticket, personID string, now time.Time) error {
	if ticket == "" {
		return ErrKeyProof
	}
	tag, err := tx.Exec(ctx, `UPDATE person_key_tickets t SET used_at = $3 FROM person_keys k
		WHERE t.ticket_hash = $1 AND t.person_id = $2 AND t.used_at IS NULL AND t.expires_at > $3
			AND k.id = t.key_id AND k.removed_at IS NULL`, hashTicket(ticket), personID, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrKeyProof
	}
	return nil
}

func liveKeys(ctx context.Context, tx pgx.Tx, personID string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM person_keys WHERE person_id = $1 AND removed_at IS NULL`, personID).Scan(&n)
	return n, err
}

// AddKey keeps k as personID's sign-in key, made over a challenge from RegisterChallenge. The BFF calls it for the
// person whose session it holds. The person's first key, with no passcode set, is sign-up's key step and needs
// nothing more; after that a key is added only with proof: a ticket from signing in with a key they have, or the
// passcode.
func (s *Store) AddKey(ctx context.Context, personID string, k NewKey, proof KeyProof, address string) (PersonKey, error) {
	var (
		challenge, kind, credID, platform string
		spki                              []byte
		count                             int64
	)
	switch {
	case k.Passkey != nil && k.Device == nil:
		if s.RP == nil {
			return PersonKey{}, ErrKeysOff
		}
		challenge = passkey.ChallengeOf(k.Passkey.ClientDataJSON)
		n, err := s.RP.VerifyRegistration(*k.Passkey, challenge)
		if err != nil {
			return PersonKey{}, fmt.Errorf("%w: %v", ErrKeyRefused, err)
		}
		kind, credID, spki, count = "passkey", k.Passkey.CredentialID, k.Passkey.PublicKey, int64(n)
	case k.Device != nil && k.Passkey == nil:
		if s.Devices == nil {
			return PersonKey{}, ErrKeysOff
		}
		if err := s.Devices.VerifyRegistration(*k.Device); err != nil {
			return PersonKey{}, fmt.Errorf("%w: %v", ErrKeyRefused, err)
		}
		challenge, kind, platform, spki = k.Device.Challenge, "device", k.Device.Platform, k.Device.PublicKey
	default:
		return PersonKey{}, fmt.Errorf("%w: give a passkey or a device key, one of them", ErrKeyRefused)
	}
	now := s.now()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return PersonKey{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lock(ctx, tx, "person-keys:"+personID); err != nil {
		return PersonKey{}, err
	}
	phone := phoneOf(ctx, tx, personID)
	if phone == "" {
		return PersonKey{}, ErrNoPerson
	}
	tag, err := tx.Exec(ctx, `UPDATE person_key_challenges SET used_at = $3
		WHERE challenge = $1 AND purpose = 'register' AND person_id = $2 AND used_at IS NULL AND expires_at > $3`, challenge, personID, now)
	if err != nil {
		return PersonKey{}, err
	}
	if tag.RowsAffected() == 0 {
		return PersonKey{}, s.refuseEvent(ctx, tx, phone, address, "key_refused", "a new key over no open challenge of the person's", ErrKeyChallenge)
	}
	var kept bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM person_keys WHERE credential_id = $1 OR public_key = $2)`, nullIfEmpty(credID), spki).Scan(&kept); err != nil {
		return PersonKey{}, err
	}
	if kept {
		return PersonKey{}, s.refuseEvent(ctx, tx, phone, address, "key_refused", "a key kept already or removed", ErrKeyKept)
	}
	live, err := liveKeys(ctx, tx, personID)
	if err != nil {
		return PersonKey{}, err
	}
	hasPasscode, err := s.passcodeSet(ctx, tx, personID)
	if err != nil {
		return PersonKey{}, err
	}
	if live > 0 || hasPasscode {
		if err := s.useProof(ctx, tx, personID, phone, address, "a new key", proof, hasPasscode, now); err != nil {
			return PersonKey{}, err
		}
	}
	id, err := randomToken(16)
	if err != nil {
		return PersonKey{}, err
	}
	pk, err := scanPersonKey(tx.QueryRow(ctx, `INSERT INTO person_keys (id, person_id, kind, credential_id, platform, name, public_key, sign_count, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+personKeyColumns,
		"pky_"+id, personID, kind, nullIfEmpty(credID), platform, strings.TrimSpace(k.Name), spki, count, now))
	if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// The same key, added at the same instant for another person, was kept first.
		return PersonKey{}, s.refuseEvent(ctx, tx, phone, address, "key_refused", "a key kept already or removed", ErrKeyKept)
	}
	if err != nil {
		return PersonKey{}, err
	}
	if err := audit(ctx, tx, phone, address, "key_added", kind); err != nil {
		return PersonKey{}, err
	}
	return pk, tx.Commit(ctx)
}

// useProof spends proof that it is personID, inside tx, for what (a new key, or removing one): a ticket from signing
// in with a key they have, or the passcode when one is set. A refusal undoes tx, except a wrong passcode, whose try
// the passcode counts and tx keeps.
func (s *Store) useProof(ctx context.Context, tx pgx.Tx, personID, phone, address, what string, proof KeyProof, hasPasscode bool, now time.Time) error {
	switch {
	case proof.Ticket != "":
		if err := useKeyTicket(ctx, tx, proof.Ticket, personID, now); err != nil {
			return s.refuseEvent(ctx, tx, phone, address, "key_refused", what+" with a ticket that is not the person's, used, old or its key's removed", err)
		}
	case proof.Passcode != "" && hasPasscode:
		if err := s.passcodes().CheckPasscode(ctx, tx, personID, proof.Passcode, address); err != nil {
			if aerr := audit(ctx, tx, phone, address, "key_refused", what+" with a passcode that was not right"); aerr != nil {
				return aerr
			}
			return commitThen(ctx, tx, err)
		}
	default:
		return s.refuseEvent(ctx, tx, phone, address, "key_refused", what+" with no proof", ErrKeyProof)
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const personKeyColumns = `id, kind, COALESCE(credential_id, ''), platform, name, created_at, last_used_at`

func scanPersonKey(row pgx.Row) (PersonKey, error) {
	var k PersonKey
	err := row.Scan(&k.ID, &k.Kind, &k.CredentialID, &k.Platform, &k.Name, &k.CreatedAt, &k.LastUsedAt)
	return k, err
}

// PersonIdentity is the identity personID holds, the one POST /v1/provision is given for them.
func (s *Store) PersonIdentity(ctx context.Context, personID string) (string, error) {
	var identity string
	err := s.DB.QueryRow(ctx, `SELECT identity FROM people WHERE id = $1`, personID).Scan(&identity)
	identity = workspace.IdentityFromStored(identity)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoPerson
	}
	return identity, err
}

// ListKeys is personID's sign-in keys, oldest first; removed ones are gone from it.
func (s *Store) ListKeys(ctx context.Context, personID string) ([]PersonKey, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+personKeyColumns+` FROM person_keys WHERE person_id = $1 AND removed_at IS NULL ORDER BY created_at, id`, personID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PersonKey, error) { return scanPersonKey(r) })
	if out == nil {
		out = []PersonKey{}
	}
	return out, err
}

// RemoveKey removes personID's key keyID: it signs nobody in from now on. It takes the proof AddKey does: a ticket from
// signing in with a key the person has (the one going included), or the passcode. The person's last key is never
// removed while they have no passcode, because it is their last way in.
func (s *Store) RemoveKey(ctx context.Context, personID, keyID string, proof KeyProof, address string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lock(ctx, tx, "person-keys:"+personID); err != nil {
		return err
	}
	var kind string
	err = tx.QueryRow(ctx, `SELECT kind FROM person_keys WHERE id = $1 AND person_id = $2 AND removed_at IS NULL FOR UPDATE`, keyID, personID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoKey
	}
	if err != nil {
		return err
	}
	phone := phoneOf(ctx, tx, personID)
	live, err := liveKeys(ctx, tx, personID)
	if err != nil {
		return err
	}
	hasPasscode, err := s.passcodeSet(ctx, tx, personID)
	if err != nil {
		return err
	}
	if live <= 1 && !hasPasscode {
		return s.refuseEvent(ctx, tx, phone, address, "key_refused", "the last way in, with no passcode", ErrLastWayIn)
	}
	now := s.now()
	if err := s.useProof(ctx, tx, personID, phone, address, "removing a key", proof, hasPasscode, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE person_keys SET removed_at = $2 WHERE id = $1`, keyID, now); err != nil {
		return err
	}
	if err := audit(ctx, tx, phone, address, "key_removed", kind); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
