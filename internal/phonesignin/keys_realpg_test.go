package phonesignin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
)

// B61.24 — a passkey signs its person in, on a migrated schema.

const (
	testRPID           = "app.talyvor.test"
	testOrigin         = "https://app.talyvor.test"
	otherAllowedOrigin = "https://talyvor.test"
)

var b64 = base64.RawURLEncoding

// authenticator stands in for a browser's passkey: a P-256 key, its credential id and its counter.
type authenticator struct {
	key   *ecdsa.PrivateKey
	id    []byte
	count uint32
}

func newAuthenticator(t *testing.T) *authenticator {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &authenticator{key: k, id: id}
}

func (a *authenticator) spki(t *testing.T) []byte {
	der, err := x509.MarshalPKIXPublicKey(&a.key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func authData(flags byte, count uint32, credID []byte) []byte {
	rp := sha256.Sum256([]byte(testRPID))
	out := append([]byte{}, rp[:]...)
	out = append(out, flags)
	out = binary.BigEndian.AppendUint32(out, count)
	if credID != nil {
		out = append(out, make([]byte, 16)...) // AAGUID
		out = binary.BigEndian.AppendUint16(out, uint16(len(credID)))
		out = append(out, credID...)
	}
	return out
}

func clientData(typ, challenge, origin string) []byte {
	b, _ := json.Marshal(map[string]string{"type": typ, "challenge": challenge, "origin": origin})
	return b
}

func (a *authenticator) register(t *testing.T, challenge string) *passkey.Registration {
	return &passkey.Registration{CredentialID: b64.EncodeToString(a.id), PublicKey: a.spki(t),
		ClientDataJSON: clientData("webauthn.create", challenge, testOrigin), AuthenticatorData: authData(0x45, 0, a.id)}
}

// answer signs a sign-in challenge from origin; verified says Face ID was used.
func (a *authenticator) answer(t *testing.T, challenge, origin string, verified bool) passkey.Assertion {
	a.count++
	flags := byte(0x01)
	if verified {
		flags |= 0x04
	}
	ad := authData(flags, a.count, nil)
	cdj := clientData("webauthn.get", challenge, origin)
	h := sha256.Sum256(cdj)
	signed := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, signed[:])
	if err != nil {
		t.Fatal(err)
	}
	return passkey.Assertion{CredentialID: b64.EncodeToString(a.id), ClientDataJSON: b64.EncodeToString(cdj),
		AuthenticatorData: b64.EncodeToString(ad), Signature: b64.EncodeToString(sig)}
}

func deviceSign(t *testing.T, k *ecdsa.PrivateKey, challenge string) string {
	h := sha256.Sum256(devicekey.Message(challenge))
	sig, err := ecdsa.SignASN1(rand.Reader, k, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return b64.EncodeToString(sig)
}

// fakePasscodes stands in for B61.23's passcode: has says the person set one, right is it.
type fakePasscodes struct {
	has   bool
	right string
}

func (f fakePasscodes) HasPasscode(context.Context, pgx.Tx, string) (bool, error) { return f.has, nil }
func (f fakePasscodes) CheckPasscode(_ context.Context, _ pgx.Tx, _, passcode, _ string) error {
	if !f.has {
		return errors.New("no passcode")
	}
	if passcode != f.right {
		return errors.New("wrong passcode")
	}
	return nil
}

func keyStore(t *testing.T) (*Store, *pgxpool.Pool) {
	pool := phoneDB(t)
	return &Store{DB: pool, Sender: func() partners.TextSender { return &recordingSender{sandbox: true} }, Key: []byte("k"),
		RP:      &passkey.RelyingParty{ID: testRPID, Origins: []string{testOrigin, otherAllowedOrigin}},
		Devices: &devicekey.Verifier{AllowTest: true}}, pool
}

func newPerson(t *testing.T, pool *pgxpool.Pool, id, phone string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO people (id, phone, identity, phone_sandbox, phone_confirmed_at)
		VALUES ($1, $2, $3, true, now())`, id, phone, "person:"+id); err != nil {
		t.Fatal(err)
	}
}

// signUpKey is sign-up's key step: the person's first passkey, kept with no other proof.
func signUpKey(t *testing.T, s *Store, personID string) *authenticator {
	t.Helper()
	ctx := context.Background()
	c, err := s.RegisterChallenge(ctx, personID, "")
	if err != nil {
		t.Fatal(err)
	}
	a := newAuthenticator(t)
	if _, err := s.AddKey(ctx, personID, NewKey{Name: "This Mac", Passkey: a.register(t, c.Challenge)}, KeyProof{}, "203.0.113.1"); err != nil {
		t.Fatalf("sign-up's key: %v", err)
	}
	return a
}

func signIn(t *testing.T, s *Store, a *authenticator, personID string) (KeySignIn, error) {
	t.Helper()
	c, err := s.SignInChallenge(context.Background(), testOrigin, personID)
	if err != nil {
		t.Fatal(err)
	}
	return s.SignInWithKey(context.Background(), a.answer(t, c.Challenge, testOrigin, true), "203.0.113.1")
}

func TestB6124KeyMadeAtSignUpSignsIn(t *testing.T) {
	s, pool := keyStore(t)
	ctx := context.Background()
	newPerson(t, pool, "per_a", "+447700900001")
	a := signUpKey(t, s, "per_a")

	// The browser offers the key with no number typed: the challenge names no person.
	got, err := signIn(t, s, a, "")
	if err != nil {
		t.Fatalf("sign in with the sign-up key: %v", err)
	}
	if got.Person.ID != "per_a" || got.Ticket == "" || got.KeyID == "" {
		t.Fatalf("sign-in = %+v, want per_a with a ticket", got)
	}
	// The ticket is kept only as a hash.
	var plain int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM person_key_tickets WHERE ticket_hash = $1`, []byte(got.Ticket)).Scan(&plain); err != nil || plain != 0 {
		t.Fatalf("the ticket is kept in plain (%d, %v)", plain, err)
	}
	// Signing in gives no approval power: no workspace key appears.
	var approvalKeys int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM workspace_passkeys) + (SELECT count(*) FROM workspace_device_keys)`).Scan(&approvalKeys); err != nil || approvalKeys != 0 {
		t.Fatalf("a sign-in key became an approval key (%d, %v)", approvalKeys, err)
	}

	// The phone's key, made at the phone's sign-up, signs in from the app, which asks with no origin.
	newPerson(t, pool, "per_p", "+447700900002")
	dk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&dk.PublicKey)
	rc, err := s.RegisterChallenge(ctx, "per_p", "")
	if err != nil {
		t.Fatal(err)
	}
	pk, err := s.AddKey(ctx, "per_p", NewKey{Name: "iPhone", Device: &devicekey.Registration{Platform: devicekey.PlatformTest,
		PublicKey: spki, Challenge: rc.Challenge, Signature: deviceSign(t, dk, rc.Challenge)}}, KeyProof{}, "")
	if err != nil {
		t.Fatalf("the phone's key: %v", err)
	}
	sc, err := s.SignInChallenge(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	phoneIn, err := s.SignInWithKey(ctx, passkey.Assertion{DeviceID: pk.ID, Challenge: sc.Challenge, Signature: deviceSign(t, dk, sc.Challenge)}, "")
	if err != nil || phoneIn.Person.ID != "per_p" {
		t.Fatalf("the phone's key signs in = %+v, %v", phoneIn, err)
	}
}

func TestB6124KeySignInRefusals(t *testing.T) {
	s, pool := keyStore(t)
	ctx := context.Background()
	newPerson(t, pool, "per_a", "+447700900011")
	newPerson(t, pool, "per_b", "+447700900012")
	a := signUpKey(t, s, "per_a")
	b := signUpKey(t, s, "per_b")

	t.Run("a replayed answer", func(t *testing.T) {
		c, _ := s.SignInChallenge(ctx, testOrigin, "")
		ans := a.answer(t, c.Challenge, testOrigin, true)
		if _, err := s.SignInWithKey(ctx, ans, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SignInWithKey(ctx, ans, ""); !errors.Is(err, ErrKeyChallenge) {
			t.Fatalf("replay = %v, want ErrKeyChallenge", err)
		}
	})
	t.Run("a wrong origin", func(t *testing.T) {
		if _, err := s.SignInChallenge(ctx, "https://evil.test", ""); !errors.Is(err, ErrKeyOrigin) {
			t.Fatalf("a challenge from an origin not allowed = %v, want ErrKeyOrigin", err)
		}
		// Asked from one allowed origin, answered from another: the challenge is bound to the first.
		c, _ := s.SignInChallenge(ctx, testOrigin, "")
		if _, err := s.SignInWithKey(ctx, a.answer(t, c.Challenge, otherAllowedOrigin, true), ""); !errors.Is(err, ErrKeyOrigin) {
			t.Fatalf("answered from another origin = %v, want ErrKeyOrigin", err)
		}
	})
	t.Run("another person's key", func(t *testing.T) {
		if _, err := signIn(t, s, b, "per_a"); !errors.Is(err, ErrNoKey) {
			t.Fatalf("per_b's key for per_a = %v, want ErrNoKey", err)
		}
	})
	t.Run("an answer without user verification", func(t *testing.T) {
		c, _ := s.SignInChallenge(ctx, testOrigin, "")
		if _, err := s.SignInWithKey(ctx, a.answer(t, c.Challenge, testOrigin, false), ""); !errors.Is(err, ErrKeyRefused) {
			t.Fatalf("no user verification = %v, want ErrKeyRefused", err)
		}
	})
	t.Run("a counter that went backwards", func(t *testing.T) {
		var stored uint32
		if err := pool.QueryRow(ctx, `SELECT sign_count FROM person_keys WHERE person_id = 'per_a'`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		saved := a.count
		a.count = stored - 1 // the answer carries the stored count again
		c, _ := s.SignInChallenge(ctx, testOrigin, "")
		if _, err := s.SignInWithKey(ctx, a.answer(t, c.Challenge, testOrigin, true), ""); !errors.Is(err, ErrKeyRefused) {
			t.Fatalf("a cloned key's counter = %v, want ErrKeyRefused", err)
		}
		a.count = saved
	})
	t.Run("a removed key", func(t *testing.T) {
		// A second key first, so the first is not the last way in.
		in, err := signIn(t, s, a, "per_a")
		if err != nil {
			t.Fatal(err)
		}
		rc, _ := s.RegisterChallenge(ctx, "per_a", "")
		second := newAuthenticator(t)
		if _, err := s.AddKey(ctx, "per_a", NewKey{Passkey: second.register(t, rc.Challenge)}, KeyProof{Ticket: in.Ticket}, ""); err != nil {
			t.Fatal(err)
		}
		if err := s.RemoveKey(ctx, "per_a", in.KeyID, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := signIn(t, s, a, ""); !errors.Is(err, ErrNoKey) {
			t.Fatalf("a removed key = %v, want ErrNoKey", err)
		}
		// And it is never kept again.
		rc, _ = s.RegisterChallenge(ctx, "per_a", "")
		if _, err := s.AddKey(ctx, "per_a", NewKey{Passkey: a.register(t, rc.Challenge)}, KeyProof{}, ""); !errors.Is(err, ErrKeyKept) {
			t.Fatalf("a removed key registered again = %v, want ErrKeyKept", err)
		}
	})
	var refused int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM phone_signin_events WHERE event = 'key_refused'`).Scan(&refused); err != nil || refused < 6 {
		t.Fatalf("refusals audited = %d (%v), want each one", refused, err)
	}
}

