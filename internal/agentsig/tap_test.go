package agentsig

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"
)

// B62.6 — the Trusted Agent Protocol's agent recognition signature, signed and checked as Visa's merchant
// specification gives it (developer.visa.com/capabilities/trusted-agent-protocol/trusted-agent-protocol-specifications).

func tapKey(t *testing.T) (ed25519.PrivateKey, TAPKeys) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, func(_ context.Context, keyID string) (ed25519.PublicKey, error) {
		if keyID != "agent-key" {
			return nil, ErrUnknownKey
		}
		return pub, nil
	}
}

var tapNow = time.Unix(1735689600, 0)

func signedTAP(t *testing.T, priv ed25519.PrivateKey, life time.Duration) (string, string) {
	t.Helper()
	in, sig, err := SignTAP(priv, "shop.example", "/checkout", TAPSignature{Tag: TAPTagPay, KeyID: "agent-key", Nonce: "n-1",
		Created: tapNow, Expires: tapNow.Add(life)})
	if err != nil {
		t.Fatal(err)
	}
	return in, sig
}

func TestTAPSignatureVerifiesWithItsTagKeyAndNonce(t *testing.T) {
	priv, keys := tapKey(t)
	in, sig := signedTAP(t, priv, TAPMaxLife)
	for _, p := range []string{`("@authority" "@path")`, `alg="Ed25519"`, `tag="agent-payer-auth"`, `nonce="n-1"`, `created=1735689600`, `expires=1735690080`} {
		if !strings.Contains(in, p) {
			t.Fatalf("Signature-Input %s lacks %s", in, p)
		}
	}
	got, err := VerifyTAP(context.Background(), "shop.example", "/checkout", in, sig, keys, fakeNonces{}, tapNow.Add(time.Minute))
	if err != nil || got.Tag != TAPTagPay || got.KeyID != "agent-key" || got.Nonce != "n-1" {
		t.Fatalf("VerifyTAP = %+v, %v", got, err)
	}
}

func TestTAPSignatureForAnotherPathFails(t *testing.T) {
	priv, keys := tapKey(t)
	in, sig := signedTAP(t, priv, TAPMaxLife)
	_, err := VerifyTAP(context.Background(), "shop.example", "/admin", in, sig, keys, fakeNonces{}, tapNow.Add(time.Minute))
	var refused *Error
	if !errors.As(err, &refused) || refused.Reason != "the signature does not verify" {
		t.Fatalf("another path = %v", err)
	}
}

func TestTAPSignatureLongerThanEightMinutesFails(t *testing.T) {
	priv, keys := tapKey(t)
	in, sig := signedTAP(t, priv, TAPMaxLife+time.Second)
	if _, err := VerifyTAP(context.Background(), "shop.example", "/checkout", in, sig, keys, fakeNonces{}, tapNow.Add(time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "at most 8 minutes") {
		t.Fatalf("a 481-second signature = %v", err)
	}
}

func TestTAPNonceIsUsedOnce(t *testing.T) {
	priv, keys := tapKey(t)
	in, sig := signedTAP(t, priv, TAPMaxLife)
	seen := fakeNonces{}
	if _, err := VerifyTAP(context.Background(), "shop.example", "/checkout", in, sig, keys, seen, tapNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyTAP(context.Background(), "shop.example", "/checkout", in, sig, keys, seen, tapNow.Add(time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "already used") {
		t.Fatalf("the replay = %v", err)
	}
}

// Visa's specification publishes an example signature but not the key that made it, so it cannot be verified: what can
// be checked is that our verifier reads the published header as the specification means it — every parameter
// accepted, and the key it names looked up — and stops only at the key, which Visa's directory holds.
func TestVisaPublishedExampleIsReadUpToItsKey(t *testing.T) {
	in := `sig2=("@authority" "@path");created=1735689600;expires=1735693200;keyid="poqkLGiymh_W0uP6PZFw-dvez3QJT5SolqXBCW38r0U";` +
		`alg="Ed25519";nonce="e8N7S2MFd/qrd6T2R3tdfAuuANngKI7LFtKYI/vowzk4lAZyadIX6wW25MwG7DCT9RUKAJ0qVkU0mEeLElW1qg==";tag="agent-browser-auth"`
	sig := `sig2=:jdq0SqOwHdyHr9+r5jw3iYZH6aNGKijYp/EstF4RQTQdi5N5YYKrD+mCT1HA1nZDsi6nJKuHxUi/5Syp3rLWBA==:`
	var asked string
	keys := func(_ context.Context, keyID string) (ed25519.PublicKey, error) { asked = keyID; return nil, ErrUnknownKey }
	// The example lives an hour, created to expires; the specification's own rule allows 8 minutes, so the example is
	// refused for that before its key is asked for. Within the rule, with the example's other parameters, it reads.
	if _, err := VerifyTAP(context.Background(), "example.com", "/example-product", in, sig, keys, fakeNonces{}, tapNow.Add(time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "at most 8 minutes") {
		t.Fatalf("the published example's hour = %v", err)
	}
	in = strings.Replace(in, "expires=1735693200", "expires=1735690080", 1)
	_, err := VerifyTAP(context.Background(), "example.com", "/example-product", in, sig, keys, fakeNonces{}, tapNow.Add(time.Minute))
	if asked != "poqkLGiymh_W0uP6PZFw-dvez3QJT5SolqXBCW38r0U" || err == nil || !strings.Contains(err.Error(), "is not known") {
		t.Fatalf("the published example asked for key %q: %v", asked, err)
	}
}
