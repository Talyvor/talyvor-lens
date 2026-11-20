package phonesignin

import (
	"context"
	"errors"
	"testing"
	"time"
)

// B61.30 — a number moved by an operator takes every way in the person had, and the person comes back in by a text to
// the new number, a new passcode and a new key, as the same person with the same accounts.
func TestMoveNumberTakesEveryWayInAndTheNewNumberComesBackIn(t *testing.T) {
	ctx := context.Background()
	pool := phoneDB(t)
	d := newPasscodeDoor(t, pool)
	const lost, fresh, other = "+447700900501", "+447700900502", "+447700900503"
	p := joinWith(t, d, lost, testPasscode)
	someone := joinWith(t, d, other, "740193")
	if _, err := pool.Exec(ctx, `INSERT INTO person_keys (id, person_id, kind, platform, name, public_key, sign_count, created_at)
		VALUES ('pky_lost_phone', $1, 'device', 'ios', 'The lost phone', '\x01', 0, now())`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO person_key_tickets (ticket_hash, person_id, key_id, expires_at)
		VALUES ('\x02', $1, 'pky_lost_phone', now() + interval '5 minutes')`, p.ID); err != nil {
		t.Fatal(err)
	}
	stale := ticketFor(t, d, fresh) // a ticket for the new number asked before the move
	accounts := []string{"u-b6130-first", "u-b6130-second"}
	move := NumberMove{PersonID: p.ID, OldPhone: lost, NewPhone: fresh, Operator: "nicolai", IdentityCheckRef: "kyc_b6130", Accounts: accounts}

	// Onto another person's number, or for a person who does not hold the number: refused, nothing moved.
	if _, err := d.MoveNumber(ctx, NumberMove{PersonID: p.ID, OldPhone: lost, NewPhone: other, Operator: "nicolai"}); !errors.Is(err, ErrHeld) {
		t.Fatalf("a move onto another person's number: %v, want ErrHeld", err)
	}
	if _, err := d.MoveNumber(ctx, NumberMove{PersonID: someone.ID, OldPhone: lost, NewPhone: fresh, Operator: "nicolai"}); !errors.Is(err, ErrNotThatPerson) {
		t.Fatalf("a move for a person who does not hold the number: %v, want ErrNotThatPerson", err)
	}
	if got, _, _ := d.PersonByPhone(ctx, lost); got.ID != p.ID {
		t.Fatalf("after the refusals the lost number is held by %q, want %q", got.ID, p.ID)
	}

	moved, err := d.MoveNumber(ctx, move)
	if err != nil {
		t.Fatal(err)
	}
	if moved.KeysRemoved != 1 || moved.HeldAccount != 2 || !moved.HeldUntil.Equal(d.clock.Add(24*time.Hour)) {
		t.Fatalf("moved = %+v; want one key removed, two accounts held for a day", moved)
	}
	var (
		phone, operator string
		sandbox         bool
		passcodes, live int
		keyTicketOpen   bool
		holds           int
	)
	if err := pool.QueryRow(ctx, `SELECT p.phone, p.phone_sandbox, m.operator,
		(SELECT count(*) FROM person_passcodes WHERE person_id = p.id),
		(SELECT count(*) FROM person_keys WHERE person_id = p.id AND removed_at IS NULL),
		EXISTS (SELECT 1 FROM person_key_tickets WHERE person_id = p.id AND used_at IS NULL),
		(SELECT count(*) FROM number_move_holds WHERE move_id = m.id AND held_until = m.held_until)
		FROM people p JOIN person_number_moves m ON m.person_id = p.id WHERE p.id = $1`, p.ID).
		Scan(&phone, &sandbox, &operator, &passcodes, &live, &keyTicketOpen, &holds); err != nil {
		t.Fatal(err)
	}
	if phone != fresh || !sandbox || operator != "nicolai" || passcodes != 0 || live != 0 || keyTicketOpen || holds != 2 {
		t.Fatalf("after the move: phone %s sandbox %v operator %q passcodes %d live keys %d key ticket open %v holds %d", phone, sandbox,
			operator, passcodes, live, keyTicketOpen, holds)
	}
	var events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM phone_signin_events WHERE event = 'number_moved' AND phone IN ($1, $2)
		AND detail LIKE 'by operator nicolai%'`, lost, fresh).Scan(&events); err != nil || events != 2 {
		t.Fatalf("number_moved audit rows = %d (%v), want one for each number", events, err)
	}

	// The ticket asked before the move is spent, and the lost number now signs nobody in as the person.
	if _, _, err := d.RedeemWithPasscode(ctx, stale, "", "290471", "203.0.113.50"); !errors.Is(err, ErrTicket) {
		t.Fatalf("a ticket for the new number from before the move: %v, want ErrTicket", err)
	}
	if got, ok, _ := d.PersonByPhone(ctx, lost); ok {
		t.Fatalf("the lost number still finds %+v", got)
	}

	// A text to the new number and a new passcode: the same person, with a passcode again, and the old one is gone.
	back, created, err := d.RedeemWithPasscode(ctx, ticketFor(t, d, fresh), "", "290471", "203.0.113.50")
	if err != nil || created || back.ID != p.ID || back.Identity != p.Identity || !back.HasPasscode {
		t.Fatalf("back in by the new number: %+v created=%v err=%v; want the same person with a passcode", back, created, err)
	}
	if err := d.CheckPasscode(ctx, p.ID, testPasscode, "203.0.113.50"); !errors.Is(err, ErrWrongPasscode) {
		t.Fatalf("the old passcode after the move: %v, want ErrWrongPasscode", err)
	}
	if err := d.CheckPasscode(ctx, p.ID, "290471", "203.0.113.50"); err != nil {
		t.Fatalf("the new passcode: %v", err)
	}

	// The BFF takes the move once, to tell the person's email addresses.
	notices, err := d.ClaimNumberMoveNotices(ctx, 10)
	if err != nil || len(notices) != 1 || notices[0].ID != moved.ID || notices[0].Identity != p.Identity || notices[0].NewPhone != fresh {
		t.Fatalf("notices = %+v (%v); want the one move, with the person's identity", notices, err)
	}
	if again, err := d.ClaimNumberMoveNotices(ctx, 10); err != nil || len(again) != 0 {
		t.Fatalf("notices claimed again = %+v (%v); want none", again, err)
	}
}
