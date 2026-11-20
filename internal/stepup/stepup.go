// Package stepup is B30.8's step-up authentication for the operator's compliance actions: on top of the admin key,
// a six-digit time-based code (RFC 6238 — HMAC-SHA1, 30-second steps) from the authenticator app holding
// LENS_OPERATOR_STEP_UP_SECRET. A code is good for its own step and the one either side of it, and once only — by
// anyone, on any replica (Claims) — so one seen over a shoulder or in a log opens nothing.
//
// B28.461: and it cannot be guessed. Five wrong codes inside ten minutes and every step-up action waits fifteen minutes,
// a good code included (ErrLocked); a good code before the fifth clears the count. The count is one for the whole
// step-up, whatever operator is named, because the secret is one, and it is kept on every replica (Guesses). A missing
// or malformed code is not a guess and is not counted.
package stepup

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 6238's HMAC-SHA1: what every authenticator app computes
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	step   = 30 * time.Second
	digits = 6
	// minSecretBytes is RFC 4226's floor for the shared secret: 128 bits.
	minSecretBytes = 16

	// MaxWrong wrong codes inside GuessWindow start a wait of LockFor.
	MaxWrong    = 5
	GuessWindow = 10 * time.Minute
	LockFor     = 15 * time.Minute
)

var (
	// ErrNotConfigured: no step-up secret is set, so no step-up action can be authorised.
	ErrNotConfigured = errors.New("stepup: no step-up secret is set (LENS_OPERATOR_STEP_UP_SECRET), so this action cannot be authorised")
	// ErrInvalid: the code is missing, malformed, wrong or out of date.
	ErrInvalid = errors.New("stepup: the step-up code is wrong or out of date")
	// ErrReplayed: someone already used this code.
	ErrReplayed = errors.New("stepup: this step-up code was already used; wait for the next one")
)

// ErrLocked is answered for every code while step-up waits after too many wrong ones. Started is true for the one
// wrong code that began the wait, on whichever replica it reached.
type ErrLocked struct {
	RetryAfter time.Duration
	Started    bool
}

func (e *ErrLocked) Error() string {
	return fmt.Sprintf("stepup: too many wrong step-up codes; step-up waits %s", e.RetryAfter.Round(time.Second))
}

// Guesses is the wrong-code count: Update runs fn on the one State under a lock — across replicas when kept in
// Postgres — and keeps what fn returns.
type Guesses interface {
	Update(ctx context.Context, fn func(State) State) error
}

// State is the wrong-code count: Wrong codes since WindowStart, and the wait, if one began, until LockedUntil.
type State struct {
	Wrong       int
	WindowStart time.Time
	LockedUntil time.Time
}

// memGuesses is Guesses in this process alone: for one replica, and for tests.
type memGuesses struct {
	mu sync.Mutex
	s  State
}

func (m *memGuesses) Update(_ context.Context, fn func(State) State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s = fn(m.s)
	return nil
}

// Claims records each step whose code was used: Claim answers true the first time a step is claimed, and false ever
// after. The secret is one, so a step's code is the same for every operator: it is claimed once, whoever uses it.
type Claims interface {
	Claim(ctx context.Context, step int64, operator string) (bool, error)
}

// memClaims is Claims in this process alone: for one replica, and for tests.
type memClaims struct {
	mu   sync.Mutex
	used map[int64]bool
}

func (m *memClaims) Claim(_ context.Context, step int64, _ string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.used[step] {
		return false, nil
	}
	m.used[step] = true
	return true, nil
}

// Verifier checks step-up codes against one shared secret.
type Verifier struct {
	secret  []byte
	now     func() time.Time
	claims  Claims
	guesses Guesses
	onLock  func(ctx context.Context, operator, target string) error
}

