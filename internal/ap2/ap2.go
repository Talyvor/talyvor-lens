// Package ap2 keeps a workspace owner's approvals as AP2 v0.2 Payment Mandates, and signs the evidence of one agent
// approving another's request (B45.28). docs/ap2-mandates.md is the format, for whoever checks one.
//
// A mandate is an SD-JWT (RFC 9901): a JWS, ES256 as AP2 signs them, whose payload carries the SHA-256 digest of each
// disclosure in _sd, followed by the disclosures, each ending in "~"; no key binding JWT is appended. The owner
// approving one request is a closed mandate (vct mandate.payment.1); the owner letting an agent approve another's
// requests up to a limit is an open one (mandate.payment.open.1), bound to the approver's Agent Card key by cnf.
// Talyvor signs both as the operator of the Trusted Surface — the Talyvor app, where the owner signed with a passkey or
// a device key — and that signature travels inside, as the trusted_surface claim. AP2 v0.2 has no agent-to-agent
// delegation, so an agent's approval is Talyvor-signed evidence: a plain JWS of type talyvor-agent-approval+jwt.
package ap2

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"
)

// The credential types and the JWS types.
const (
	VCTClosed   = "mandate.payment.1"
	VCTOpen     = "mandate.payment.open.1"
	TypSDJWT    = "dc+sd-jwt"
	TypEvidence = "talyvor-agent-approval+jwt"
)

// ErrInvalid: the token is not one this key signed, or not as signed.
var ErrInvalid = errors.New("ap2: does not verify")

// JWK is a P-256 public key (RFC 7517).
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

// JWKS is the published key set.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// Signer signs mandates and evidence with one P-256 key, derived from a secret so nothing is stored.
type Signer struct {
	key    *ecdsa.PrivateKey
	jwk    JWK
	Issuer string
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// NewSigner is the signer whose key is derived from seed, naming issuer as iss.
func NewSigner(seed []byte, issuer string) *Signer {
	d := sha256.Sum256(append([]byte("talyvor/ap2/es256/v1:"), seed...))
	for {
		k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d[:])
		if err == nil {
			pt, _ := k.PublicKey.Bytes() // 0x04 || X || Y
			j := JWK{Kty: "EC", Crv: "P-256", X: b64(pt[1:33]), Y: b64(pt[33:]), Alg: "ES256", Use: "sig"}
			sum := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + j.X + `","y":"` + j.Y + `"}`)) // RFC 7638
			j.Kid = b64(sum[:])
			return &Signer{key: k, jwk: j, Issuer: issuer}
		}
		d = sha256.Sum256(d[:]) // a digest not below the curve's order (about 1 in 2^32) is hashed again
	}
}

// JWKS is the key set a verifier checks with.
func (s *Signer) JWKS() JWKS { return JWKS{Keys: []JWK{s.jwk}} }

// SignJWT is claims as a compact JWS of typ, signed ES256.
func (s *Signer) SignJWT(typ string, claims map[string]any) (string, error) {
	h, err := json.Marshal(map[string]string{"alg": "ES256", "typ": typ, "kid": s.jwk.Kid})
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := b64(h) + "." + b64(p)
	sum := sha256.Sum256([]byte(input))
	r, sv, err := ecdsa.Sign(rand.Reader, s.key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	sv.FillBytes(sig[32:])
	return input + "." + b64(sig), nil
}

// Issue is an SD-JWT: plain in the clear, and each claim of disclose selectively disclosable.
func (s *Signer) Issue(plain, disclose map[string]any) (string, error) {
	payload := map[string]any{}
	for k, v := range plain {
		payload[k] = v
	}
	digests, disclosures := []string{}, []string{}
	for _, name := range slices.Sorted(maps.Keys(disclose)) {
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return "", err
		}
		raw, err := json.Marshal([]any{b64(salt), name, disclose[name]})
		if err != nil {
			return "", err
		}
		d := b64(raw)
		disclosures = append(disclosures, d)
		digests = append(digests, Hash(d))
	}
	slices.Sort(digests) // their order says nothing of the claims'
	payload["_sd"], payload["_sd_alg"] = digests, "sha-256"
	jws, err := s.SignJWT(TypSDJWT, payload)
	if err != nil {
		return "", err
	}
	return strings.Join(append([]string{jws}, disclosures...), "~") + "~", nil
}

// Hash is token's SHA-256, base64url: a disclosure's digest, and how evidence names the mandate it ran under.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return b64(sum[:])
}

func publicKey(k JWK) (*ecdsa.PublicKey, error) {
	x, errX := base64.RawURLEncoding.DecodeString(k.X)
	y, errY := base64.RawURLEncoding.DecodeString(k.Y)
	if k.Kty != "EC" || k.Crv != "P-256" || errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
		return nil, fmt.Errorf("%w: not a P-256 key", ErrInvalid)
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return pub, nil
}

func decodeJSON(b64s string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(b64s)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber() // amounts stay exact
	return dec.Decode(v)
}

// VerifyJWT checks token is a JWS of typ signed by key, and is its claims.
func VerifyJWT(token, typ string, key JWK) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: not a compact JWS", ErrInvalid)
	}
	var h map[string]string
	if err := decodeJSON(parts[0], &h); err != nil || h["alg"] != "ES256" || h["typ"] != typ {
		return nil, fmt.Errorf("%w: the header is not alg ES256, typ %s", ErrInvalid, typ)
	}
	pub, err := publicKey(key)
	if err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err != nil || len(sig) != 64 || !ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return nil, fmt.Errorf("%w: the signature", ErrInvalid)
	}
	var claims map[string]any
	if err := decodeJSON(parts[1], &claims); err != nil {
		return nil, fmt.Errorf("%w: the payload", ErrInvalid)
	}
	return claims, nil
}

// Verify checks sdjwt was signed by key and each disclosure is one it signed, and is its claims with those disclosed
// in place and the digests gone. A disclosure left out is simply not there.
func Verify(sdjwt string, key JWK) (map[string]any, error) {
	if !strings.HasSuffix(sdjwt, "~") {
		return nil, fmt.Errorf("%w: an SD-JWT ends in ~ (no key binding JWT is used)", ErrInvalid)
	}
	parts := strings.Split(strings.TrimSuffix(sdjwt, "~"), "~")
	claims, err := VerifyJWT(parts[0], TypSDJWT, key)
	if err != nil {
		return nil, err
	}
	if claims["_sd_alg"] != "sha-256" {
		return nil, fmt.Errorf("%w: _sd_alg is not sha-256", ErrInvalid)
	}
	signed := map[string]bool{}
	list, _ := claims["_sd"].([]any)
	for _, d := range list {
		if s, ok := d.(string); ok {
			signed[s] = true
		}
	}
	delete(claims, "_sd")
	delete(claims, "_sd_alg")
	for _, d := range parts[1:] {
		h := Hash(d)
		if !signed[h] {
			return nil, fmt.Errorf("%w: a disclosure the issuer did not sign, or given twice", ErrInvalid)
		}
		delete(signed, h)
		var disc []any
		if err := decodeJSON(d, &disc); err != nil || len(disc) != 3 {
			return nil, fmt.Errorf("%w: a disclosure is not [salt, name, value]", ErrInvalid)
		}
		name, ok := disc[1].(string)
		if _, taken := claims[name]; !ok || taken || name == "_sd" || name == "_sd_alg" {
			return nil, fmt.Errorf("%w: a disclosure names %v, which it may not", ErrInvalid, disc[1])
		}
		claims[name] = disc[2]
	}
	return claims, nil
}
