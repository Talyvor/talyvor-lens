package attestation

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/talyvor/lens/internal/povi"
)

// ---- test PKI: a self-generated CA + leaf, standing in for NVIDIA's root + NRAS signing cert. This proves
// the verify LOGIC against REAL signatures + REAL x509 chain validation; only the trust ANCHOR (root) is a
// test CA rather than NVIDIA's production root (that is the LENS_TEST_NVIDIA_EAT go-live gate). ----

type testPKI struct {
	root     *x509.Certificate
	rootKey  *ecdsa.PrivateKey
	leaf     *x509.Certificate
	leafKey  *ecdsa.PrivateKey
	rootPool *x509.CertPool
}

// certValidFrom/Until are within x509's supported range and bracket fixedNow (below).
var certValidFrom = time.Unix(1_000_000_000, 0)  // 2001
var certValidUntil = time.Unix(4_000_000_000, 0) // 2096

func mkCert(t *testing.T, tmpl, parent *x509.Certificate, pub interface{}, signer interface{}) *x509.Certificate {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	rootKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rootTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "TEST NVIDIA Root"},
		NotBefore: certValidFrom, NotAfter: certValidUntil,
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	root := mkCert(t, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)

	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "TEST NRAS Signer"},
		NotBefore: certValidFrom, NotAfter: certValidUntil,
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	leaf := mkCert(t, leafTmpl, root, &leafKey.PublicKey, rootKey)

	pool := x509.NewCertPool()
	pool.AddCert(root)
	return &testPKI{root: root, rootKey: rootKey, leaf: leaf, leafKey: leafKey, rootPool: pool}
}

// staticJWKS maps a kid to a (leaf, intermediates) chain — the hand-rolled JWKS source under test.
type staticJWKS struct {
	kid           string
	leaf          *x509.Certificate
	intermediates *x509.CertPool
	fetches       int
}

func (s *staticJWKS) CertForKID(_ context.Context, kid string) (*x509.Certificate, *x509.CertPool, error) {
	s.fetches++
	if kid != s.kid {
		return nil, nil, errUnknownKID
	}
	return s.leaf, s.intermediates, nil
}

const (
	testNonce = int64(918273645)
	testKID   = "nras-kid-1"
)

