package phonesignin

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// B61.23 — the passcode on a migrated schema: set, checked, counted on every replica, waited, turned off.

const testPasscode = "583920"

func newPasscodeDoor(t *testing.T, pool *pgxpool.Pool) *door {
	d := newDoor(t, pool)
	d.PasscodeSecret = []byte("passcode-secret-for-tests-0123456789")
	d.PerAddressHour = 1000
	return d
}

// ticketFor confirms phone and answers the ticket.
func ticketFor(t *testing.T, d *door, phone string) string {
	t.Helper()
	ctx := context.Background()
	d.later(TextGap + time.Second)
	st, err := d.Start(ctx, phone, "203.0.113.50")
	if err != nil {
		t.Fatal(err)
	}
	fin, err := d.Finish(ctx, phone, st.SandboxCode, "203.0.113.50")
	if err != nil {
		t.Fatal(err)
	}
	return fin.Ticket
}

// joinWith makes the person for phone at sign-up, setting passcode with the fresh ticket.
func joinWith(t *testing.T, d *door, phone, passcode string) Person {
	t.Helper()
	p, created, err := d.RedeemWithPasscode(context.Background(), ticketFor(t, d, phone), "", passcode, "203.0.113.50")
	if err != nil || !created || !p.HasPasscode {
		t.Fatalf("sign-up with a passcode: %+v created=%v err=%v", p, created, err)
	}
	return p
}

func wantWait(t *testing.T, err error, at time.Time) {
	t.Helper()
	var wait *PasscodeWaitError
	if !errors.As(err, &wait) || !wait.RetryAt.Equal(at) {
		t.Fatalf("got %v, want a wait until %s", err, at)
	}
}

func TestPasscodeSetAndCheck(t *testing.T) {
	ctx := context.Background()
	d := newPasscodeDoor(t, phoneDB(t))
	const phone = "+447700900401"
	p := joinWith(t, d, phone, testPasscode)

	// The phone app's lock: right and wrong, for a session of the person.
	if err := d.CheckPasscode(ctx, p.ID, testPasscode, "203.0.113.50"); err != nil {
		t.Fatalf("the right passcode: %v", err)
	}
	if err := d.CheckPasscode(ctx, p.ID, "583921", "203.0.113.50"); !errors.Is(err, ErrWrongPasscode) {
		t.Fatalf("a wrong passcode: %v, want ErrWrongPasscode", err)
	}

	// A ticket alone signs in nobody who has a passcode, and the refusal leaves the ticket for the passcode to follow.
	ticket := ticketFor(t, d, phone)
	if _, _, err := d.Redeem(ctx, ticket, "", "203.0.113.50"); !errors.Is(err, ErrPasscodeNeeded) {
		t.Fatalf("a ticket alone: %v, want ErrPasscodeNeeded", err)
	}
	if _, _, err := d.RedeemWithPasscode(ctx, ticket, "", "000000", "203.0.113.50"); !errors.Is(err, ErrWrongPasscode) {
		t.Fatalf("a ticket with a wrong passcode: %v, want ErrWrongPasscode", err)
	}
	again, created, err := d.RedeemWithPasscode(ctx, ticket, "", testPasscode, "203.0.113.50")
	if err != nil || created || again.ID != p.ID || !again.HasPasscode {
		t.Fatalf("a ticket with the right passcode signs the person in: %+v created=%v err=%v", again, created, err)
	}
	if _, _, err := d.RedeemWithPasscode(ctx, ticket, "", testPasscode, "203.0.113.50"); !errors.Is(err, ErrTicket) {
		t.Fatalf("the ticket twice: %v, want ErrTicket", err)
	}

	// A person who joined before passcodes sets one with a fresh ticket, and from then on needs it.
	const older = "+447700900402"
	if _, _, err := d.Redeem(ctx, ticketFor(t, d, older), "", "203.0.113.50"); err != nil {
		t.Fatal(err)
	}
	q, _, err := d.RedeemWithPasscode(ctx, ticketFor(t, d, older), "", "271828", "203.0.113.50")
	if err != nil || !q.HasPasscode {
		t.Fatalf("setting a passcode with a fresh ticket: %+v %v", q, err)
	}
	if _, _, err := d.Redeem(ctx, ticketFor(t, d, older), "", "203.0.113.50"); !errors.Is(err, ErrPasscodeNeeded) {
		t.Fatalf("after setting one, a ticket alone: %v, want ErrPasscodeNeeded", err)
	}

	// Changing it needs the old one.
	if err := d.ChangePasscode(ctx, p.ID, "000000", "314159", "203.0.113.50"); !errors.Is(err, ErrWrongPasscode) {
		t.Fatalf("a change with a wrong old passcode: %v, want ErrWrongPasscode", err)
	}
	if err := d.ChangePasscode(ctx, p.ID, testPasscode, "314159", "203.0.113.50"); err != nil {
		t.Fatal(err)
	}
	if err := d.CheckPasscode(ctx, p.ID, testPasscode, "203.0.113.50"); !errors.Is(err, ErrWrongPasscode) {
		t.Fatalf("the old passcode after a change: %v, want ErrWrongPasscode", err)
	}
	if err := d.CheckPasscode(ctx, p.ID, "314159", "203.0.113.50"); err != nil {
		t.Fatalf("the new passcode: %v", err)
	}
}

