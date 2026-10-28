package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
)

// JWTKid is the key identifier stamped in every JWT header and in the JWKS
// response. It lets JWT verifiers select the right key from the key set
// without inspecting the payload. Bump this string whenever the signing key
// is rotated so verifiers can distinguish new tokens from old ones.
const JWTKid = "lens-1"

// ParseECPrivateKeyPEM decodes a PEM-encoded EC P-256 private key. Both the
// traditional SEC1 format ("BEGIN EC PRIVATE KEY") and the PKCS8 format
// ("BEGIN PRIVATE KEY") are accepted. Any other PEM type, or a key that does
// not use the P-256 curve, is rejected.
//
// Operators can generate a suitable key with:
//
//	openssl ecparam -name prime256v1 -genkey -noout | \
//	  openssl pkcs8 -topk8 -nocrypt -out ec-private.pem
//	export LENS_JWT_PRIVATE_KEY=$(cat ec-private.pem)
func ParseECPrivateKeyPEM(data string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(data))
	if block == nil {
		return nil, errors.New("auth: failed to decode PEM block from LENS_JWT_PRIVATE_KEY")
	}
	var (
		key *ecdsa.PrivateKey
		err error
	)
	switch block.Type {
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("auth: parse EC private key: %w", err)
		}
	case "PRIVATE KEY":
		raw, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("auth: parse PKCS8 private key: %w", err)
		}
		var ok bool
		key, ok = raw.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("auth: PKCS8 key is not an ECDSA key")
		}
	default:
		return nil, fmt.Errorf("auth: unexpected PEM type %q; want EC PRIVATE KEY or PRIVATE KEY", block.Type)
	}
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("auth: EC key must use the P-256 curve, got %s", key.Curve.Params().Name)
	}
	return key, nil
}

// GenerateECKey creates a fresh EC P-256 key pair. Used when
// LENS_JWT_PRIVATE_KEY is not configured and an ephemeral key is acceptable
// (single-instance dev). Production deployments must persist the key.
func GenerateECKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// PublicKeyToJWK serialises an EC P-256 public key as a JSON Web Key map.
// The returned map is safe to marshal directly into a JWKS response:
//
//	{"keys": [PublicKeyToJWK(key, JWTKid)]}
//
// X and Y coordinates are zero-padded to the full 32-byte P-256 field width
// before base64url encoding, as required by RFC 7518 §6.2.1.
func PublicKeyToJWK(key *ecdsa.PublicKey, kid string) map[string]any {
	// The uncompressed point 0x04 || X || Y, each already the full field width (key.X and key.Y are
	// deprecated since Go 1.26). Only an invalid key errs, and the old big.Int reads panicked on one too.
	pt, err := key.Bytes()
	if err != nil {
		panic("auth: PublicKeyToJWK: " + err.Error())
	}
	byteLen := (len(pt) - 1) / 2
	xBytes, yBytes := pt[1:1+byteLen], pt[1+byteLen:]
	return map[string]any{
		"kty": "EC",
		"crv": "P-256",
		"alg": "ES256",
		"use": "sig",
		"kid": kid,
		"x":   base64.RawURLEncoding.EncodeToString(xBytes),
		"y":   base64.RawURLEncoding.EncodeToString(yBytes),
	}
}