func TestB6124LastWayInStays(t *testing.T) {
	s, pool := keyStore(t)
	ctx := context.Background()
	newPerson(t, pool, "per_a", "+447700900021")
	a := signUpKey(t, s, "per_a")
	keys, err := s.ListKeys(ctx, "per_a")
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys = %v, %v", keys, err)
	}
	if err := s.RemoveKey(ctx, "per_a", keys[0].ID, ""); !errors.Is(err, ErrLastWayIn) {
		t.Fatalf("removing the last key with no passcode = %v, want ErrLastWayIn", err)
	}
	if _, err := signIn(t, s, a, ""); err != nil {
		t.Fatalf("the last key still signs in: %v", err)
	}
	// With a passcode set, the passcode is a way in, so the last key may go.
	s.Passcodes = fakePasscodes{has: true, right: "284719"}
	if err := s.RemoveKey(ctx, "per_a", keys[0].ID, ""); err != nil {
		t.Fatalf("removing the last key with a passcode set: %v", err)
	}
	if keys, _ := s.ListKeys(ctx, "per_a"); len(keys) != 0 {
		t.Fatalf("a removed key is still listed: %v", keys)
	}
}

func TestB6124NewDeviceKeyNeedsPasscodeOrKey(t *testing.T) {
	s, pool := keyStore(t)
	ctx := context.Background()
	newPerson(t, pool, "per_a", "+447700900031")
	newPerson(t, pool, "per_b", "+447700900032")
	a := signUpKey(t, s, "per_a")
	b := signUpKey(t, s, "per_b")
	add := func(proof KeyProof) error {
		rc, err := s.RegisterChallenge(ctx, "per_a", "")
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.AddKey(ctx, "per_a", NewKey{Name: "New laptop", Passkey: newAuthenticator(t).register(t, rc.Challenge)}, proof, "")
		return err
	}
	if err := add(KeyProof{}); !errors.Is(err, ErrKeyProof) {
		t.Fatalf("a new key with a session alone = %v, want ErrKeyProof", err)
	}
	bIn, err := signIn(t, s, b, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := add(KeyProof{Ticket: bIn.Ticket}); !errors.Is(err, ErrKeyProof) {
		t.Fatalf("a new key with another person's ticket = %v, want ErrKeyProof", err)
	}
	if err := add(KeyProof{Passcode: "284719"}); !errors.Is(err, ErrKeyProof) {
		t.Fatalf("a passcode for a person with none = %v, want ErrKeyProof", err)
	}
	aIn, err := signIn(t, s, a, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := add(KeyProof{Ticket: aIn.Ticket}); err != nil {
		t.Fatalf("a new key with an existing key's sign-in: %v", err)
	}
	if err := add(KeyProof{Ticket: aIn.Ticket}); !errors.Is(err, ErrKeyProof) {
		t.Fatalf("the same ticket twice = %v, want ErrKeyProof", err)
	}
	s.Passcodes = fakePasscodes{has: true, right: "284719"}
	if err := add(KeyProof{Passcode: "000001"}); err == nil {
		t.Fatal("a new key with a wrong passcode was kept")
	}
	if err := add(KeyProof{Passcode: "284719"}); err != nil {
		t.Fatalf("a new key with the passcode: %v", err)
	}
	// A challenge is the person's own: per_b's registration challenge does not add a key for per_a.
	rc, _ := s.RegisterChallenge(ctx, "per_b", "")
	if _, err := s.AddKey(ctx, "per_a", NewKey{Passkey: newAuthenticator(t).register(t, rc.Challenge)}, KeyProof{Passcode: "284719"}, ""); !errors.Is(err, ErrKeyChallenge) {
		t.Fatalf("another person's challenge = %v, want ErrKeyChallenge", err)
	}
	if keys, _ := s.ListKeys(ctx, "per_a"); len(keys) != 3 {
		t.Fatalf("per_a has %d keys, want 3", len(keys))
	}
}
