package stepup

import (
	"context"
	"encoding/base32"
	"errors"
	"testing"
	"time"
)

// RFC 6238's SHA-1 test vectors, as six digits: what an authenticator app shows for its test secret.
func TestCodeMatchesRFC6238(t *testing.T) {
	key := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		if got := CodeAt(key, time.Unix(unix, 0)); got != want {
			t.Errorf("code at %d = %s; want %s", unix, got, want)
		}
	}
}

// A code is good for its own step and the ones either side, once, whoever sends it; a wrong one, an old one or an
// unset secret opens nothing.
func TestVerifyAcceptsACurrentCodeOnce(t *testing.T) {
	key := []byte("0123456789abcdef0123")
	v, err := New(base32.StdEncoding.EncodeToString(key), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	v.now = func() time.Time { return now }

	if err := v.Verify(ctx, "nicolai", CodeAt(key, now.Add(-30*time.Second))); err != nil {
		t.Fatalf("the previous step's code: %v", err)
	}
	if err := v.Verify(ctx, "nicolai", CodeAt(key, now)); err != nil {
		t.Fatalf("the current code: %v", err)
	}
	if err := v.Verify(ctx, "nicolai", CodeAt(key, now)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("the same code again = %v; want ErrReplayed", err)
	}
	if err := v.Verify(ctx, "ada", CodeAt(key, now)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("the same code under another operator's name = %v; want ErrReplayed", err)
	}
	for _, bad := range []string{"", "12345", "abcdef", CodeAt(key, now.Add(-2*time.Minute))} {
		if err := v.Verify(ctx, "grace", bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("code %q = %v; want ErrInvalid", bad, err)
		}
	}

	unset, err := New("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := unset.Verify(ctx, "nicolai", CodeAt(key, time.Now())); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("no secret = %v; want ErrNotConfigured", err)
	}
	if _, err := New("JBSWY3DPEHPK3PXP", nil); err == nil {
		t.Fatal("a 10-byte secret was taken; it needs 16")
	}
}

// B28.461 — five wrong codes inside ten minutes and step-up waits fifteen, a good code included; after the wait a good
// code works; a good code before the fifth clears the count; a missing or malformed code is never counted.
func TestVerifyWaitsAfterFiveWrongCodes(t *testing.T) {
	key := []byte("0123456789abcdef0123")
	ctx := context.Background()
	at := time.Unix(1_800_000_000, 0)
	newV := func() *Verifier {
		v, err := New(base32.StdEncoding.EncodeToString(key), nil)
		if err != nil {
			t.Fatal(err)
		}
		v.now = func() time.Time { return at }
		return v
	}
	wrong := func(v *Verifier) error { return v.Verify(ctx, "nicolai", "000000") }
	if CodeAt(key, at) == "000000" {
		t.Fatal("the test's wrong code is the good one")
	}
	var locked *ErrLocked

	v := newV()
	for i := 1; i < MaxWrong; i++ {
		if err := wrong(v); !errors.Is(err, ErrInvalid) {
			t.Fatalf("wrong code %d = %v; want ErrInvalid", i, err)
		}
	}
	for _, empty := range []string{"", "  ", "12345", "abcdef"} { // not guesses: still four counted
		if err := v.Verify(ctx, "ada", empty); !errors.Is(err, ErrInvalid) {
			t.Fatalf("code %q = %v; want ErrInvalid", empty, err)
		}
	}
	if err := wrong(v); !errors.As(err, &locked) || !locked.Started || locked.RetryAfter != LockFor {
		t.Fatalf("the fifth wrong code = %v; want the wait to start, %s", err, LockFor)
	}
	if err := v.Verify(ctx, "ada", "111111"); !errors.As(err, &locked) || locked.Started { // another operator: one count
		t.Fatalf("the sixth wrong code, under another name = %v; want ErrLocked, not started again", err)
	}
	at = at.Add(time.Minute)
	if err := v.Verify(ctx, "nicolai", CodeAt(key, at)); !errors.As(err, &locked) || locked.RetryAfter != LockFor-time.Minute {
		t.Fatalf("a good code during the wait = %v; want ErrLocked for %s", err, LockFor-time.Minute)
	}
	at = at.Add(LockFor)
	if err := v.Verify(ctx, "nicolai", CodeAt(key, at)); err != nil {
		t.Fatalf("a good code after the wait: %v", err)
	}

	// A good code before the fifth wrong one clears the count: four wrong, good, four wrong is no wait.
	v = newV()
	for i := 1; i < MaxWrong; i++ {
		_ = wrong(v)
	}
	if err := v.Verify(ctx, "nicolai", CodeAt(key, at)); err != nil {
		t.Fatalf("a good code after four wrong: %v", err)
	}
	for i := 1; i < MaxWrong; i++ {
		if err := wrong(v); !errors.Is(err, ErrInvalid) {
			t.Fatalf("wrong code %d after a good one = %v; want ErrInvalid: the count was not cleared", i, err)
		}
	}

	// Wrong codes more than ten minutes apart do not add up.
	v = newV()
	for i := 1; i < MaxWrong; i++ {
		_ = wrong(v)
	}
	at = at.Add(GuessWindow)
	if err := wrong(v); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a fifth wrong code ten minutes after the first = %v; want ErrInvalid", err)
	}
}
