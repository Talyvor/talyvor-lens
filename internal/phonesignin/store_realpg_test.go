package phonesignin

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/migrations"
)

// B61.22 — the phone door on a migrated schema: codes, tries, limits, tickets and people.

func phoneDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG phone sign-in test")
	}
	const schema = "phonesignin_realpg"
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatalf("reset schema: %v", err)
		}
	}
	if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = conn.Close(ctx)
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// recordingSender stands in for a sender: sandbox says whether it is the Sandbox one, and it keeps what it sent.
type recordingSender struct {
	mu      sync.Mutex
	sandbox bool
	sent    map[string]string
}

func (r *recordingSender) Name() string {
	if r.sandbox {
		return "sandbox"
	}
	return "real-stand-in"
}
func (r *recordingSender) Sandbox() bool { return r.sandbox }
func (r *recordingSender) SendCode(_ context.Context, phone, code string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sent == nil {
		r.sent = map[string]string{}
	}
	r.sent[phone] = code
	return nil
}
func (r *recordingSender) last(phone string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent[phone]
}

type door struct {
	*Store
	sender partners.TextSender
	clock  time.Time
}

func newDoor(t *testing.T, pool *pgxpool.Pool) *door {
	d := &door{sender: partners.SandboxTextSender{}, clock: time.Now().UTC().Truncate(time.Second)}
	d.Store = &Store{DB: pool, Sender: func() partners.TextSender { return d.sender }, Key: []byte("test-key"),
		PerAddressHour: DefaultTextsPerAddressHour, Now: func() time.Time { return d.clock }}
	return d
}

func (d *door) later(by time.Duration) { d.clock = d.clock.Add(by) }

func wrongCode(right string) string {
	if right == "000000" {
		return "000001"
	}
	return "000000"
}

func TestPhoneDoorRightCodeGivesOneTicketOnce(t *testing.T) {
	ctx := context.Background()
	d := newDoor(t, phoneDB(t))
	const phone = "+447700900101"
	st, err := d.Start(ctx, phone, "203.0.113.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.SandboxCode) != 6 || st.Sender != "sandbox" {
		t.Fatalf("the Sandbox sender hands the 6-digit code back: %+v", st)
	}
	fin, err := d.Finish(ctx, phone, st.SandboxCode, "203.0.113.1")
	if err != nil {
		t.Fatal(err)
	}
	if fin.Ticket == "" || fin.HasPerson || !fin.Sandbox {
		t.Fatalf("a new number's right code: %+v", fin)
	}
	if _, err := d.Finish(ctx, phone, st.SandboxCode, "203.0.113.1"); !errors.Is(err, ErrNoCode) {
		t.Fatalf("the same code twice: %v, want ErrNoCode", err)
	}
	p, created, err := d.Redeem(ctx, fin.Ticket, "", "203.0.113.1")
	if err != nil || !created || p.Phone != phone || !strings.HasPrefix(p.Identity, "person:") || !p.PhoneSandbox {
		t.Fatalf("the ticket makes the person: %+v created=%v err=%v", p, created, err)
	}
	if _, _, err := d.Redeem(ctx, fin.Ticket, "", "203.0.113.1"); !errors.Is(err, ErrTicket) {
		t.Fatalf("the ticket twice: %v, want ErrTicket", err)
	}

	// A known number: the next right code says a person holds it, and the ticket finds the same person.
	d.later(time.Minute)
	st, _ = d.Start(ctx, phone, "203.0.113.1")
	fin, err = d.Finish(ctx, phone, st.SandboxCode, "203.0.113.1")
	if err != nil || !fin.HasPerson {
		t.Fatalf("a known number's right code: %+v %v", fin, err)
	}
	again, created, err := d.Redeem(ctx, fin.Ticket, "", "203.0.113.1")
	if err != nil || created || again.ID != p.ID || again.Identity != p.Identity {
		t.Fatalf("the known number's person: %+v created=%v err=%v, want %+v", again, created, err, p)
	}

	// A ticket older than 5 minutes is refused.
	d.later(time.Minute)
	st, _ = d.Start(ctx, phone, "203.0.113.1")
	fin, _ = d.Finish(ctx, phone, st.SandboxCode, "203.0.113.1")
	d.later(TicketLife + time.Second)
	if _, _, err := d.Redeem(ctx, fin.Ticket, "", "203.0.113.1"); !errors.Is(err, ErrTicket) {
		t.Fatalf("an old ticket: %v, want ErrTicket", err)
	}
}