// signEAT builds a real ES256-signed JWT with the NVIDIA EAT claim shape.
func signEAT(t *testing.T, pki *testPKI, mutate func(jwt.MapClaims)) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":       "NRAS",
		"iat":       time.Unix(1_500_000_000, 0).Unix(), // 2017
		"exp":       time.Unix(3_000_000_000, 0).Unix(), // 2065
		"eat_nonce": testNonce,
		"x-nvidia-gpu-attestation-report-cc-mode": true,
		"x-nvidia-gpu-hwmodel":                    "H100",
		"measres":                                 "comparison-successful",
	}
	if mutate != nil {
		mutate(claims)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = testKID
	s, err := tok.SignedString(pki.leafKey)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// tamperSig corrupts an EAT's ES256 signature DETERMINISTICALLY and SELF-VERIFYINGLY. This is a crypto
// NEGATIVE test: its only job is to prove a tampered signature is REJECTED, so the tamper must ALWAYS change
// the signature the verifier actually checks — otherwise the test silently proves nothing.
//
// The old form `s[:len(s)-2] + "xx"` blind-replaced the last two base64url characters. That is intermittently
// a NO-OP: on a lenient JWT decoder (golang-jwt v5's default — Verify does not use WithStrictDecoding), "xx"
// is a non-canonical encoding of the byte 0xC7, and 0xC7's canonical final group is "xw". So whenever a fresh
// signature's last byte was already 0xC7 (~1/256 of runs), "xw"→"xx" decoded to the SAME signature bytes,
// verification correctly returned nil, and the negative test flaked with "tampered-sig must REJECT, got nil
// error" (issue #285). The lucky ~1/256 was the LOUD failure; the other 255/256 silently exercised a real
// tamper — but nothing guaranteed that, so the test could have regressed to always-no-op undetected.
//
// The fix: decode the signature, flip one bit of a signature byte, re-encode — a byte-level mutation is
// guaranteed to change the decoded signature — then ASSERT the token actually changed so a future no-op fails
// loudly instead of silently passing.
func tamperSig(t *testing.T, rawEAT string) string {
	t.Helper()
	dot := strings.LastIndexByte(rawEAT, '.')
	if dot < 0 || dot == len(rawEAT)-1 {
		t.Fatalf("tamperSig: not a JWT (no signature segment): %q", rawEAT)
	}
	sig, err := base64.RawURLEncoding.DecodeString(rawEAT[dot+1:])
	if err != nil {
		t.Fatalf("tamperSig: decode signature segment: %v", err)
	}
	if len(sig) == 0 {
		t.Fatal("tamperSig: empty signature segment")
	}
	sig[0] ^= 0x01 // single-bit flip ⇒ the decoded signature is ALWAYS different from the original
	tampered := rawEAT[:dot+1] + base64.RawURLEncoding.EncodeToString(sig)
	if tampered == rawEAT {
		t.Fatal("tamperSig: tamper was a NO-OP (signature unchanged) — a negative test must actually tamper")
	}
	return tampered
}

func fixedNow() time.Time { return time.Unix(2_000_000_000, 0) } // 2033 — between iat and exp, within cert validity

func nodeKeyHashHex(pub []byte) string { h := sha256.Sum256(pub); return hex.EncodeToString(h[:]) }

// (proof 1) REAL-CRYPTO: a good EAT passes real signature verify + real x509 chain-to-root + all claims.
func TestVerify_RealCrypto_Pass(t *testing.T) {
	pki := newTestPKI(t)
	jwks := &staticJWKS{kid: testKID, leaf: pki.leaf, intermediates: x509.NewCertPool()}
	v := NewVerifier(pki.rootPool, jwks, fixedNow)

	res, err := v.Verify(context.Background(), signEAT(t, pki, nil), testNonce, nil)
	if err != nil {
		t.Fatalf("valid EAT must verify, got %v", err)
	}
	if !res.CCMode || res.GPUClass != "H100" {
		t.Fatalf("result wrong: %+v", res)
	}
	if res.KeyBound {
		t.Fatal("no report_data ⇒ key_bound must be false")
	}
	if res.EATDigest == "" {
		t.Fatal("eat_digest must be set")
	}
}

// (proof 1 negatives) each must REJECT.
func TestVerify_RealCrypto_Negatives(t *testing.T) {
	pki := newTestPKI(t)
	good := &staticJWKS{kid: testKID, leaf: pki.leaf, intermediates: x509.NewCertPool()}

	cases := []struct {
		name  string
		roots *x509.CertPool
		eat   func(t *testing.T) string
	}{
		// tampered-sig: deterministically corrupt the signature (see tamperSig) — never the old blind
		// last-two-chars replacement, which was intermittently a no-op (issue #285).
		{"tampered-sig", pki.rootPool, func(t *testing.T) string { return tamperSig(t, signEAT(t, pki, nil)) }},
		{"x5c-to-wrong-root", newTestPKI(t).rootPool, func(t *testing.T) string { return signEAT(t, pki, nil) }}, // verifier trusts a DIFFERENT root
		{"expired", pki.rootPool, func(t *testing.T) string {
			return signEAT(t, pki, func(c jwt.MapClaims) { c["exp"] = time.Unix(1_600_000_000, 0).Unix() }) // 2020 < fixedNow
		}},
		{"no-exp", pki.rootPool, func(t *testing.T) string { // B50.3: an EAT that never expires is refused
			return signEAT(t, pki, func(c jwt.MapClaims) { delete(c, "exp") })
		}},
		{"iss-not-NRAS", pki.rootPool, func(t *testing.T) string {
			return signEAT(t, pki, func(c jwt.MapClaims) { c["iss"] = "evil" })
		}},
		{"missing-cc-mode", pki.rootPool, func(t *testing.T) string {
			return signEAT(t, pki, func(c jwt.MapClaims) { delete(c, "x-nvidia-gpu-attestation-report-cc-mode") })
		}},
		{"cc-mode-false", pki.rootPool, func(t *testing.T) string {
			return signEAT(t, pki, func(c jwt.MapClaims) { c["x-nvidia-gpu-attestation-report-cc-mode"] = false })
		}},
		{"measurements-absent", pki.rootPool, func(t *testing.T) string {
			return signEAT(t, pki, func(c jwt.MapClaims) { delete(c, "measres") })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := NewVerifier(tc.roots, good, fixedNow)
			if _, err := v.Verify(context.Background(), tc.eat(t), testNonce, nil); err == nil {
				t.Fatalf("%s must REJECT, got nil error", tc.name)
			}
		})
	}
	// eat_nonce mismatch (verify called with a different wantNonce than the EAT carries).
	v := NewVerifier(pki.rootPool, good, fixedNow)
	if _, err := v.Verify(context.Background(), signEAT(t, pki, nil), testNonce+1, nil); err == nil {
		t.Fatal("eat_nonce mismatch must REJECT")
	}
}

// (proof 3 + 4) KEY-BINDING + RELAY RESIDUAL: report_data==H(pubkey) ⇒ key_bound=true; absent OR bound to a
// different key ⇒ verified but key_bound=FALSE (never a hard fail) — the fence step (c) pays behind.
func TestVerify_KeyBinding_And_RelayResidual(t *testing.T) {
	pki := newTestPKI(t)
	jwks := &staticJWKS{kid: testKID, leaf: pki.leaf, intermediates: x509.NewCertPool()}
	v := NewVerifier(pki.rootPool, jwks, fixedNow)
	nodePub, _, _ := povi.GenerateNodeKey()

	// (3) report_data bound to THIS node's pubkey ⇒ key_bound=true.
	bound := signEAT(t, pki, func(c jwt.MapClaims) { c["report_data"] = nodeKeyHashHex(nodePub) })
	if res, err := v.Verify(context.Background(), bound, testNonce, nodePub); err != nil || !res.KeyBound {
		t.Fatalf("matching report_data ⇒ key_bound=true: res=%+v err=%v", res, err)
	}
	// (3) no report_data ⇒ verified, key_bound=false.
	if res, err := v.Verify(context.Background(), signEAT(t, pki, nil), testNonce, nodePub); err != nil || res.KeyBound {
		t.Fatalf("absent report_data ⇒ verified + key_bound=false: res=%+v err=%v", res, err)
	}
	// (4) RELAY RESIDUAL: report_data bound to a DIFFERENT (node B's) key, presented for this node ⇒ NOT a
	// hard fail, but key_bound=FALSE. This is the documented relay gap: it lands as key_bound=false so step
	// (c)'s mint (pays only key_bound=true) structurally cannot pay it. Closed when enclave report_data
	// binding is available.
	otherPub, _, _ := povi.GenerateNodeKey()
	relayed := signEAT(t, pki, func(c jwt.MapClaims) { c["report_data"] = nodeKeyHashHex(otherPub) })
	res, err := v.Verify(context.Background(), relayed, testNonce, nodePub)
	if err != nil {
		t.Fatalf("relayed EAT (valid NVIDIA sig) verifies as a token, got err %v", err)
	}
	if res.KeyBound {
		t.Fatal("RELAY: report_data bound to another key MUST record key_bound=false (the fence)")
	}
}

// (proof 5) JWKS hand-roll edges: unknown kid ⇒ re-fetch then REJECT; missing intermediate ⇒ chain REJECT.
func TestVerify_JWKS_Edges(t *testing.T) {
	pki := newTestPKI(t)

	// unknown kid: the source is asked, misses, the verifier re-fetches once, still misses ⇒ reject.
	wrongKid := &staticJWKS{kid: "other-kid", leaf: pki.leaf, intermediates: x509.NewCertPool()}
	v := NewVerifier(pki.rootPool, wrongKid, fixedNow)
	if _, err := v.Verify(context.Background(), signEAT(t, pki, nil), testNonce, nil); err == nil {
		t.Fatal("unknown kid must REJECT")
	}
	if wrongKid.fetches < 1 {
		t.Fatal("unknown kid must have triggered at least one JWKS lookup/refetch")
	}

	// missing intermediate: a 3-level chain root→intermediate→leaf, but the JWKS omits the intermediate ⇒
	// leaf.Verify can't build the path to root ⇒ reject.
	interKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	interTmpl := &x509.Certificate{SerialNumber: big.NewInt(10), Subject: pkix.Name{CommonName: "Inter"},
		NotBefore: certValidFrom, NotAfter: certValidUntil, IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	inter := mkCert(t, interTmpl, pki.root, &interKey.PublicKey, pki.rootKey)
	leaf2Key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf2Tmpl := &x509.Certificate{SerialNumber: big.NewInt(11), Subject: pkix.Name{CommonName: "Leaf2"},
		NotBefore: certValidFrom, NotAfter: certValidUntil, KeyUsage: x509.KeyUsageDigitalSignature}
	leaf2 := mkCert(t, leaf2Tmpl, inter, &leaf2Key.PublicKey, interKey)

	// JWKS returns leaf2 with an EMPTY intermediates pool (omits `inter`).
	brokenJWKS := &staticJWKS{kid: testKID, leaf: leaf2, intermediates: x509.NewCertPool()}
	v2 := NewVerifier(pki.rootPool, brokenJWKS, fixedNow)
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": "NRAS", "iat": time.Unix(2000, 0).Unix(), "exp": time.Unix(1<<39, 0).Unix(),
		"eat_nonce": testNonce, "x-nvidia-gpu-attestation-report-cc-mode": true,
		"x-nvidia-gpu-hwmodel": "H100", "measres": "comparison-successful",
	})
	tok.Header["kid"] = testKID
	s, _ := tok.SignedString(leaf2Key)
	if _, err := v2.Verify(context.Background(), s, testNonce, nil); err == nil {
		t.Fatal("missing intermediate must REJECT (chain can't reach the pinned root)")
	}
}
