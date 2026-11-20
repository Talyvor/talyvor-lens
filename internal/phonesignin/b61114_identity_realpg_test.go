package phonesignin

import (
	"context"
	"errors"
	"testing"
)

// B61.114 — the BFF's Google identity is issuer + a zero byte + sub, and PostgreSQL's text refuses a zero byte
// (22021). The person is made with it, answered with it byte for byte, and found by it again.

const googleIdentity = "https://accounts.google.com\x00108234567890123456789"

func TestIdentityWithAZeroByte(t *testing.T) {
	ctx := context.Background()
	d := newPasscodeDoor(t, phoneDB(t))
	const phone, other = "+447700901141", "+447700901142"

	p, created, err := d.RedeemWithPasscode(ctx, ticketFor(t, d, phone), googleIdentity, testPasscode, "203.0.113.50")
	if err != nil || !created {
		t.Fatalf("a Google identity on a new number: created=%v err=%v", created, err)
	}
	if p.Identity != googleIdentity {
		t.Fatalf("identity answered %q, want %q", p.Identity, googleIdentity)
	}
	if got, err := d.PersonIdentity(ctx, p.ID); err != nil || got != googleIdentity {
		t.Fatalf("PersonIdentity = %q, %v; want the given identity", got, err)
	}

	again, created, err := d.RedeemWithPasscode(ctx, ticketFor(t, d, phone), googleIdentity, testPasscode, "203.0.113.50")
	if err != nil || created || again.ID != p.ID || again.Identity != googleIdentity {
		t.Fatalf("signing in again: %+v created=%v err=%v; want %s", again, created, err, p.ID)
	}

	if _, _, err := d.RedeemWithPasscode(ctx, ticketFor(t, d, other), googleIdentity, testPasscode, "203.0.113.50"); !errors.Is(err, ErrHeld) {
		t.Fatalf("the same identity on another number: %v, want ErrHeld", err)
	}
}

// An identity that holds the stored form's own escapes as text comes back as it was given.
func TestIdentityHoldingEscapesAsText(t *testing.T) {
	ctx := context.Background()
	d := newPasscodeDoor(t, phoneDB(t))
	const given = "issuer%00sub%25and%"
	p, _, err := d.RedeemWithPasscode(ctx, ticketFor(t, d, "+447700901143"), given, testPasscode, "203.0.113.50")
	if err != nil || p.Identity != given {
		t.Fatalf("identity %q, %v; want %q", p.Identity, err, given)
	}
	if got, err := d.PersonIdentity(ctx, p.ID); err != nil || got != given {
		t.Fatalf("PersonIdentity = %q, %v; want %q", got, err, given)
	}
}