func TestPhoneDoorFiveWrongCodesEndIt(t *testing.T) {
	ctx := context.Background()
	d := newDoor(t, phoneDB(t))
	const phone = "+447700900102"
	st, _ := d.Start(ctx, phone, "203.0.113.2")
	for i := 1; i <= MaxWrongTries; i++ {
		if _, err := d.Finish(ctx, phone, wrongCode(st.SandboxCode), "203.0.113.2"); !errors.Is(err, ErrWrongCode) {
			t.Fatalf("wrong try %d: %v, want ErrWrongCode", i, err)
		}
	}
	if _, err := d.Finish(ctx, phone, st.SandboxCode, "203.0.113.2"); !errors.Is(err, ErrNoCode) {
		t.Fatalf("the right code after five wrong ones: %v, want ErrNoCode", err)
	}
}

func TestPhoneDoorOldCodeRefused(t *testing.T) {
	ctx := context.Background()
	d := newDoor(t, phoneDB(t))
	const phone = "+447700900103"
	st, _ := d.Start(ctx, phone, "203.0.113.3")
	d.later(CodeLife + time.Second)
	if _, err := d.Finish(ctx, phone, st.SandboxCode, "203.0.113.3"); !errors.Is(err, ErrNoCode) {
		t.Fatalf("a code older than 10 minutes: %v, want ErrNoCode", err)
	}
	// A code a newer one replaced is old too.
	first, _ := d.Start(ctx, phone, "203.0.113.3")
	d.later(TextGap)
	second, _ := d.Start(ctx, phone, "203.0.113.3")
	if first.SandboxCode != second.SandboxCode {
		if _, err := d.Finish(ctx, phone, first.SandboxCode, "203.0.113.3"); !errors.Is(err, ErrWrongCode) {
			t.Fatalf("the replaced code: %v, want ErrWrongCode", err)
		}
	}
	if _, err := d.Finish(ctx, phone, second.SandboxCode, "203.0.113.3"); err != nil {
		t.Fatalf("the newer code: %v", err)
	}
}

func TestPhoneDoorTextLimits(t *testing.T) {
	ctx := context.Background()
	d := newDoor(t, phoneDB(t))
	const phone = "+447700900104"
	start := d.clock
	if _, err := d.Start(ctx, phone, "203.0.113.4"); err != nil {
		t.Fatal(err)
	}
	var limit *LimitError
	if _, err := d.Start(ctx, phone, "203.0.113.4"); !errors.As(err, &limit) || !limit.RetryAt.Equal(start.Add(TextGap)) {
		t.Fatalf("a second text inside 30 seconds: %v, want a limit until %v", err, start.Add(TextGap))
	}
	for i := 2; i <= TextsPerHour; i++ {
		d.later(TextGap)
		if _, err := d.Start(ctx, phone, "203.0.113.4"); err != nil {
			t.Fatalf("text %d: %v", i, err)
		}
	}
	d.later(TextGap)
	if _, err := d.Start(ctx, phone, "203.0.113.4"); !errors.As(err, &limit) || !limit.RetryAt.Equal(start.Add(time.Hour)) {
		t.Fatalf("the sixth text inside the hour: %v, want a limit until %v", err, start.Add(time.Hour))
	}
	d.clock = start.Add(time.Hour + time.Second)
	if _, err := d.Start(ctx, phone, "203.0.113.4"); err != nil {
		t.Fatalf("after the hour: %v", err)
	}

	// One address, many numbers: the address's own limit.
	a := newDoor(t, d.DB)
	for i := 0; i < DefaultTextsPerAddressHour; i++ {
		if _, err := a.Start(ctx, "+44770090020"+string(rune('0'+i)), "198.51.100.9"); err != nil {
			t.Fatalf("number %d from one address: %v", i, err)
		}
	}
	if _, err := a.Start(ctx, "+447700900299", "198.51.100.9"); !errors.As(err, &limit) {
		t.Fatalf("one address past its hour: %v, want a limit", err)
	}
	var limited int
	if err := d.DB.QueryRow(ctx, `SELECT count(*) FROM phone_signin_events WHERE event = 'limited'`).Scan(&limited); err != nil || limited != 3 {
		t.Fatalf("limits reached are audited: %d rows (%v), want 3", limited, err)
	}
}

