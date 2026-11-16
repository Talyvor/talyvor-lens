package phonesignin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/devicekey"
)

// B61.81 — five tightenings to a person's sign-in keys, on a migrated schema. The route-side ones (removing a key
// needs proof; a register challenge names only the person's own account) are in cmd/lens.

// A ticket from a key that has since been removed adds no new key.
func TestB6181RemovedKeysTicketAddsNoKey(t *testing.T) {
	s, pool := keyStore(t)
	ctx := context.Background()
	newPerson(t, pool, "per_a", "+447700908101")
	a := signUpKey(t, s, "per_a")
	in, err := signIn(t, s, a, "per_a")
	if err != nil {
		t.Fatal(err)
	}
	// The key goes while its ticket is still inside its 5 minutes.
	if _, err := pool.Exec(ctx, `UPDATE person_keys SET removed_at = now() WHERE id = $1`, in.KeyID); err != nil {
		t.Fatal(err)
	}
	s.Passcodes = fakePasscodes{has: true, right: "284719"} // a way in remains, so a key may be added with proof
	rc, err := s.RegisterChallenge(ctx, "per_a", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddKey(ctx, "per_a", NewKey{Name: "Stranger's laptop", Passkey: newAuthenticator(t).register(t, rc.Challenge)},
		KeyProof{Ticket: in.Ticket}, ""); !errors.Is(err, ErrKeyProof) {
		t.Fatalf("a new key on a removed key's ticket = %v, want ErrKeyProof", err)
	}
	if keys, _ := s.ListKeys(ctx, "per_a"); len(keys) != 0 {
		t.Fatalf("per_a has %d keys, want none", len(keys))
	}
}

// Challenges and tickets a day past their time are deleted when a new one is issued; younger ones stay.
func TestB6181OldChallengesAndTicketsAreDeleted(t *testing.T) {
	s, pool := keyStore(t)
	ctx := context.Background()
	newPerson(t, pool, "per_a", "+447700908111")
	a := signUpKey(t, s, "per_a")
	keys, _ := s.ListKeys(ctx, "per_a")
	old, recent := time.Now().Add(-KeyRowsKept-time.Hour), time.Now().Add(-KeyRowsKept+time.Hour)
	for i, at := range []time.Time{old, recent} {
		if _, err := pool.Exec(ctx, `INSERT INTO person_key_challenges (challenge, purpose, origin, created_at, expires_at)
			VALUES ($1, 'signin', $2, $3, $3)`, []string{"old-challenge", "recent-challenge"}[i], testOrigin, at); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO person_key_tickets (ticket_hash, person_id, key_id, created_at, expires_at)
			VALUES ($1, 'per_a', $2, $3, $3)`, []byte{byte(i)}, keys[0].ID, at); err != nil {
			t.Fatal(err)
		}
	}
	// A sign-in issues a challenge and then a ticket.
	if _, err := signIn(t, s, a, "per_a"); err != nil {
		t.Fatal(err)
	}
	var oldLeft, recentLeft int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM person_key_challenges WHERE challenge = 'old-challenge') + (SELECT count(*) FROM person_key_tickets WHERE ticket_hash = '\x00'),
		(SELECT count(*) FROM person_key_challenges WHERE challenge = 'recent-challenge') + (SELECT count(*) FROM person_key_tickets WHERE ticket_hash = '\x01')`).
		Scan(&oldLeft, &recentLeft); err != nil {
		t.Fatal(err)
	}
	if oldLeft != 0 || recentLeft != 2 {
		t.Fatalf("after a new challenge and ticket: %d rows a day past their time left (want 0), %d younger (want 2)", oldLeft, recentLeft)
	}
}

// The same device key, added for two people at the same instant, is kept for one of them only.
func TestB6181SameKeyForTwoPeopleAtOnceIsKeptOnce(t *testing.T) {
	s, pool := keyStore(t)
	ctx := context.Background()
	newPerson(t, pool, "per_a", "+447700908121")
	newPerson(t, pool, "per_b", "+447700908122")
	dk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&dk.PublicKey)
	regs := map[string]*devicekey.Registration{}
	for _, p := range []string{"per_a", "per_b"} {
		rc, err := s.RegisterChallenge(ctx, p, "")
		if err != nil {
			t.Fatal(err)
		}
		regs[p] = &devicekey.Registration{Platform: devicekey.PlatformTest, PublicKey: spki, Challenge: rc.Challenge,
			Signature: deviceSign(t, dk, rc.Challenge)}
	}
	// Hold every add at its audit row, after its "kept already" read and its insert, so both reads happen before
	// either add commits.
	hold, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if _, err := hold.Exec(ctx, `LOCK TABLE phone_signin_events IN EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := map[string]error{}
	var mu sync.Mutex
	for _, p := range []string{"per_a", "per_b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.AddKey(ctx, p, NewKey{Name: "iPhone", Device: regs[p]}, KeyProof{}, "")
			mu.Lock()
			errs[p] = err
			mu.Unlock()
		}()
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		var waiting int
		// Waiting on this hold, or on an add that waits on it.
		if err := hold.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity a WHERE pg_backend_pid() = ANY (pg_blocking_pids(a.pid))
			OR EXISTS (SELECT 1 FROM pg_stat_activity b WHERE b.pid = ANY (pg_blocking_pids(a.pid))
				AND pg_backend_pid() = ANY (pg_blocking_pids(b.pid)))`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the two adds did not both wait (%d waiting)", waiting)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	var kept int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM person_keys WHERE public_key = $1`, spki).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Fatalf("the same key is kept %d times (per_a: %v, per_b: %v), want once", kept, errs["per_a"], errs["per_b"])
	}
	refused := 0
	for _, err := range errs {
		if err != nil {
			if !errors.Is(err, ErrKeyKept) {
				t.Fatalf("the second add = %v, want ErrKeyKept", err)
			}
			refused++
		}
	}
	if refused != 1 {
		t.Fatalf("%d adds refused, want one (%v)", refused, errs)
	}
}