func TestPasscodeWaitsAfterFiveWrong(t *testing.T) {
	ctx := context.Background()
	d := newPasscodeDoor(t, phoneDB(t))
	p := joinWith(t, d, "+447700900411", testPasscode)
	for i := 1; i <= 4; i++ {
		if err := d.CheckPasscode(ctx, p.ID, "000000", ""); !errors.Is(err, ErrWrongPasscode) {
			t.Fatalf("wrong try %d: %v", i, err)
		}
	}
	fifth := d.clock
	wantWait(t, d.CheckPasscode(ctx, p.ID, "000000", ""), fifth.Add(PasscodeWait))

	// Inside the window the sixth wrong try is refused with the wait, and the right one too.
	d.later(time.Minute)
	wantWait(t, d.CheckPasscode(ctx, p.ID, "000000", ""), fifth.Add(PasscodeWait))
	wantWait(t, d.CheckPasscode(ctx, p.ID, testPasscode, ""), fifth.Add(PasscodeWait))
	var counted int
	if err := d.DB.QueryRow(ctx, `SELECT count(*) FROM passcode_events WHERE person_id = $1 AND event = 'wrong'`, p.ID).Scan(&counted); err != nil || counted != 5 {
		t.Fatalf("tries refused inside the wait are not tried: %d wrong rows, want 5 (%v)", counted, err)
	}

	// After the wait the right one is let in and clears the count: four wrong tries are only wrong again.
	d.clock = fifth.Add(PasscodeWait)
	if err := d.CheckPasscode(ctx, p.ID, testPasscode, ""); err != nil {
		t.Fatalf("the right passcode after the wait: %v", err)
	}
	for i := 1; i <= 4; i++ {
		if err := d.CheckPasscode(ctx, p.ID, "000000", ""); !errors.Is(err, ErrWrongPasscode) {
			t.Fatalf("after a right try, wrong try %d: %v", i, err)
		}
	}
}

