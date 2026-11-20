package phonesignin

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/passkey"
)

// B61.107 — a forgotten passcode, set again with the number's text and a key, on a migrated schema.
func TestB61107ForgottenPasscodeNeedsTheNumberAndAKeyOfOnePerson(t *testing.T) {
	ctx := context.Background()
	pool := phoneDB(t)
	d := newPasscodeDoor(t, pool)
	d.RP = &passkey.RelyingParty{ID: testRPID, Origins: []string{testOrigin}}
	d.Devices = &devicekey.Verifier{AllowTest: true}
	const fresh = "314159"
	join := func(phone string) (Person, *authenticator) {
		t.Helper()
		p := joinWith(t, d, phone, testPasscode)
		rc, err := d.RegisterChallenge(ctx, p.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		a := newAuthenticator(t)
		if _, err := d.AddKey(ctx, p.ID, NewKey{Name: "Phone", Passkey: a.register(t, rc.Challenge)}, KeyProof{Passcode: testPasscode}, ""); err != nil {
			t.Fatal(err)
		}
		return p, a
	}
	keyTicket := func(p Person, a *authenticator) string {
		t.Helper()
		in, err := signIn(t, d.Store, a, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		return in.Ticket
	}
	ann, annKey := join("+447700910701")
	bob, bobKey := join("+447700910702")
	// Ten wrong tries turned Ann's passcode off: the forgot path is how it comes back on.
	if _, err := pool.Exec(ctx, `UPDATE person_passcodes SET off_at = now() WHERE person_id = $1`, ann.ID); err != nil {
		t.Fatal(err)
	}

	phoneT, keyT := ticketFor(t, d, ann.Phone), keyTicket(ann, annKey)
	if _, err := d.ReplaceForgottenPasscode(ctx, phoneT, "", fresh, "203.0.113.70"); !errors.Is(err, ErrKeyTicket) {
		t.Fatalf("the number's ticket alone = %v, want ErrKeyTicket", err)
	}
	if _, err := d.ReplaceForgottenPasscode(ctx, "", keyT, fresh, "203.0.113.70"); !errors.Is(err, ErrTicket) {
		t.Fatalf("a key's ticket alone = %v, want ErrTicket", err)
	}
	if _, err := d.ReplaceForgottenPasscode(ctx, phoneT, keyTicket(bob, bobKey), fresh, "203.0.113.70"); !errors.Is(err, ErrTwoPeople) {
		t.Fatalf("Ann's number with Bob's key = %v, want ErrTwoPeople", err)
	}
	var weak *WeakPasscodeError
	if _, err := d.ReplaceForgottenPasscode(ctx, phoneT, keyT, "123456", "203.0.113.70"); !errors.As(err, &weak) {
		t.Fatalf("a weak new passcode = %v, want WeakPasscodeError", err)
	}

	// Both of Ann's, unspent by the refusals above, set the new passcode once.
	p, err := d.ReplaceForgottenPasscode(ctx, phoneT, keyT, fresh, "203.0.113.70")
	if err != nil || p.ID != ann.ID || !p.HasPasscode {
		t.Fatalf("both of Ann's tickets: %+v %v", p, err)
	}
	if err := d.CheckPasscode(ctx, ann.ID, testPasscode, ""); !errors.Is(err, ErrWrongPasscode) {
		t.Fatalf("the old passcode after the new one = %v, want ErrWrongPasscode", err)
	}
	if err := d.CheckPasscode(ctx, ann.ID, fresh, ""); err != nil {
		t.Fatalf("the new passcode: %v", err)
	}
	if _, _, err := d.RedeemWithPasscode(ctx, ticketFor(t, d, ann.Phone), "", testPasscode, ""); !errors.Is(err, ErrWrongPasscode) {
		t.Fatalf("signing in with the old passcode = %v, want ErrWrongPasscode", err)
	}
	if err := d.CheckPasscode(ctx, bob.ID, testPasscode, ""); err != nil {
		t.Fatalf("Bob's passcode moved: %v", err)
	}

	// Each ticket works once.
	if _, err := d.ReplaceForgottenPasscode(ctx, phoneT, keyTicket(ann, annKey), "271828", ""); !errors.Is(err, ErrTicket) {
		t.Fatalf("the number's ticket again = %v, want ErrTicket", err)
	}
	if _, err := d.ReplaceForgottenPasscode(ctx, ticketFor(t, d, ann.Phone), keyT, "271828", ""); !errors.Is(err, ErrKeyTicket) {
		t.Fatalf("the key's ticket again = %v, want ErrKeyTicket", err)
	}
	var sets int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM passcode_events WHERE person_id = $1 AND event = 'set_after_key'`, ann.ID).Scan(&sets); err != nil || sets != 1 {
		t.Fatalf("set_after_key rows = %d %v, want 1", sets, err)
	}
}
