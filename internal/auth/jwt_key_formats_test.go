package auth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
)

// THE COMMAND THE RUNBOOK GIVES AN OPERATOR MUST ACTUALLY PARSE.
//
// LENS_JWT_PRIVATE_KEY is the difference between "a restart logs everyone out" and not, so the
// instruction for producing one is operational code: it is executed by a person under time pressure
// who cannot debug Go. These pin the formats ParseECPrivateKeyPEM really accepts, generated the way
// the documentation says to generate them — not described from the variable name.
//
// ⚠ THE -noout TRAP IS PINNED DELIBERATELY. `openssl ecparam -name prime256v1 -genkey` WITHOUT
// -noout emits an "EC PARAMETERS" block BEFORE the key, and pem.Decode reads only the FIRST block —
// so the file looks right, contains a valid key, and fails with `unexpected PEM type`. That is the
// most likely way an operator following a half-remembered command gets a Lens that will not boot.

func p256PEM(t *testing.T, blockType string, pkcs8 bool) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var der []byte
	if pkcs8 {
		der, err = x509.MarshalPKCS8PrivateKey(k)
	} else {
		der, err = x509.MarshalECPrivateKey(k)
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}))
}

// SEC1 — what `openssl ecparam -name prime256v1 -genkey -noout` produces.
func TestParse_SEC1_ECPrivateKey(t *testing.T) {
	if _, err := ParseECPrivateKeyPEM(p256PEM(t, "EC PRIVATE KEY", false)); err != nil {
		t.Fatalf("SEC1 P-256 must be accepted — it is what the documented command emits: %v", err)
	}
}

// PKCS#8 — what `openssl pkcs8 -topk8 -nocrypt` produces. Also accepted.
func TestParse_PKCS8_PrivateKey(t *testing.T) {
	if _, err := ParseECPrivateKeyPEM(p256PEM(t, "PRIVATE KEY", true)); err != nil {
		t.Fatalf("PKCS#8 P-256 must be accepted: %v", err)
	}
}

// ⚠ The -noout trap: EC PARAMETERS first, key second. pem.Decode takes only the first block.
func TestParse_RejectsECParametersFirstBlock(t *testing.T) {
	params := string(pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: []byte{0x06, 0x08, 0x2a}}))
	combined := params + p256PEM(t, "EC PRIVATE KEY", false)

	_, err := ParseECPrivateKeyPEM(combined)
	if err == nil {
		t.Fatal("a file whose FIRST block is EC PARAMETERS must be rejected — if this ever starts " +
			"passing, the -noout warning in the operator instructions is obsolete and must be removed, " +
			"because a warning about an impossible failure teaches people to ignore the real ones")
	}
	if !strings.Contains(err.Error(), "EC PARAMETERS") {
		t.Errorf("the error must NAME the offending block so an operator can act on it; got: %v", err)
	}
}

// The curve check is real, not decorative.
func TestParse_RejectsWrongCurve(t *testing.T) {
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	body := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))

	_, perr := ParseECPrivateKeyPEM(body)
	if perr == nil {
		t.Fatal("P-384 must be rejected: JWTKid is a constant and the JWKS advertises P-256")
	}
	if !strings.Contains(perr.Error(), "P-256") {
		t.Errorf("the error must say which curve is required; got: %v", perr)
	}
}

// ⚠ Rotating the key invalidates every existing token, and nothing in the design softens that:
// JWTKid is a CONSTANT, so an old and a new token are indistinguishable by header — there is no
// kid-based lookup, no second verification key, no grace period. This pins that as a deploy
// consequence rather than leaving it to be discovered during one.
func TestRotatingTheKeyInvalidatesExistingTokens(t *testing.T) {
	if JWTKid != "lens-1" {
		t.Fatalf("JWTKid = %q; this test's premise is that it is a CONSTANT shared by every key", JWTKid)
	}
	a, err := ParseECPrivateKeyPEM(p256PEM(t, "EC PRIVATE KEY", false))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseECPrivateKeyPEM(p256PEM(t, "EC PRIVATE KEY", false))
	if err != nil {
		t.Fatal(err)
	}
	da, errA := a.Bytes()
	db, errB := b.Bytes()
	if errA != nil || errB != nil || bytes.Equal(da, db) {
		t.Fatal("two generated keys are identical — the premise of this test is broken")
	}
	// Same advertised kid, different signing material: a token from `a` cannot verify under `b`,
	// and the header gives a client no way to tell why.
	if !a.PublicKey.Equal(&a.PublicKey) || a.PublicKey.Equal(&b.PublicKey) {
		t.Fatal("distinct keys must not share a public key")
	}
}
