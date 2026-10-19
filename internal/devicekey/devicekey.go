// Package devicekey checks the phone app's hardware keys (B40.4): a P-256 key in an iPhone's Secure Enclave or an
// Android phone's StrongBox or TEE, which signs only after Face ID or a fingerprint.
//
// A device key answers a challenge Lens issued by signing Message(challenge) — ECDSA P-256 over SHA-256, ASN.1 DER,
// as SecKeyCreateSignature(.ecdsaSignatureMessageX962SHA256) and Android's SHA256withECDSA produce it.
//
// Registering one proves two things. The phone holds the key: it signs Message(challenge) of the registration
// challenge. The key is the app's on real hardware: an attestation bound to Binding(challenge, publicKey) —
//   - ios: an Apple App Attest attestation object (attestKey with clientDataHash = Binding), checked against Apple's
//     App Attestation root and the app's App ID (Apple, "Validating apps that connect to your server");
//   - android: the device key's own attestation certificate chain (setAttestationChallenge(Binding)), leaf first,
//     DER certificates concatenated, checked against Google's hardware attestation roots and the KeyDescription
//     extension (Android, "Verify hardware-backed key pairs with key attestation");
//   - test: no attestation at all, accepted only when the server is started with the test-only flag.
package devicekey

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/asn1"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalid wraps every reason a device key or its signature is refused.
var ErrInvalid = errors.New("devicekey: the device key is not valid")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Platforms a device key may be registered from.
const (
	PlatformIOS     = "ios"
	PlatformAndroid = "android"
	PlatformTest    = "test"
)

// Message is what a device key signs to answer challenge.
func Message(challenge string) []byte { return []byte("talyvor-device-v1:" + challenge) }

// Binding is the 32 bytes a registration's attestation must carry: the SHA-256 of Message(challenge) followed by the
// key's SubjectPublicKeyInfo DER, so the attestation is for this key and this registration.
func Binding(challenge string, spki []byte) [32]byte {
	return sha256.Sum256(append(Message(challenge), spki...))
}

func decode64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// ParseKey reads a device's public key: SubjectPublicKeyInfo DER of a P-256 key.
func ParseKey(spki []byte) (*ecdsa.PublicKey, error) {
	k, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return nil, invalid("the public key is not SubjectPublicKeyInfo DER")
	}
	ec, ok := k.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, invalid("a device key must be P-256")
	}
	return ec, nil
}

// Verify checks signature (base64url, ASN.1 DER) is the key spki's over Message(challenge).
func Verify(spki []byte, challenge, signature string) error {
	key, err := ParseKey(spki)
	if err != nil {
		return err
	}
	sig, err := decode64(signature)
	if err != nil {
		return invalid("the signature is not base64url")
	}
	h := sha256.Sum256(Message(challenge))
	if !ecdsa.VerifyASN1(key, h[:], sig) {
		return invalid("the signature does not verify")
	}
	return nil
}

// Registration is a new device key as the phone app hands it over.
type Registration struct {
	Platform    string
	PublicKey   []byte // SubjectPublicKeyInfo DER
	Challenge   string // the registration challenge Lens issued
	Signature   string // base64url: the key's signature over Message(Challenge)
	Attestation []byte
}

//go:embed roots/apple-app-attestation-root-ca.pem
var appleRootPEM []byte

//go:embed roots/google-hardware-attestation-roots.pem
var googleRootsPEM []byte

func pool(pem []byte) *x509.CertPool {
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(pem) {
		panic("devicekey: an embedded root does not read")
	}
	return p
}

// Verifier checks registrations.
type Verifier struct {
	// AppleAppID is the iPhone app's App ID, "<team id>.<bundle id>"; "" refuses every iPhone key.
	AppleAppID string
	// AllowTest accepts the "test" platform, with no attestation. Test-only: never on a server real people use.
	AllowTest bool
	// AppleRoots and AndroidRoots replace Apple's and Google's roots, in tests; nil uses theirs.
	AppleRoots, AndroidRoots *x509.CertPool
	// Now is the time certificates are checked at; nil is time.Now.
	Now func() time.Time
}

