package devicekey

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"errors"
	"math/big"
	"testing"
	"time"
)

func newKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spki, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	return k, spki
}

func sign(t *testing.T, k *ecdsa.PrivateKey, challenge string) string {
	t.Helper()
	h := sha256.Sum256(Message(challenge))
	sig, err := ecdsa.SignASN1(rand.Reader, k, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(sig)
}

// chain makes a root and a leaf for leafKey signed by it, with ext on the leaf.
func chain(t *testing.T, leafKey *ecdsa.PublicKey, ext pkix.Extension) (*x509.CertPool, []byte) {
	t.Helper()
	rootKey, _ := newKey(t)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := x509.ParseCertificate(rootDER)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "test leaf"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtraExtensions: []pkix.Extension{ext}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, root, leafKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	p := x509.NewCertPool()
	p.AddCert(root)
	return p, leafDER
}

func cborHead(major byte, n int) []byte {
	if n < 24 {
		return []byte{major<<5 | byte(n)}
	}
	return []byte{major<<5 | 25, byte(n >> 8), byte(n)}
}

func cborBytes(b []byte) []byte { return append(cborHead(2, len(b)), b...) }
func cborText(s string) []byte  { return append(cborHead(3, len(s)), s...) }

func TestRegistrationOfEachPlatform(t *testing.T) {
	devKey, spki := newKey(t)
	const challenge = "registration-challenge"
	binding := Binding(challenge, spki)
	reg := Registration{PublicKey: spki, Challenge: challenge, Signature: sign(t, devKey, challenge)}

	t.Run("ios", func(t *testing.T) {
		attestKey, _ := newKey(t)
		point, _ := attestKey.PublicKey.ECDH()
		keyID := sha256.Sum256(point.Bytes())
		rpID := sha256.Sum256([]byte("TEAM123.com.talyvor.app"))
		authData := append(append(append(rpID[:], 0x41, 0, 0, 0, 0), "appattestdevelop"...), 0, 32)
		authData = append(authData, keyID[:]...)
		nonce := sha256.Sum256(append(append([]byte{}, authData...), binding[:]...))
		val, _ := asn1.Marshal(struct {
			Nonce []byte `asn1:"tag:1,explicit"`
		}{nonce[:]})
		roots, leaf := chain(t, &attestKey.PublicKey, pkix.Extension{Id: oidAppleNonce, Value: val})
		obj := append(cborHead(5, 3), cborText("fmt")...)
		obj = append(append(obj, cborText("apple-appattest")...), cborText("attStmt")...)
		obj = append(append(append(obj, cborHead(5, 1)...), cborText("x5c")...), cborHead(4, 1)...)
		obj = append(append(append(obj, cborBytes(leaf)...), cborText("authData")...), cborBytes(authData)...)

		v := Verifier{AppleAppID: "TEAM123.com.talyvor.app", AppleRoots: roots}
		r := reg
		r.Platform, r.Attestation = PlatformIOS, obj
		if err := v.VerifyRegistration(r); err != nil {
			t.Fatalf("a genuine App Attest registration is refused: %v", err)
		}
		other := Verifier{AppleAppID: "TEAM123.com.someone.else", AppleRoots: roots}
		if err := other.VerifyRegistration(r); !errors.Is(err, ErrInvalid) {
			t.Fatalf("another app's attestation is accepted: %v", err)
		}
		r.Challenge, r.Signature = "another-challenge", sign(t, devKey, "another-challenge")
		if err := v.VerifyRegistration(r); !errors.Is(err, ErrInvalid) {
			t.Fatalf("an attestation for another registration is accepted: %v", err)
		}
	})

	t.Run("android", func(t *testing.T) {
		desc := func(level asn1.Enumerated) pkix.Extension {
			val, err := asn1.Marshal(keyDescription{AttestationVersion: 300, AttestationSecurityLevel: level, KeyMintVersion: 300,
				KeyMintSecurityLevel: level, AttestationChallenge: binding[:], UniqueID: []byte{},
				SoftwareEnforced: asn1.RawValue{FullBytes: []byte{0x30, 0}}, HardwareEnforced: asn1.RawValue{FullBytes: []byte{0x30, 0}}})
			if err != nil {
				t.Fatal(err)
			}
			return pkix.Extension{Id: oidKeyDescription, Value: val}
		}
		r := reg
		r.Platform = PlatformAndroid
		roots, leaf := chain(t, &devKey.PublicKey, desc(2))
		r.Attestation = leaf
		if err := (Verifier{AndroidRoots: roots}).VerifyRegistration(r); err != nil {
			t.Fatalf("a StrongBox key's attestation is refused: %v", err)
		}
		if err := (Verifier{}).VerifyRegistration(r); !errors.Is(err, ErrInvalid) {
			t.Fatalf("a chain to a root that is not Google's is accepted: %v", err)
		}
		roots, r.Attestation = chain(t, &devKey.PublicKey, desc(0))
		if err := (Verifier{AndroidRoots: roots}).VerifyRegistration(r); !errors.Is(err, ErrInvalid) {
			t.Fatalf("a software key is accepted: %v", err)
		}
	})

	t.Run("test", func(t *testing.T) {
		r := reg
		r.Platform = PlatformTest
		if err := (Verifier{}).VerifyRegistration(r); !errors.Is(err, ErrInvalid) {
			t.Fatalf("a fake attestation is accepted without the test-only flag: %v", err)
		}
		if err := (Verifier{AllowTest: true}).VerifyRegistration(r); err != nil {
			t.Fatalf("a test key is refused with the flag: %v", err)
		}
		r.Signature = sign(t, devKey, "not-the-challenge")
		if err := (Verifier{AllowTest: true}).VerifyRegistration(r); !errors.Is(err, ErrInvalid) {
			t.Fatalf("a key that did not sign the challenge is accepted: %v", err)
		}
	})
}
