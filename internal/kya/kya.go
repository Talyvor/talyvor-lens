// Package kya is Know Your Agent (B30.5): every agent can show a credential — a JWT signed with Ed25519 — that any
// platform can check against Talyvor's published keys, without an account. It says who the agent is, who answers for
// it and how far they are verified, what it may do with live money and what with test money only, and a summary of
// its limits. docs/kya.md is the format, for the platforms that check one.
//
// internal/economy/agent_kya.go reads what a credential says and keeps the record of each one issued; freezing or
// archiving an agent, or changing its rules, revokes its credential there, in the same transaction. This package
// signs, publishes the keys and the revocation list, and verifies.
package kya

import (
	"context"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/talyvor/lens/internal/economy"
)

// Issuer is every credential's iss.
const Issuer = "talyvor"

// TokenType is every credential's typ header.
const TokenType = "kya+jwt"

// The published paths.
const (
	JWKSPath        = "/.well-known/talyvor-kya/jwks.json"
	RevocationsPath = "/.well-known/talyvor-kya/revoked.json"
	VerifyPath      = "/v1/kya/verify"
)

// Store is what Know Your Agent needs of the economy store.
type Store interface {
	AgentKYAFacts(ctx context.Context, workspaceID, agentID string) (economy.AgentKYAFacts, error)
	LatestKYACredential(ctx context.Context, workspaceID, agentID string, at time.Time) (economy.KYACredentialRecord, bool, error)
	KYACredential(ctx context.Context, id string) (economy.KYACredentialRecord, error)
	IssueKYACredential(ctx context.Context, workspaceID, agentID string,
		sign func(economy.AgentKYAFacts) (economy.KYACredentialRecord, error)) (economy.KYACredentialRecord, economy.AgentKYAFacts, error)
	RevokeKYACredential(ctx context.Context, id, reason string) error
	KYARevocations(ctx context.Context, at time.Time) ([]economy.KYARevocation, error)
	KYAKeys(ctx context.Context, at time.Time) ([]economy.KYAKey, error)
}

// Claims is what a credential says: the registered claims (iss talyvor, sub the agent's id, jti the credential's id,
// iat, nbf, exp) and the agent, its owner, its capabilities and its limits.
type Claims struct {
	jwt.RegisteredClaims
	economy.AgentKYAFacts
}

// Key is the Ed25519 key credentials are signed with. Its kid is its RFC 7638 thumbprint.
type Key struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	kid  string
}

// ParseKey reads LENS_KYA_SIGNING_KEY: base64 of an Ed25519 32-byte seed or 64-byte private key. An empty one makes a
// key for this process only (ephemeral true): credentials it signs stay verifiable after a restart, because the JWKS
// publishes every key a credential not yet expired was signed with, but a fixed key is set in production.
func ParseKey(b64 string) (k Key, ephemeral bool, err error) {
	if b64 == "" {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return k, true, err
		}
		return keyOf(priv), true, nil
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	switch {
	case err != nil:
		return k, false, fmt.Errorf("kya: the signing key must be base64: %w", err)
	case len(raw) == ed25519.SeedSize:
		return keyOf(ed25519.NewKeyFromSeed(raw)), false, nil
	case len(raw) == ed25519.PrivateKeySize:
		return keyOf(ed25519.PrivateKey(raw)), false, nil
	}
	return k, false, errors.New("kya: the signing key must be an Ed25519 32-byte seed or 64-byte private key")
}

func keyOf(priv ed25519.PrivateKey) Key {
	pub := priv.Public().(ed25519.PublicKey)
	return Key{priv: priv, pub: pub, kid: thumbprint(pub)}
}

// Kid is the key's id: its RFC 7638 thumbprint.
func (k Key) Kid() string { return k.kid }

// Derive is an Ed25519 key of its own for info, derived from this one with HKDF-SHA256 (RFC 5869): B45.23 signs each
// agent's Agent Card with one, so no private key is stored and none leaves the process.
func (k Key) Derive(info string) ed25519.PrivateKey {
	seed, err := hkdf.Key(sha256.New, k.priv.Seed(), nil, info, ed25519.SeedSize)
	if err != nil {
		panic(err) // only for a length HKDF-SHA256 cannot give, and 32 bytes it always can
	}
	return ed25519.NewKeyFromSeed(seed)
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// thumbprint is pub's RFC 7638 JWK thumbprint: SHA-256 over its required members in lexicographic order.
func thumbprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + b64url(pub) + `"}`))
	return b64url(sum[:])
}

