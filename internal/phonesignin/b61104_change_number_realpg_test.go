package phonesignin

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// B61.104 — a person with a session changes their own number: a texted code to the new number (the door's own start
// and finish) and their passcode. They keep their keys and accounts, sign in with the new number and not the old, and
// the move is there for the BFF to tell. Refused: a number another person holds, their own number, a wrong passcode
// (and its wait), no passcode set; each refusal leaves the number and the ticket as they were.
func TestPersonChangesTheirOwnNumber(t *testing.T) {
	ctx := context.Background()
	pool := phoneDB(t)
	d := newPasscodeDoor(t, pool)
	const old, fresh, other, bare, waiter = "+447700900601", "+447700900602", "+447700900603", "+447700900604", "+447700900605"
	p := joinWith(t, d, old, testPasscode)
	joinWith(t, d, other, "740193")
	if _, err := pool.Exec(ctx, `INSERT INTO person_keys (id, person_id, kind, platform, name, public_key, sign_count, created_at)
		VALUES ('pky_b61104', $1, 'device', 'ios', 'My phone', '\x01', 0, now())`, p.ID); err != nil {
		t.Fatal(err)
	}
	holder := func(phone string) string {
		t.Helper()
		got, ok, err := d.PersonByPhone(ctx, phone)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return ""
		}
		return got.ID
	}

	// Onto another person's number: refused in the door's words, once the code is right; nothing moved.
	if _, err := d.ChangeNumber(ctx, p.ID, ticketFor(t, d, other), testPasscode, "203.0.113.50"); !errors.Is(err, ErrHeld) {
		t.Fatalf("a change onto another person's number: %v, want ErrHeld", err)
	}
	// Onto the number they have.
	if _, err := d.ChangeNumber(ctx, p.ID, ticketFor(t, d, old), testPasscode, "203.0.113.50"); !errors.Is(err, ErrSameNumber) {
		t.Fatalf("a change onto their own number: %v, want ErrSameNumber", err)
	}
	// A wrong passcode: refused, and the ticket is still good.
	ticket := ticketFor(t, d, fresh)
	if _, err := d.ChangeNumber(ctx, p.ID, ticket, "290471", "203.0.113.50"); !errors.Is(err, ErrWrongPasscode) {
		t.Fatalf("a wrong passcode: %v, want ErrWrongPasscode", err)
	}
	if holder(old) != p.ID || holder(fresh) != "" {
		t.Fatalf("after the refusals the old number is held by %q and the new by %q", holder(old), holder(fresh))
	}

	// No passcode set: refused.
	noPasscode, _, err := d.Redeem(ctx, ticketFor(t, d, bare), "", "203.0.113.50")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ChangeNumber(ctx, noPasscode.ID, ticketFor(t, d, "+447700900606"), "", "203.0.113.50"); !errors.Is(err, ErrNoPasscode) {
		t.Fatalf("a person with no passcode: %v, want ErrNoPasscode", err)
	}
	// The passcode's own wait: five wrong tries, and the sixth waits whatever it is.
	w := joinWith(t, d, waiter, testPasscode)
	elsewhere := func(i int) string { return fmt.Sprintf("+4477009007%02d", i) }
	for i := 1; i < PasscodeWaitAfter; i++ {
		if _, err := d.ChangeNumber(ctx, w.ID, ticketFor(t, d, elsewhere(i)), "290471", "203.0.113.50"); !errors.Is(err, ErrWrongPasscode) {
			t.Fatalf("wrong try %d: %v", i, err)
		}
	}
	if _, err := d.ChangeNumber(ctx, w.ID, ticketFor(t, d, elsewhere(PasscodeWaitAfter)), "290471", "203.0.113.50"); err == nil {
		t.Fatal("the fifth wrong try changed the number")
	}
	fifth := d.clock
	last := ticketFor(t, d, elsewhere(PasscodeWaitAfter+1))
	_, err = d.ChangeNumber(ctx, w.ID, last, testPasscode, "203.0.113.50")
	wantWait(t, err, fifth.Add(PasscodeWait))
	if holder(waiter) != w.ID {
		t.Fatalf("after the wait the waiter's number is held by %q", holder(waiter))
	}

	// The new number's code and the passcode: the number is the new one, the keys are kept.
	changed, err := d.ChangeNumber(ctx, p.ID, ticket, testPasscode, "203.0.113.50")
	if err != nil {
		t.Fatal(err)
	}
	if changed.Person.ID != p.ID || changed.Person.Phone != fresh || changed.Person.Identity != p.Identity || changed.MoveID == "" {
		t.Fatalf("changed = %+v; want the same person on the new number", changed)
	}
	if holder(fresh) != p.ID || holder(old) != "" {
		t.Fatalf("after the change the new number is held by %q and the old by %q", holder(fresh), holder(old))
	}
	if _, err := d.ChangeNumber(ctx, p.ID, ticket, testPasscode, "203.0.113.50"); !errors.Is(err, ErrTicket) {
		t.Fatalf("the ticket used again: %v, want ErrTicket", err)
	}
	var (
		live           int
		by, oldPhone   string
		events, holds  int
		operatorOnMove string
		movedHeldUntil time.Time
		movedAt        time.Time
	)
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM person_keys WHERE person_id = m.person_id AND removed_at IS NULL),
		m.changed_by, m.old_phone, m.operator, m.moved_at, m.held_until,
		(SELECT count(*) FROM number_move_holds WHERE move_id = m.id),
		(SELECT count(*) FROM phone_signin_events WHERE event = 'number_moved' AND phone IN ($2, $3) AND detail LIKE 'by the person%')
		FROM person_number_moves m WHERE m.id = $1`, changed.MoveID, old, fresh).
		Scan(&live, &by, &oldPhone, &operatorOnMove, &movedAt, &movedHeldUntil, &holds, &events); err != nil {
		t.Fatal(err)
	}
	if live != 1 || by != "person" || oldPhone != old || operatorOnMove != "" || !movedHeldUntil.Equal(movedAt) || holds != 0 || events != 2 {
		t.Fatalf("the move: live keys %d by %q old %q operator %q held until %v at %v holds %d events %d", live, by, oldPhone,
			operatorOnMove, movedHeldUntil, movedAt, holds, events)
	}

	// Signs in with the new number and the passcode; the old number finds nobody.
	back, created, err := d.RedeemWithPasscode(ctx, ticketFor(t, d, fresh), "", testPasscode, "203.0.113.50")
	if err != nil || created || back.ID != p.ID {
		t.Fatalf("sign-in by the new number: %+v created=%v err=%v; want the same person", back, created, err)
	}
	if again, created, err := d.RedeemWithPasscode(ctx, ticketFor(t, d, old), "", "381946", "203.0.113.50"); err != nil || !created || again.ID == p.ID {
		t.Fatalf("sign-in by the old number: %+v created=%v err=%v; want a new person, never the one who changed", again, created, err)
	}

	// The BFF takes the change once, with both numbers and who changed it, to tell the person's email addresses.
	notices, err := d.ClaimNumberMoveNotices(ctx, 10)
	if err != nil || len(notices) != 1 || notices[0].ID != changed.MoveID || notices[0].OldPhone != old || notices[0].NewPhone != fresh ||
		notices[0].By != "person" {
		t.Fatalf("notices = %+v (%v); want the one change", notices, err)
	}
}
