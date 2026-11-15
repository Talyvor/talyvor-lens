package kya

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// idtoken.go — B62.6: the credential in the form Visa's Trusted Agent Protocol publishes for the ID token an agent
// carries in its Agentic Consumer Recognition Object: a JWT of typ "JWT+ext.id_token" with iss, sub, aud, iat and
// exp. It says what the agent's credential says, for one merchant (aud), until its request signature expires, and is
// bound by cnf (RFC 7800) to the key that signature is made with, so whoever copies it cannot sign for it. It is the
// credential's jti, so revoking the credential revokes it.

// IDTokenType is the ID token's typ header.
const IDTokenType = "JWT+ext.id_token"

// Confirmation is the key an ID token is bound to (RFC 7800 cnf).
type Confirmation struct {
	JWK JWK `json:"jwk"`
}

// IDClaims is what an ID token says: the credential's claims, its audience, and the key it is bound to.
type IDClaims struct {
	Claims
	Cnf Confirmation `json:"cnf"`
}

// IDToken is agentID's credential as an ID token for audience, bound to holder, until expires or the credential's own
// expiry, whichever comes first.
func (s *Service) IDToken(ctx context.Context, workspaceID, agentID, audience string, holder ed25519.PublicKey,
	expires time.Time) (string, Credential, error) {
	c, err := s.Current(ctx, workspaceID, agentID)
	if err != nil {
		return "", c, err
	}
	now := s.now().UTC().Truncate(time.Second)
	if expires.After(c.ExpiresAt) {
		expires = c.ExpiresAt
	}
	claims := IDClaims{Claims: c.Claims, Cnf: Confirmation{JWK: JWKOf(holder)}}
	claims.Audience = jwt.ClaimStrings{audience}
	claims.IssuedAt, claims.NotBefore, claims.ExpiresAt = jwt.NewNumericDate(now), jwt.NewNumericDate(now), jwt.NewNumericDate(expires)
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"], tok.Header["typ"] = s.key.kid, IDTokenType
	signed, err := tok.SignedString(s.key.priv)
	if err != nil {
		return "", c, fmt.Errorf("kya: sign an ID token: %w", err)
	}
	return signed, c, nil
}

// VerifyIDToken checks an ID token for audience as Verify checks a credential, and gives the key it is bound to.
func (s *Service) VerifyIDToken(ctx context.Context, token, audience string) (Verification, *Confirmation, error) {
	keys, _, err := s.keys(ctx)
	if err != nil {
		return Verification{}, nil, err
	}
	var claims IDClaims
	_, err = jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		if typ, _ := t.Header["typ"].(string); typ != IDTokenType {
			return nil, errors.New("it is not an ID token")
		}
		kid, _ := t.Header["kid"].(string)
		if pub, ok := keys[kid]; ok {
			return pub, nil
		}
		return nil, errors.New("it is signed with a key Talyvor does not publish")
	}, jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}), jwt.WithIssuer(Issuer), jwt.WithAudience(audience),
		jwt.WithExpirationRequired(), jwt.WithTimeFunc(s.now))
	if err != nil {
		return Verification{Reason: "not a valid ID token: " + err.Error()}, nil, nil
	}
	v, err := s.standing(ctx, &claims.Claims)
	return v, &claims.Cnf, err
}