// New is a Verifier for the base32 secret an authenticator app was given, its used codes kept by claims — in this
// process alone when claims is nil — and its wrong codes counted where claims keeps them when claims is also Guesses
// (PostgresClaims is), else in this process. An empty secret is a Verifier that refuses every code with
// ErrNotConfigured.
func New(secret string, claims Claims) (*Verifier, error) {
	if claims == nil {
		claims = &memClaims{used: map[int64]bool{}}
	}
	guesses, ok := claims.(Guesses)
	if !ok {
		guesses = &memGuesses{}
	}
	v := &Verifier{now: time.Now, claims: claims, guesses: guesses}
	secret = strings.ToUpper(strings.Join(strings.Fields(secret), ""))
	if secret == "" {
		return v, nil
	}
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimRight(secret, "="))
	if err != nil {
		return nil, fmt.Errorf("stepup: LENS_OPERATOR_STEP_UP_SECRET is not base32: %w", err)
	}
	if len(key) < minSecretBytes {
		return nil, fmt.Errorf("stepup: LENS_OPERATOR_STEP_UP_SECRET is %d bytes; it needs at least %d", len(key), minSecretBytes)
	}
	v.secret = key
	return v, nil
}

// Configured says whether a secret is set.
func (v *Verifier) Configured() bool { return len(v.secret) > 0 }

// OnLock is called once when a wait begins, with the operator named and the action's target, by whoever answered
// the wrong code that began it: the operator audit row. It is set once, before the Verifier serves.
func (v *Verifier) OnLock(fn func(ctx context.Context, operator, target string) error) { v.onLock = fn }

// RecordLock writes the wait's start through OnLock's fn, if one is set.
func (v *Verifier) RecordLock(ctx context.Context, operator, target string) error {
	if v.onLock == nil {
		return nil
	}
	return v.onLock(ctx, operator, target)
}

// Verify accepts code for operator once: a code for the current step or the one either side of it that nobody has
// used yet, while step-up is not waiting after too many wrong codes (ErrLocked).
func (v *Verifier) Verify(ctx context.Context, operator, code string) error {
	if !v.Configured() {
		return ErrNotConfigured
	}
	code = strings.TrimSpace(code)
	if len(code) != digits || strings.Trim(code, "0123456789") != "" {
		return ErrInvalid
	}
	at := v.now()
	now := at.Unix() / int64(step/time.Second)
	match := int64(0)
	matched := false
	for _, c := range []int64{now, now - 1, now + 1} {
		if hmac.Equal([]byte(Code(v.secret, c)), []byte(code)) {
			match, matched = c, true
			break
		}
	}

	var locked *ErrLocked
	if err := v.guesses.Update(ctx, func(s State) State {
		if s.LockedUntil.After(at) {
			locked = &ErrLocked{RetryAfter: s.LockedUntil.Sub(at)}
			return s
		}
		if !s.LockedUntil.IsZero() || at.Sub(s.WindowStart) >= GuessWindow {
			s = State{} // the wait is over, or the window is: count afresh
		}
		if matched {
			return s // counted only once the code is claimed, below
		}
		if s.Wrong == 0 {
			s.WindowStart = at
		}
		s.Wrong++
		if s.Wrong >= MaxWrong {
			s.LockedUntil = at.Add(LockFor)
			locked = &ErrLocked{RetryAfter: LockFor, Started: true}
		}
		return s
	}); err != nil {
		return fmt.Errorf("stepup: count the code: %w", err)
	}
	switch {
	case locked != nil:
		return locked
	case !matched:
		return ErrInvalid
	}

	first, err := v.claims.Claim(ctx, match, operator)
	switch {
	case err != nil:
		return fmt.Errorf("stepup: record the code's use: %w", err)
	case !first:
		return ErrReplayed
	}
	if err := v.guesses.Update(ctx, func(s State) State {
		if s.LockedUntil.After(at) {
			return s // a wait another replica began meanwhile stands
		}
		return State{}
	}); err != nil {
		return fmt.Errorf("stepup: clear the wrong-code count: %w", err)
	}
	return nil
}

// Code is the code for key at step counter (RFC 4226's HOTP with RFC 6238's counter).
func Code(key []byte, counter int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", digits, n%1_000_000)
}

// CodeAt is the code for key at t: what an authenticator app shows then.
func CodeAt(key []byte, t time.Time) string { return Code(key, t.Unix()/int64(step/time.Second)) }
