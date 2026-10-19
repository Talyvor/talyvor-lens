// Package passkey verifies WebAuthn ceremonies for ES256 (P-256) passkeys: a registration with "none"
// attestation, and an assertion (W3C WebAuthn Level 2, §7.1 and §7.2).
//
// A registration is taken as the browser's AuthenticatorAttestationResponse gives it through
// getPublicKey() and getAuthenticatorData(): the credential's public key as SubjectPublicKeyInfo DER, so
// nothing here reads CBOR. With "none" attestation nothing about the authenticator is proven; what is
// checked is that the ceremony answers this server's challenge, for this relying party, from an allowed
// origin, and that the key is a P-256 key for the credential id the authenticator names.
package passkey

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrInvalid wraps every reason a ceremony is refused.
var ErrInvalid = errors.New("passkey: the passkey ceremony is not valid")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

const (
	flagUserPresent  = 0x01
	flagUserVerified = 0x04
	flagAttested     = 0x40
)

// RelyingParty is who the passkeys are for: the RP ID the browser scopes them to, and the origins a
// ceremony may come from.
type RelyingParty struct {
	ID      string
	Origins []string
}

// Decode64 reads base64url with or without padding.
func Decode64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

type clientData struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	Origin    string `json:"origin"`
}

// checkClientData checks the ceremony's type and origin and returns the challenge it answers.
func (rp RelyingParty) checkClientData(raw []byte, wantType string) (string, error) {
	var cd clientData
	if err := json.Unmarshal(raw, &cd); err != nil {
		return "", invalid("clientDataJSON is not JSON")
	}
	if cd.Type != wantType {
		return "", invalid("clientDataJSON.type is %q, want %q", cd.Type, wantType)
	}
	if !slices.Contains(rp.Origins, cd.Origin) {
		return "", invalid("the origin %q is not allowed", cd.Origin)
	}
	return strings.TrimRight(cd.Challenge, "="), nil
}

// authData is the part of authenticatorData the checks read.
type authData struct {
	flags     byte
	signCount uint32
	credID    []byte // present when the attested-credential flag is set
}

func (rp RelyingParty) parseAuthData(raw []byte) (authData, error) {
	var a authData
	if len(raw) < 37 {
		return a, invalid("authenticatorData is too short")
	}
	want := sha256.Sum256([]byte(rp.ID))
	if !bytes.Equal(raw[:32], want[:]) {
		return a, invalid("authenticatorData is for another relying party")
	}
	a.flags, a.signCount = raw[32], binary.BigEndian.Uint32(raw[33:37])
	if a.flags&flagUserPresent == 0 {
		return a, invalid("the user was not present")
	}
	if a.flags&flagAttested != 0 {
		if len(raw) < 55 {
			return a, invalid("attested credential data is too short")
		}
		n := int(binary.BigEndian.Uint16(raw[53:55]))
		if len(raw) < 55+n {
			return a, invalid("attested credential data is too short")
		}
		a.credID = raw[55 : 55+n]
	}
	return a, nil
}

// Registration is a new passkey as the browser hands it over.
type Registration struct {
	CredentialID      string // base64url
	PublicKey         []byte // SubjectPublicKeyInfo DER
	ClientDataJSON    []byte
	AuthenticatorData []byte
}

// VerifyRegistration checks a registration answers challenge and returns its sign count.
func (rp RelyingParty) VerifyRegistration(r Registration, challenge string) (uint32, error) {
	got, err := rp.checkClientData(r.ClientDataJSON, "webauthn.create")
	if err != nil {
		return 0, err
	}
	if got != challenge {
		return 0, invalid("the registration answers another challenge")
	}
	a, err := rp.parseAuthData(r.AuthenticatorData)
	if err != nil {
		return 0, err
	}
	id, err := Decode64(r.CredentialID)
	if err != nil || a.credID == nil || !bytes.Equal(id, a.credID) {
		return 0, invalid("the authenticator data does not name this credential")
	}
	if _, err := parseKey(r.PublicKey); err != nil {
		return 0, err
	}
	return a.signCount, nil
}

func parseKey(spki []byte) (*ecdsa.PublicKey, error) {
	k, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return nil, invalid("the public key is not SubjectPublicKeyInfo DER")
	}
	ec, ok := k.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, invalid("the passkey must be ES256 (P-256)")
	}
	return ec, nil
}

// Assertion is a passkey's answer to a challenge, as the browser hands it over.
type Assertion struct {
	CredentialID      string `json:"credential_id"`
	ClientDataJSON    string `json:"client_data_json"`   // base64url
	AuthenticatorData string `json:"authenticator_data"` // base64url
	Signature         string `json:"signature"`          // base64url, ASN.1 DER ECDSA
	// B40.4: a phone app's device key (internal/devicekey) signs instead when DeviceID is set: Signature is then over
	// devicekey.Message(Challenge), and CredentialID and the WebAuthn fields are empty.
	DeviceID  string `json:"device_id,omitempty"`
	Challenge string `json:"challenge,omitempty"`
}

// VerifyAssertion checks an assertion answers challenge, signed by the key spki with the user verified
// (Face ID, a fingerprint or the device's PIN), and returns the new sign count. A counter that did not
// move forward (when either is non-zero) is refused: the passkey may have been cloned.
func (rp RelyingParty) VerifyAssertion(a Assertion, challenge string, spki []byte, storedCount uint32) (uint32, error) {
	cdj, err1 := Decode64(a.ClientDataJSON)
	ad, err2 := Decode64(a.AuthenticatorData)
	sig, err3 := Decode64(a.Signature)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, invalid("the assertion is not base64url")
	}
	got, err := rp.checkClientData(cdj, "webauthn.get")
	if err != nil {
		return 0, err
	}
	if got != challenge {
		return 0, invalid("the assertion answers another challenge")
	}
	auth, err := rp.parseAuthData(ad)
	if err != nil {
		return 0, err
	}
	if auth.flags&flagUserVerified == 0 {
		return 0, invalid("the user was not verified")
	}
	key, err := parseKey(spki)
	if err != nil {
		return 0, err
	}
	h := sha256.Sum256(cdj)
	signed := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	if !ecdsa.VerifyASN1(key, signed[:], sig) {
		return 0, invalid("the signature does not verify")
	}
	if (auth.signCount != 0 || storedCount != 0) && auth.signCount <= storedCount {
		return 0, invalid("the passkey's counter went backwards")
	}
	return auth.signCount, nil
}

// ChallengeOf reads the challenge a ceremony's clientDataJSON answers, before anything is verified, so
// the server can find the challenge it issued; "" when there is none.
func ChallengeOf(clientDataJSON []byte) string {
	var cd clientData
	if json.Unmarshal(clientDataJSON, &cd) != nil {
		return ""
	}
	return strings.TrimRight(cd.Challenge, "=")
}
