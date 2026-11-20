package phonesignin

import (
	"context"
	"errors"
	"testing"
)

// B50.52 — a Store built with no passcode rule still asks the person's passcode: a person whose only way in is a
// passcode adds no first key with a session alone.
func TestB5052NoPasscodeRuleStillAsksThePasscode(t *testing.T) {
	ctx := context.Background()
	s, pool := keyStore(t)
	s.PasscodeSecret = []byte("passcode-secret-for-tests-0123456789")
	newPerson(t, pool, "per_b5052", "+447700905201")
	if err := s.ReplacePasscodeAfterKey(ctx, "per_b5052", testPasscode, ""); err != nil {
		t.Fatal(err)
	}
	add := func(proof KeyProof) error {
		rc, err := s.RegisterChallenge(ctx, "per_b5052", "")
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.AddKey(ctx, "per_b5052", NewKey{Name: "Phone", Passkey: newAuthenticator(t).register(t, rc.Challenge)}, proof, "")
		return err
	}
	if err := add(KeyProof{}); !errors.Is(err, ErrKeyProof) {
		t.Fatalf("a first key with a passcode set, no rule wired and no proof = %v, want ErrKeyProof", err)
	}
	if keys, _ := s.ListKeys(ctx, "per_b5052"); len(keys) != 0 {
		t.Fatalf("a key was kept with no proof: %v", keys)
	}
	if err := add(KeyProof{Passcode: testPasscode}); err != nil {
		t.Fatalf("a first key with the right passcode: %v", err)
	}
}
