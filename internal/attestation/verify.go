package attestation

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Result is the outcome of a verified EAT. GPUClass is the cryptographically-verified hardware class the
// mint (step c) reads; KeyBound is the relay fence (true only when the EAT's report_data binds the node's
// registered key). Zero Result + a non-nil error ⇒ REJECT (record status='failed', never a partial pass).
type Result struct {
	GPUClass  string
	CCMode    bool
	KeyBound  bool
	EATDigest string // sha256 hex of the raw EAT (audit)
}

// Verifier verifies an NVIDIA NRAS EAT: JWT signature (golang-jwt) with the signing key resolved from the
// JWKS and its x5c chain validated to the PINNED root CA (stdlib crypto/x509), then the EAT claims. roots is
// NVIDIA's production root in prod, a self-generated CA in CI (the LENS_TEST_NVIDIA_EAT test uses the real
// root).
type Verifier struct {
	roots *x509.CertPool
	jwks  JWKSSource
	now   func() time.Time
}

// NewVerifier pins the root CA + a JWKS source. now is injectable for deterministic exp/iat checks.
func NewVerifier(roots *x509.CertPool, jwks JWKSSource, now func() time.Time) *Verifier {
	if now == nil {
		now = time.Now
	}
	return &Verifier{roots: roots, jwks: jwks, now: now}
}

// eatClaims mirrors the NVIDIA NRAS EAT shape. golang-jwt validates exp/iat automatically via
// RegisteredClaims; the NVIDIA-specific claims are checked in Verify.
type eatClaims struct {
	jwt.RegisteredClaims
	EatNonce   int64  `json:"eat_nonce"`
	CCMode     *bool  `json:"x-nvidia-gpu-attestation-report-cc-mode"`
	HWModel    string `json:"x-nvidia-gpu-hwmodel"`
	MeasRes    string `json:"measres"`
	ReportData string `json:"report_data,omitempty"`
}

// Verify runs the full sequence and returns the verified Result or a rejection error. nodePub (optional) is
// the node's registered ed25519 pubkey used ONLY for the report_data key-binding check — a nil/absent
// binding yields KeyBound=false, never an error (the relay fence lives in the mint, step c).
func (v *Verifier) Verify(ctx context.Context, rawEAT string, wantNonce int64, nodePub ed25519.PublicKey) (Result, error) {
	// (ii) signature verify — the Keyfunc resolves the kid → leaf cert, validates the x5c chain to the pinned
	// root, and returns the leaf public key for golang-jwt to check the signature against. golang-jwt ALSO
	// validates exp/iat here (WithTimeFunc), so an expired/not-yet-valid EAT rejects at Parse.
	keyfunc := func(tok *jwt.Token) (interface{}, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodECDSA); !ok {
			if _, ok := tok.Method.(*jwt.SigningMethodRSA); !ok {
				return nil, fmt.Errorf("attestation: unexpected alg %v", tok.Header["alg"])
			}
		}
		kid, _ := tok.Header["kid"].(string)
		leaf, intermediates, err := v.jwks.CertForKID(ctx, kid)
		if err != nil {
			return nil, err
		}
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots: v.roots, Intermediates: intermediates, CurrentTime: v.now(),
		}); err != nil {
			return nil, fmt.Errorf("attestation: x5c chain not to pinned root: %w", err)
		}
		return leaf.PublicKey, nil
	}

	var claims eatClaims
	tok, err := jwt.NewParser(jwt.WithTimeFunc(v.now), jwt.WithExpirationRequired()).ParseWithClaims(rawEAT, &claims, keyfunc)
	if err != nil {
		return Result{}, fmt.Errorf("attestation: EAT verify: %w", err)
	}
	if !tok.Valid {
		return Result{}, errors.New("attestation: EAT invalid")
	}

	// (iii) claim checks — each an independent REJECT.
	if claims.Issuer != "NRAS" {
		return Result{}, fmt.Errorf("attestation: iss %q != NRAS", claims.Issuer)
	}
	if claims.CCMode == nil || !*claims.CCMode {
		return Result{}, errors.New("attestation: cc-mode absent or false")
	}
	if claims.MeasRes == "" {
		return Result{}, errors.New("attestation: measurements claim absent")
	}
	if claims.EatNonce != wantNonce {
		return Result{}, fmt.Errorf("attestation: eat_nonce %d != issued %d", claims.EatNonce, wantNonce)
	}

	// (iv) key-binding (the relay fence) — true ONLY when report_data == H(registered node pubkey). Absent or
	// mismatched ⇒ false, NEVER an error (a relayed-but-valid EAT lands as key_bound=false; step c won't pay).
	keyBound := false
	if claims.ReportData != "" && len(nodePub) == ed25519.PublicKeySize {
		want := sha256.Sum256(nodePub)
		keyBound = (claims.ReportData == hex.EncodeToString(want[:]))
	}

	digest := sha256.Sum256([]byte(rawEAT))
	return Result{
		GPUClass:  claims.HWModel,
		CCMode:    true,
		KeyBound:  keyBound,
		EATDigest: hex.EncodeToString(digest[:]),
	}, nil
}
