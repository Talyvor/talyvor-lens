package phonesignin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/workspace"
)

// B52.44 — deleting a person's last account ends their phone sign-in record: the number, the passkey, the device key,
// the passcode and every ticket they had open sign nobody in afterwards, and the number signs up again as a new person.
func TestB5244DeletingTheLastAccountEndsThePersonsSignIn(t *testing.T) {
	ctx := context.Background()
	pool := phoneDB(t)
	d := newPasscodeDoor(t, pool)
	d.RP = &passkey.RelyingParty{ID: testRPID, Origins: []string{testOrigin}}
	d.Devices = &devicekey.Verifier{AllowTest: true}
	const phone = "+447700905244"
	p := joinWith(t, d, phone, testPasscode)

	web := newAuthenticator(t)
	device, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	deviceSPKI, err := x509.MarshalPKIXPublicKey(&device.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO person_keys (id, person_id, kind, credential_id, name, public_key) VALUES
		('pky_b5244_web', $1, 'passkey', $2, 'This Mac', $3), ('pky_b5244_phone', $1, 'device', NULL, 'This phone', $4)`,
		p.ID, b64.EncodeToString(web.id), web.spki(t), deviceSPKI); err != nil {
		t.Fatal(err)
	}
	signInDevice := func() (KeySignIn, error) {
		c, err := d.SignInChallenge(ctx, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return d.SignInWithKey(ctx, passkey.Assertion{DeviceID: "pky_b5244_phone", Challenge: c.Challenge,
			Signature: deviceSign(t, device, c.Challenge)}, "203.0.113.50")
	}
	signInWeb := func() (KeySignIn, error) {
		c, err := d.SignInChallenge(ctx, testOrigin, "")
		if err != nil {
			t.Fatal(err)
		}
		return d.SignInWithKey(ctx, web.answer(t, c.Challenge, testOrigin, true), "203.0.113.50")
	}
	// Before: each way in signs the person in, and each leaves a ticket open.
	if in, err := signInWeb(); err != nil || in.Person.ID != p.ID {
		t.Fatalf("the passkey before the deletion: %+v %v", in, err)
	}
	if in, err := signInDevice(); err != nil || in.Person.ID != p.ID {
		t.Fatalf("the device key before the deletion: %+v %v", in, err)
	}
	if err := d.CheckPasscode(ctx, p.ID, testPasscode, "203.0.113.50"); err != nil {
		t.Fatalf("the passcode before the deletion: %v", err)
	}
	openPhoneTicket := ticketFor(t, d, phone)

	// The deletion as Lens runs it: as lens_app, and token_events as lens_retention (B39.6).
	wsm := workspace.New(asRole(t, pool, dbmigrate.AppRole)).WithUsageEventsPool(asRole(t, pool, dbmigrate.RetentionRole))
	account := workspace.FirstAccountID(p.Identity)
	if err := wsm.RegisterWorkspace(ctx, workspace.Workspace{ID: account, Name: "B52.44"}); err != nil {
		t.Fatal(err)
	}
	if err := wsm.DeleteAccount(ctx, account, p.Identity, nil); err != nil {
		t.Fatalf("delete the account: %v", err)
	}

	if got, ok, err := d.PersonByPhone(ctx, phone); err != nil || ok {
		t.Fatalf("the number still finds %+v (%v)", got, err)
	}
	if _, err := signInWeb(); !errors.Is(err, ErrNoKey) {
		t.Errorf("the passkey after the deletion: %v, want ErrNoKey", err)
	}
	if _, err := signInDevice(); !errors.Is(err, ErrNoKey) {
		t.Errorf("the device key after the deletion: %v, want ErrNoKey", err)
	}
	if err := d.CheckPasscode(ctx, p.ID, testPasscode, "203.0.113.50"); !errors.Is(err, ErrNoPasscode) {
		t.Errorf("the passcode after the deletion: %v, want ErrNoPasscode", err)
	}
	if _, _, err := d.RedeemWithPasscode(ctx, openPhoneTicket, "", testPasscode, "203.0.113.50"); !errors.Is(err, ErrTicket) {
		t.Errorf("a phone ticket from before the deletion: %v, want ErrTicket", err)
	}
	if _, err := d.SignInChallenge(ctx, testOrigin, p.ID); !errors.Is(err, ErrNoPerson) {
		t.Errorf("a sign-in for the deleted person: %v, want ErrNoPerson", err)
	}
	if _, err := d.RegisterChallenge(ctx, p.ID, ""); !errors.Is(err, ErrNoPerson) {
		t.Errorf("a new key for the deleted person: %v, want ErrNoPerson", err)
	}
	if _, err := d.PersonIdentity(ctx, p.ID); !errors.Is(err, ErrNoPerson) {
		t.Errorf("the deleted person's identity: %v, want ErrNoPerson", err)
	}
	var keys, keyTickets, challenges int
	var keptPhone, keptIdentity string
	if err := pool.QueryRow(ctx, `SELECT phone, identity,
		(SELECT count(*) FROM person_keys WHERE person_id = $1), (SELECT count(*) FROM person_key_tickets WHERE person_id = $1),
		(SELECT count(*) FROM person_key_challenges WHERE person_id = $1) FROM people WHERE id = $1 AND deleted_at IS NOT NULL`, p.ID).
		Scan(&keptPhone, &keptIdentity, &keys, &keyTickets, &challenges); err != nil {
		t.Fatalf("the deleted person's row: %v", err)
	}
	if keptPhone == phone || keptIdentity == workspace.StoredIdentity(p.Identity) || keys+keyTickets+challenges != 0 {
		t.Fatalf("the deleted person keeps number %q, identity %q, %d keys, %d key tickets, %d challenges", keptPhone, keptIdentity,
			keys, keyTickets, challenges)
	}

	// The number signs up again, as a new person with a new identity and a passcode of their own.
	again, created, err := d.RedeemWithPasscode(ctx, ticketFor(t, d, phone), "", "290471", "203.0.113.50")
	if err != nil || !created || again.ID == p.ID || again.Identity == p.Identity || !again.HasPasscode {
		t.Fatalf("signing up again with the number: %+v created=%v err=%v; want a new person", again, created, err)
	}
}

// asRole is pool's database as role, with that role's grants applied as a migration applies them.
func asRole(t *testing.T, pool *pgxpool.Pool, role string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	c, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = dbmigrate.ApplyRoles(ctx, c.Conn(), "", "")
	c.Release()
	if err != nil {
		t.Fatal(err)
	}
	cfg := pool.Config()
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+role)
		return err
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}
