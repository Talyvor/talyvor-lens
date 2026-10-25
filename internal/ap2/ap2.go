package ap2

import "errors"

const (
	VCTClosed   = "mandate.payment.1"
	VCTOpen     = "mandate.payment.open.1"
	TypSDJWT    = "dc+sd-jwt"
	TypEvidence = "talyvor-agent-approval+jwt"
)

var ErrInvalid = errors.New("ap2: does not verify")

type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

type JWKS struct {
	Keys []JWK `json:"keys"`
}

type Signer struct{ Issuer string }

func NewSigner(seed []byte, issuer string) *Signer                     { return &Signer{Issuer: issuer} }
func (s *Signer) JWKS() JWKS                                           { return JWKS{} }
func (s *Signer) Issue(plain, disclose map[string]any) (string, error) { return "", ErrInvalid }
func (s *Signer) SignJWT(typ string, claims map[string]any) (string, error) {
	return "", ErrInvalid
}
func Verify(sdjwt string, key JWK) (map[string]any, error)         { return nil, ErrInvalid }
func VerifyJWT(token, typ string, key JWK) (map[string]any, error) { return nil, ErrInvalid }
func Hash(token string) string                                     { return "" }