// VerifyRegistration checks r: its platform's attestation, and that the phone holds the key.
func (v Verifier) VerifyRegistration(r Registration) error {
	if _, err := ParseKey(r.PublicKey); err != nil {
		return err
	}
	if r.Challenge == "" {
		return invalid("the registration names no challenge")
	}
	if err := Verify(r.PublicKey, r.Challenge, r.Signature); err != nil {
		return invalid("the key did not sign the registration challenge")
	}
	binding := Binding(r.Challenge, r.PublicKey)
	switch r.Platform {
	case PlatformIOS:
		return v.verifyAppAttest(r.Attestation, binding)
	case PlatformAndroid:
		return v.verifyKeyAttestation(r.Attestation, r.PublicKey, binding)
	case PlatformTest:
		if !v.AllowTest {
			return invalid("test device keys are not accepted on this server")
		}
		return nil
	}
	return invalid("platform must be ios or android")
}

func (v Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// verifyChain checks der (leaf first) chains to roots and returns the leaf.
func (v Verifier) verifyChain(certs []*x509.Certificate, roots *x509.CertPool) (*x509.Certificate, error) {
	if len(certs) == 0 {
		return nil, invalid("the attestation has no certificates")
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	if _, err := certs[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, CurrentTime: v.now(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return nil, invalid("the attestation's certificates do not chain to the platform's root: %v", err)
	}
	return certs[0], nil
}

func extension(c *x509.Certificate, oid asn1.ObjectIdentifier) []byte {
	for _, e := range c.Extensions {
		if e.Id.Equal(oid) {
			return e.Value
		}
	}
	return nil
}

var oidAppleNonce = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 8, 2}

// verifyAppAttest checks an App Attest attestation object for the app AppleAppID whose clientDataHash is binding.
func (v Verifier) verifyAppAttest(obj []byte, binding [32]byte) error {
	if v.AppleAppID == "" {
		return invalid("iPhone keys are not accepted: this server has no App ID (LENS_DEVICE_APPLE_APP_ID)")
	}
	root, err := decodeCBOR(obj)
	m, _ := root.(map[string]any)
	if err != nil || m == nil {
		return invalid("the attestation is not an App Attest attestation object")
	}
	stmt, _ := m["attStmt"].(map[string]any)
	authData, _ := m["authData"].([]byte)
	if m["fmt"] != "apple-appattest" || stmt == nil || len(authData) < 55 {
		return invalid("the attestation is not an App Attest attestation object")
	}
	x5c, _ := stmt["x5c"].([]any)
	var certs []*x509.Certificate
	for _, raw := range x5c {
		der, _ := raw.([]byte)
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return invalid("the attestation's certificate does not read")
		}
		certs = append(certs, c)
	}
	roots := v.AppleRoots
	if roots == nil {
		roots = pool(appleRootPEM)
	}
	leaf, err := v.verifyChain(certs, roots)
	if err != nil {
		return err
	}
	var ext struct {
		Nonce []byte `asn1:"tag:1,explicit"`
	}
	nonce := sha256.Sum256(append(append([]byte{}, authData...), binding[:]...))
	if _, err := asn1.Unmarshal(extension(leaf, oidAppleNonce), &ext); err != nil || !bytes.Equal(ext.Nonce, nonce[:]) {
		return invalid("the attestation is not for this key and this registration")
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return invalid("the attested key is not P-256")
	}
	point, err := pub.ECDH()
	if err != nil {
		return invalid("the attested key is not P-256")
	}
	keyID := sha256.Sum256(point.Bytes())
	rpID := sha256.Sum256([]byte(v.AppleAppID))
	n := int(authData[53])<<8 | int(authData[54])
	switch {
	case !bytes.Equal(authData[:32], rpID[:]):
		return invalid("the attestation is for another app")
	case !bytes.Equal(authData[33:37], []byte{0, 0, 0, 0}):
		return invalid("the attestation's counter is not 0")
	case string(authData[37:53]) != "appattestdevelop" && string(authData[37:53]) != "appattest\x00\x00\x00\x00\x00\x00\x00":
		return invalid("the attestation is not from App Attest")
	case len(authData) < 55+n || !bytes.Equal(authData[55:55+n], keyID[:]):
		return invalid("the attestation's credential is not its certificate's key")
	}
	return nil
}

var oidKeyDescription = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 1, 17}