// JWK is one published key (RFC 8037: an OKP key on Ed25519).
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

func jwkOf(kid string, pub ed25519.PublicKey) JWK {
	return JWK{Kty: "OKP", Crv: "Ed25519", X: b64url(pub), Kid: kid, Alg: "EdDSA", Use: "sig"}
}

// JWKOf is pub as a published key, its kid its RFC 7638 thumbprint.
func JWKOf(pub ed25519.PublicKey) JWK { return jwkOf(thumbprint(pub), pub) }

// JWKS is the published key set.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// Credential is an agent's credential: the signed token and what it says.
type Credential struct {
	ID        string    `json:"id"`
	Token     string    `json:"credential"`
	Claims    Claims    `json:"claims"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
	JWKS      string    `json:"jwks_url"`
	Verify    string    `json:"verify_url"`
}

// StandingError: the agent is frozen or archived, so it has no credential.
type StandingError struct{ Standing string }

func (e *StandingError) Error() string {
	if e.Standing == economy.KYARevokedArchived {
		return "the agent is archived, so it has no Know Your Agent credential"
	}
	return "the agent is frozen, so its Know Your Agent credential is revoked until its owner resumes it"
}

// Service issues, publishes and verifies credentials.
type Service struct {
	store Store
	key   Key
	ttl   time.Duration
	now   func() time.Time
}

// New is the service signing with key, each credential valid for ttl.
func New(store Store, key Key, ttl time.Duration) *Service {
	return &Service{store: store, key: key, ttl: ttl, now: time.Now}
}

// digest is the SHA-256 of what facts say, which a credential's record keeps: a credential whose agent's facts no
// longer match it is superseded.
func digest(f economy.AgentKYAFacts) (string, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Current is agentID's credential: the one it holds while it still says what is true, or a new one, which supersedes
// it. A frozen or archived agent has none (*StandingError).
func (s *Service) Current(ctx context.Context, workspaceID, agentID string) (Credential, error) {
	now := s.now().UTC().Truncate(time.Second)
	facts, err := s.store.AgentKYAFacts(ctx, workspaceID, agentID)
	if err != nil {
		return Credential{}, err
	}
	held, ok, err := s.store.LatestKYACredential(ctx, workspaceID, agentID, now)
	if err != nil {
		return Credential{}, err
	}
	if facts.Standing != "" {
		if ok { // freezing and archiving revoke it as they happen; this is the same, should one have been missed
			if err := s.store.RevokeKYACredential(ctx, held.ID, facts.Standing); err != nil {
				return Credential{}, err
			}
		}
		return Credential{}, &StandingError{facts.Standing}
	}
	sum, err := digest(facts)
	if err != nil {
		return Credential{}, err
	}
	if ok && held.ClaimsDigest == sum {
		var claims Claims
		if _, _, err := jwt.NewParser().ParseUnverified(held.Token, &claims); err != nil {
			return Credential{}, fmt.Errorf("kya: read a credential: %w", err)
		}
		return s.credentialOf(held, claims), nil
	}
	// Issued under the agent's lock, from what is true of it then, which may have changed since it was read above.
	var claims Claims
	rec, facts, err := s.store.IssueKYACredential(ctx, workspaceID, agentID, func(f economy.AgentKYAFacts) (economy.KYACredentialRecord, error) {
		sum, err := digest(f)
		if err != nil {
			return economy.KYACredentialRecord{}, err
		}
		id := "kya_" + uuid.NewString()
		claims = Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: Issuer, Subject: agentID, ID: id,
			IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(s.ttl))},
			AgentKYAFacts: f}
		tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
		tok.Header["kid"], tok.Header["typ"] = s.key.kid, TokenType
		signed, err := tok.SignedString(s.key.priv)
		if err != nil {
			return economy.KYACredentialRecord{}, fmt.Errorf("kya: sign a credential: %w", err)
		}
		return economy.KYACredentialRecord{ID: id, WorkspaceID: workspaceID, AgentID: agentID, Kid: s.key.kid, PublicKey: b64url(s.key.pub),
			ClaimsDigest: sum, Token: signed, IssuedAt: now, ExpiresAt: now.Add(s.ttl)}, nil
	})
	if err != nil {
		return Credential{}, err
	}
	if facts.Standing != "" {
		return Credential{}, &StandingError{facts.Standing}
	}
	return s.credentialOf(rec, claims), nil
}

func (s *Service) credentialOf(rec economy.KYACredentialRecord, claims Claims) Credential {
	return Credential{ID: rec.ID, Token: rec.Token, Claims: claims, IssuedAt: rec.IssuedAt, ExpiresAt: rec.ExpiresAt,
		JWKS: JWKSPath, Verify: VerifyPath}
}

// keys is every key a credential not yet expired was signed with, and this one, by kid.
func (s *Service) keys(ctx context.Context) (map[string]ed25519.PublicKey, []string, error) {
	out, order := map[string]ed25519.PublicKey{s.key.kid: s.key.pub}, []string{s.key.kid}
	stored, err := s.store.KYAKeys(ctx, s.now())
	if err != nil {
		return nil, nil, err
	}
	for _, k := range stored {
		if _, seen := out[k.Kid]; seen {
			continue
		}
		pub, err := base64.RawURLEncoding.DecodeString(k.PublicKey)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return nil, nil, fmt.Errorf("kya: key %s is not an Ed25519 public key", k.Kid)
		}
		out[k.Kid], order = pub, append(order, k.Kid)
	}
	return out, order, nil
}

// JWKS is the published key set: the key signing now first, then every other key a credential not yet expired was
// signed with.
func (s *Service) JWKS(ctx context.Context) (JWKS, error) {
	keys, order, err := s.keys(ctx)
	if err != nil {
		return JWKS{}, err
	}
	set := JWKS{Keys: make([]JWK, 0, len(order))}
	for _, kid := range order {
		set.Keys = append(set.Keys, jwkOf(kid, keys[kid]))
	}
	return set, nil
}

// Revocations is the published revocation list: every credential revoked that has not yet expired.
type Revocations struct {
	Issuer  string                  `json:"issuer"`
	At      time.Time               `json:"generated_at"`
	Revoked []economy.KYARevocation `json:"revoked"`
}

// Revocations is the revocation list now.
func (s *Service) Revocations(ctx context.Context) (Revocations, error) {
	at := s.now().UTC()
	list, err := s.store.KYARevocations(ctx, at)
	if err != nil {
		return Revocations{}, err
	}
	if list == nil {
		list = []economy.KYARevocation{}
	}
	return Revocations{Issuer: Issuer, At: at, Revoked: list}, nil
}

// Verification is Talyvor's answer on a credential: valid, or why not, and what it says once its signature holds.
type Verification struct {
	Valid  bool    `json:"valid"`
	Reason string  `json:"reason,omitempty"`
	Claims *Claims `json:"claims,omitempty"`
}

// Verify checks a credential: its signature against the published keys, its issuer and times, that Talyvor issued it
// and has not revoked it, and that it still says what is true of its agent — one that no longer does is revoked now.
func (s *Service) Verify(ctx context.Context, token string) (Verification, error) {
	keys, _, err := s.keys(ctx)
	if err != nil {
		return Verification{}, err
	}
	var claims Claims
	_, err = jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if pub, ok := keys[kid]; ok {
			return pub, nil
		}
		return nil, errors.New("it is signed with a key Talyvor does not publish")
	}, jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}), jwt.WithIssuer(Issuer), jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.now))
	if err != nil {
		return Verification{Reason: "not a valid credential: " + err.Error()}, nil
	}
	rec, err := s.store.KYACredential(ctx, claims.ID)
	if errors.Is(err, economy.ErrKYACredentialNotFound) || (err == nil && rec.AgentID != claims.Subject) {
		return Verification{Reason: "Talyvor has no record of issuing this credential"}, nil
	}
	if err != nil {
		return Verification{}, err
	}
	if rec.RevokedAt != nil {
		return Verification{Reason: "revoked: " + rec.RevokedReason, Claims: &claims}, nil
	}
	facts, err := s.store.AgentKYAFacts(ctx, rec.WorkspaceID, rec.AgentID)
	if errors.Is(err, economy.ErrAgentNotFound) {
		return Verification{Reason: "the agent no longer exists", Claims: &claims}, nil
	}
	if err != nil {
		return Verification{}, err
	}
	reason := facts.Standing
	if reason == "" {
		sum, err := digest(facts)
		if err != nil {
			return Verification{}, err
		}
		if sum != rec.ClaimsDigest {
			reason = economy.KYARevokedSuperseded
		}
	}
	if reason != "" {
		if err := s.store.RevokeKYACredential(ctx, rec.ID, reason); err != nil {
			return Verification{}, err
		}
		return Verification{Reason: "revoked: " + reason, Claims: &claims}, nil
	}
	return Verification{Valid: true, Claims: &claims}, nil
}