// Until the code is right, a number with a person and one without get the same answers.
func TestPhoneDoorSameAnswerWithOrWithoutPerson(t *testing.T) {
	ctx := context.Background()
	d := newDoor(t, phoneDB(t))
	const known, unknown = "+447700900105", "+447700900106"
	st, _ := d.Start(ctx, known, "203.0.113.5")
	fin, _ := d.Finish(ctx, known, st.SandboxCode, "203.0.113.5")
	if _, _, err := d.Redeem(ctx, fin.Ticket, "", "203.0.113.5"); err != nil {
		t.Fatal(err)
	}
	d.later(time.Minute)
	for _, phone := range []string{known, unknown} {
		st, err := d.Start(ctx, phone, "203.0.113.5")
		if err != nil || st.Sender != "sandbox" || !st.ExpiresAt.Equal(d.clock.Add(CodeLife)) {
			t.Fatalf("%s start: %+v %v", phone, st, err)
		}
		if _, err := d.Finish(ctx, phone, wrongCode(st.SandboxCode), "203.0.113.5"); !errors.Is(err, ErrWrongCode) {
			t.Fatalf("%s wrong code: %v", phone, err)
		}
	}
	if _, err := d.Finish(ctx, "+447700900107", "123456", "203.0.113.5"); !errors.Is(err, ErrNoCode) {
		t.Fatalf("a number never sent a code: %v, want ErrNoCode", err)
	}
}

func TestPhoneDoorCodeNeverStoredPlain(t *testing.T) {
	ctx := context.Background()
	d := newDoor(t, phoneDB(t))
	const phone = "+447700900108"
	st, _ := d.Start(ctx, phone, "203.0.113.8")
	_, _ = d.Finish(ctx, phone, wrongCode(st.SandboxCode), "203.0.113.8")
	fin, _ := d.Finish(ctx, phone, st.SandboxCode, "203.0.113.8")
	if _, _, err := d.Redeem(ctx, fin.Ticket, "", "203.0.113.8"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"people", "phone_checks", "phone_tickets", "phone_signin_events"} {
		var dump string
		if err := d.DB.QueryRow(ctx, `SELECT COALESCE(string_agg(row_to_json(t)::text, ' '), '') FROM `+table+` t`).Scan(&dump); err != nil {
			t.Fatal(err)
		}
		if dump == "" && table != "people" {
			t.Fatalf("%s is empty — the scan below would pass over nothing", table)
		}
		if strings.Contains(dump, st.SandboxCode) || strings.Contains(dump, fin.Ticket) {
			t.Fatalf("%s holds the code or the ticket in plain: %s", table, dump)
		}
	}
	// The hash does not verify under another key.
	var salt, hash []byte
	if err := d.DB.QueryRow(ctx, `SELECT code_salt, code_hash FROM phone_checks WHERE phone = $1`, phone).Scan(&salt, &hash); err != nil {
		t.Fatal(err)
	}
	other := &Store{Key: []byte("another-key")}
	if string(d.hashCode(salt, phone, st.SandboxCode)) != string(hash) || string(other.hashCode(salt, phone, st.SandboxCode)) == string(hash) {
		t.Fatal("the code's hash must verify under the store's key and only under it")
	}
}