// keyDescription is the start of Android's KeyDescription; the authorization lists are not read.
type keyDescription struct {
	AttestationVersion       int
	AttestationSecurityLevel asn1.Enumerated
	KeyMintVersion           int
	KeyMintSecurityLevel     asn1.Enumerated
	AttestationChallenge     []byte
	UniqueID                 []byte
	SoftwareEnforced         asn1.RawValue
	HardwareEnforced         asn1.RawValue
}

// verifyKeyAttestation checks an Android key attestation chain for spki whose challenge is binding.
// shortcut: Google's revocation list (android.googleapis.com/attestation/status) and the app's package name in
// attestationApplicationId are not checked; check both before a partner relies on device keys for real money.
func (v Verifier) verifyKeyAttestation(der, spki []byte, binding [32]byte) error {
	certs, err := x509.ParseCertificates(der)
	if err != nil {
		return invalid("the attestation is not DER certificates")
	}
	roots := v.AndroidRoots
	if roots == nil {
		roots = pool(googleRootsPEM)
	}
	leaf, err := v.verifyChain(certs, roots)
	if err != nil {
		return err
	}
	if !bytes.Equal(leaf.RawSubjectPublicKeyInfo, spki) {
		return invalid("the attestation is for another key")
	}
	var kd keyDescription
	if _, err := asn1.Unmarshal(extension(leaf, oidKeyDescription), &kd); err != nil {
		return invalid("the attestation has no key description")
	}
	if !bytes.Equal(kd.AttestationChallenge, binding[:]) {
		return invalid("the attestation is not for this registration")
	}
	// 1 is a TEE, 2 StrongBox; 0 is software, which proves nothing about the phone.
	if kd.KeyMintSecurityLevel != 1 && kd.KeyMintSecurityLevel != 2 {
		return invalid("the key is not held in the phone's secure hardware")
	}
	return nil
}

// decodeCBOR reads the CBOR an App Attest attestation object uses: maps with text keys, arrays, byte and text
// strings and integers, all of definite length.
func decodeCBOR(b []byte) (any, error) {
	v, rest, err := cborItem(b, 0)
	if err == nil && len(rest) != 0 {
		err = errors.New("trailing bytes")
	}
	return v, err
}

func cborItem(b []byte, depth int) (any, []byte, error) {
	if len(b) == 0 || depth > 8 {
		return nil, nil, errors.New("short or too deep")
	}
	major, info, b := b[0]>>5, b[0]&0x1f, b[1:]
	var n uint64
	switch {
	case info < 24:
		n = uint64(info)
	case info <= 27:
		size := 1 << (info - 24)
		if len(b) < size {
			return nil, nil, errors.New("short")
		}
		for _, c := range b[:size] {
			n = n<<8 | uint64(c)
		}
		b = b[size:]
	default:
		return nil, nil, errors.New("indefinite lengths are not read")
	}
	switch major {
	case 0, 1:
		return n, b, nil
	case 2, 3:
		if uint64(len(b)) < n {
			return nil, nil, errors.New("short")
		}
		if major == 3 {
			return string(b[:n]), b[n:], nil
		}
		return b[:n], b[n:], nil
	case 4:
		out := []any{}
		for i := uint64(0); i < n; i++ {
			v, rest, err := cborItem(b, depth+1)
			if err != nil {
				return nil, nil, err
			}
			out, b = append(out, v), rest
		}
		return out, b, nil
	case 5:
		out := map[string]any{}
		for i := uint64(0); i < n; i++ {
			k, rest, err := cborItem(b, depth+1)
			if err != nil {
				return nil, nil, err
			}
			v, rest, err := cborItem(rest, depth+1)
			if err != nil {
				return nil, nil, err
			}
			ks, ok := k.(string)
			if !ok {
				return nil, nil, errors.New("a map key is not text")
			}
			out[ks], b = v, rest
		}
		return out, b, nil
	}
	return nil, nil, errors.New("unsupported item")
}