func TestPasscodeOffAfterTenInADay(t *testing.T) {
	ctx := context.Background()
	d := newPasscodeDoor(t, phoneDB(t))
	const phone = "+447700900421"
	p := joinWith(t, d, phone, testPasscode)
	for i := 1; i <= PasscodeOffAfter; i++ {
		err := d.CheckPasscode(ctx, p.ID, "000000", "")
		switch {
		case i < PasscodeWaitAfter && !errors.Is(err, ErrWrongPasscode),
			i >= PasscodeWaitAfter && i < PasscodeOffAfter && !errors.As(err, new(*PasscodeWaitError)),
			i == PasscodeOffAfter && !errors.Is(err, ErrPasscodeOff):
			t.Fatalf("wrong try %d: %v", i, err)
		}
		if i >= PasscodeWaitAfter {
			d.later(PasscodeWait)
		}
	}

	// The eleventh, right or not, by the lock or with a fresh ticket, is refused: it is off.
	if err := d.CheckPasscode(ctx, p.ID, testPasscode, ""); !errors.Is(err, ErrPasscodeOff) {
		t.Fatalf("the right passcode once off: %v, want ErrPasscodeOff", err)
	}
	if _, _, err := d.RedeemWithPasscode(ctx, ticketFor(t, d, phone), "", testPasscode, ""); !errors.Is(err, ErrPasscodeOff) {
		t.Fatalf("a ticket with the right passcode once off: %v, want ErrPasscodeOff", err)
	}
	if err := d.ChangePasscode(ctx, p.ID, testPasscode, "314159", ""); !errors.Is(err, ErrPasscodeOff) {
		t.Fatalf("a change with the old passcode once off: %v, want ErrPasscodeOff", err)
	}
	d.later(48 * time.Hour)
	if err := d.CheckPasscode(ctx, p.ID, testPasscode, ""); !errors.Is(err, ErrPasscodeOff) {
		t.Fatalf("a day later it is still off: %v", err)
	}

	// A key sign-in (B61.24's passkey, standing in here) sets a new one, and it is on again.
	if err := d.SetPasscodeAfterKey(ctx, p.ID, "314159", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.CheckPasscode(ctx, p.ID, "314159", ""); err != nil {
		t.Fatalf("the new passcode after a key sign-in: %v", err)
	}
}

func TestPasscodeWeakOnesRefused(t *testing.T) {
	ctx := context.Background()
	d := newPasscodeDoor(t, phoneDB(t))
	const phone = "+447700958204"
	ticket := ticketFor(t, d, phone)
	for _, weak := range []string{"777777", "123456", "654321", "345678", "958204"} {
		var w *WeakPasscodeError
		if _, _, err := d.RedeemWithPasscode(ctx, ticket, "", weak, ""); !errors.As(err, &w) {
			t.Fatalf("%s at sign-up: %v, want refused as weak", weak, err)
		}
	}
	p, created, err := d.RedeemWithPasscode(ctx, ticket, "", testPasscode, "")
	if err != nil || !created {
		t.Fatalf("the weak ones leave the ticket for a good one: %+v %v", p, err)
	}
	if err := d.ChangePasscode(ctx, p.ID, testPasscode, "000000", ""); !errors.As(err, new(*WeakPasscodeError)) {
		t.Fatalf("a weak change: %v", err)
	}
	if err := d.SetPasscodeAfterKey(ctx, p.ID, "987654", ""); !errors.As(err, new(*WeakPasscodeError)) {
		t.Fatalf("a weak set after a key: %v", err)
	}
}

func TestPasscodeHashNeedsTheSecret(t *testing.T) {
	ctx := context.Background()
	pool := phoneDB(t)
	d := newPasscodeDoor(t, pool)
	p := joinWith(t, d, "+447700900431", testPasscode)
	var salt, hash []byte
	if err := pool.QueryRow(ctx, `SELECT salt, hash FROM person_passcodes WHERE person_id = $1`, p.ID).Scan(&salt, &hash); err != nil {
		t.Fatal(err)
	}
	other := &Store{PasscodeSecret: []byte("another-secret-of-thirty-two-chars!!")}
	if !bytes.Equal(d.hashPasscode(salt, p.ID, testPasscode), hash) || bytes.Equal(other.hashPasscode(salt, p.ID, testPasscode), hash) {
		t.Fatal("the passcode's hash must verify under the store's secret and only under it")
	}

	// Unset, setting and checking are refused, and a person who has one is not signed in.
	none := newDoor(t, pool)
	none.clock = d.clock
	if err := none.CheckPasscode(ctx, p.ID, testPasscode, ""); !errors.Is(err, ErrPasscodeUnavailable) {
		t.Fatalf("checking with no secret: %v", err)
	}
	if _, _, err := none.RedeemWithPasscode(ctx, ticketFor(t, none, "+447700900432"), "", testPasscode, ""); !errors.Is(err, ErrPasscodeUnavailable) {
		t.Fatalf("setting with no secret: %v", err)
	}
	if _, _, err := none.RedeemWithPasscode(ctx, ticketFor(t, none, "+447700900431"), "", testPasscode, ""); !errors.Is(err, ErrPasscodeUnavailable) {
		t.Fatalf("signing in with no secret: %v", err)
	}
}

func TestPasscodeNeverInARowOrALog(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx := context.Background()
	d := newPasscodeDoor(t, phoneDB(t))
	const phone = "+447700900441"
	p := joinWith(t, d, phone, testPasscode)
	_ = d.CheckPasscode(ctx, p.ID, testPasscode, "")
	_ = d.CheckPasscode(ctx, p.ID, "000000", "")
	_, _, _ = d.RedeemWithPasscode(ctx, ticketFor(t, d, phone), "", testPasscode, "")
	for _, table := range []string{"person_passcodes", "passcode_events", "phone_signin_events", "people"} {
		var dump string
		if err := d.DB.QueryRow(ctx, `SELECT COALESCE(string_agg(row_to_json(t)::text, ' '), '') FROM `+table+` t`).Scan(&dump); err != nil {
			t.Fatal(err)
		}
		if dump == "" {
			t.Fatalf("%s is empty — the scan below would pass over nothing", table)
		}
		if strings.Contains(dump, testPasscode) {
			t.Fatalf("%s holds the passcode in plain: %s", table, dump)
		}
	}
	if strings.Contains(logs.String(), testPasscode) {
		t.Fatalf("a log holds the passcode: %s", logs.String())
	}
}

func TestPasscodeTwoReplicasShareTheCount(t *testing.T) {
	ctx := context.Background()
	poolA := phoneDB(t)
	cfg := poolA.Config().Copy()
	poolB, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(poolB.Close)
	a := newPasscodeDoor(t, poolA)
	p := joinWith(t, a, "+447700900451", testPasscode)
	b := newPasscodeDoor(t, poolB)
	b.clock = a.clock

	// Twelve wrong tries at once, half on each replica: five are tried, the rest wait.
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		replica := a
		if i%2 == 1 {
			replica = b
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = replica.CheckPasscode(ctx, p.ID, "000000", "")
		}()
	}
	wg.Wait()
	var wrong, waited int
	if err := poolA.QueryRow(ctx, `SELECT count(*) FILTER (WHERE event = 'wrong'), count(*) FILTER (WHERE event = 'waited')
		FROM passcode_events WHERE person_id = $1`, p.ID).Scan(&wrong, &waited); err != nil {
		t.Fatal(err)
	}
	if wrong != PasscodeWaitAfter || waited != 12-PasscodeWaitAfter {
		t.Fatalf("two replicas: %d wrong and %d waited, want %d and %d", wrong, waited, PasscodeWaitAfter, 12-PasscodeWaitAfter)
	}
	wantWait(t, a.CheckPasscode(ctx, p.ID, testPasscode, ""), a.clock.Add(PasscodeWait))
}