func TestPhoneDoorSandboxNumberAskedAgainUnderRealSender(t *testing.T) {
	ctx := context.Background()
	d := newDoor(t, phoneDB(t))
	const phone = "+447700900109"
	st, _ := d.Start(ctx, phone, "203.0.113.9")
	sandboxCode := st.SandboxCode
	fin, _ := d.Finish(ctx, phone, sandboxCode, "203.0.113.9")
	p, _, err := d.Redeem(ctx, fin.Ticket, "", "203.0.113.9")
	if err != nil || !p.PhoneSandbox || d.PhoneCheckDue(p) {
		t.Fatalf("under the Sandbox the number is marked and not due: %+v due=%v %v", p, d.PhoneCheckDue(p), err)
	}

	// A real sender is set (a stand-in that keeps what it sends).
	d.later(time.Minute)
	pending, _ := d.Start(ctx, phone, "203.0.113.9")
	fin, _ = d.Finish(ctx, phone, pending.SandboxCode, "203.0.113.9")
	texts := &recordingSender{}
	d.sender = texts
	if !d.PhoneCheckDue(p) {
		t.Fatal("a Sandbox-confirmed number must be asked again under a real sender")
	}
	if _, _, err := d.Redeem(ctx, fin.Ticket, "", "203.0.113.9"); !errors.Is(err, ErrTicket) {
		t.Fatalf("a Sandbox ticket under a real sender: %v, want ErrTicket", err)
	}
	d.later(time.Minute)
	sandboxPending, _ := d.Start(ctx, "+447700900110", "203.0.113.9")
	_ = sandboxPending // sent by the real stand-in, so no code comes back
	if sandboxPending.SandboxCode != "" {
		t.Fatal("a real sender's code must never come back in the answer")
	}
	st, err = d.Start(ctx, phone, "203.0.113.9")
	if err != nil {
		t.Fatal(err)
	}
	if st.SandboxCode != "" || texts.last(phone) == "" {
		t.Fatalf("the real sender texts the code and the answer has none: %+v", st)
	}
	fin, err = d.Finish(ctx, phone, texts.last(phone), "203.0.113.9")
	if err != nil || fin.Sandbox {
		t.Fatalf("the real text's code: %+v %v", fin, err)
	}
	p, _, err = d.Redeem(ctx, fin.Ticket, "", "203.0.113.9")
	if err != nil || p.PhoneSandbox || d.PhoneCheckDue(p) {
		t.Fatalf("a real text clears the mark: %+v %v", p, err)
	}

	// Back under the Sandbox, its handed-back code never signs in a person a real text confirmed.
	d.sender = partners.SandboxTextSender{}
	d.later(time.Minute)
	st, _ = d.Start(ctx, phone, "203.0.113.9")
	fin, err = d.Finish(ctx, phone, st.SandboxCode, "203.0.113.9")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Redeem(ctx, fin.Ticket, "", "203.0.113.9"); !errors.Is(err, ErrTicket) {
		t.Fatalf("a Sandbox ticket for a number a real text confirmed: %v, want ErrTicket", err)
	}
}

func TestPhoneDoorSandboxCodeEndsUnderRealSender(t *testing.T) {
	ctx := context.Background()
	d := newDoor(t, phoneDB(t))
	const phone = "+447700900111"
	st, _ := d.Start(ctx, phone, "203.0.113.11")
	d.sender = &recordingSender{}
	if _, err := d.Finish(ctx, phone, st.SandboxCode, "203.0.113.11"); !errors.Is(err, ErrNoCode) {
		t.Fatalf("a code the Sandbox sent, finished under a real sender: %v, want ErrNoCode", err)
	}
}

func TestPhoneDoorTwoPeopleCannotHoldOneNumber(t *testing.T) {
	ctx := context.Background()
	d := newDoor(t, phoneDB(t))
	const phone = "+447700900112"
	confirm := func(phone string) string {
		d.later(time.Minute)
		st, err := d.Start(ctx, phone, "203.0.113.12")
		if err != nil {
			t.Fatal(err)
		}
		fin, err := d.Finish(ctx, phone, st.SandboxCode, "203.0.113.12")
		if err != nil {
			t.Fatal(err)
		}
		return fin.Ticket
	}
	if _, _, err := d.Redeem(ctx, confirm(phone), "google:alice", "203.0.113.12"); err != nil {
		t.Fatal(err)
	}
	ticket := confirm(phone)
	if _, _, err := d.Redeem(ctx, ticket, "google:bob", "203.0.113.12"); !errors.Is(err, ErrHeld) {
		t.Fatalf("another identity on a held number: %v, want ErrHeld", err)
	}
	if p, _, err := d.Redeem(ctx, ticket, "google:alice", "203.0.113.12"); err != nil || p.Identity != "google:alice" {
		t.Fatalf("a refused ticket is not used up, and its own person signs in: %+v %v", p, err)
	}
	if _, _, err := d.Redeem(ctx, confirm("+447700900113"), "google:alice", "203.0.113.12"); !errors.Is(err, ErrHeld) {
		t.Fatalf("one identity on a second number: %v, want ErrHeld", err)
	}
	if _, err := d.DB.Exec(ctx, `INSERT INTO people (id, phone, identity, phone_sandbox, phone_confirmed_at) VALUES ('per_x', $1, 'x', true, now())`, phone); err == nil {
		t.Fatal("the database must refuse a second person on one number")
	}
	c, err := d.Counts(ctx)
	if err != nil || c.People != 1 || c.JoinedLastDay != 1 || c.RefusedLastDay != 2 {
		t.Fatalf("counts: %+v %v", c, err)
	}
}
