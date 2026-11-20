// Package kyatap is an agent's Know Your Agent credential in the form Visa's Trusted Agent Protocol publishes (B62.6,
// docs/kya.md): for one request to a merchant, the agent recognition signature (RFC 9421, internal/agentsig) and the
// Agentic Consumer Recognition Object that carries the credential as an ID token (internal/kya), both signed with a
// key of the agent's own and bound to each other by its keyid and one nonce. Talyvor makes one for its agents and
// checks one for anyone, with no network account: the key is derived inside Lens, so none is stored or shown, and the
// ID token is bound to it (cnf), so the published keys and the credential's record are all a check needs.
//
// What needs a network is not here: Visa's own key directory and ID token, its approval of an agent, and the payment
// container's card data.
package kyatap

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/agentsig"
	"github.com/talyvor/lens/internal/kya"
)

// VerifyPath is where anyone checks one.
const VerifyPath = "/v1/kya/verify/tap"

// Alg is the algorithm both signatures are made with, as the specification writes it.
const Alg = "Ed25519"

// Credentials is what this needs of Know Your Agent.
type Credentials interface {
	IDToken(ctx context.Context, workspaceID, agentID, audience string, holder ed25519.PublicKey, expires time.Time) (string, kya.Credential, error)
	VerifyIDToken(ctx context.Context, token, audience string) (kya.Verification, *kya.Confirmation, error)
}

// ErrBadRequest: a tag, authority or path the specification does not allow.
var ErrBadRequest = errors.New("kyatap")

// Service makes and checks them.
type Service struct {
	creds  Credentials
	derive func(info string) ed25519.PrivateKey
	nonces agentsig.Nonces
	now    func() time.Time
}

// New is the service: derive is Lens's key derivation (kya.Key.Derive), nonces where a used nonce is kept.
func New(creds Credentials, derive func(info string) ed25519.PrivateKey, nonces agentsig.Nonces, now func() time.Time) *Service {
	return &Service{creds: creds, derive: derive, nonces: nonces, now: now}
}

// agentKey is agentID's key for these signatures: its own, derived for this use alone.
func (s *Service) agentKey(agentID string) ed25519.PrivateKey {
	return s.derive("talyvor kya trusted agent protocol v1 agent " + agentID)
}

// ConsumerObject is the Agentic Consumer Recognition Object, in the specification's field names.
type ConsumerObject struct {
	Nonce     string `json:"nonce"`
	IDToken   string `json:"idToken"`
	Kid       string `json:"kid"`
	Alg       string `json:"alg"`
	Signature string `json:"signature"`
}

// signed is what the object's signature covers: every field but the signature, in the order above, as compact JSON.
func (o ConsumerObject) signed() []byte {
	b, _ := json.Marshal(struct {
		Nonce   string `json:"nonce"`
		IDToken string `json:"idToken"`
		Kid     string `json:"kid"`
		Alg     string `json:"alg"`
	}{o.Nonce, o.IDToken, o.Kid, o.Alg})
	return b
}

// Request is the request to a merchant the agent is about to make, and whether it browses or pays.
type Request struct {
	Authority string `json:"authority"` // the merchant's host, like shop.example
	Path      string `json:"path"`
	Tag       string `json:"tag"` // agentsig.TAPTagBrowse or agentsig.TAPTagPay
}

// Presentation is what the agent sends with that request: the two signature headers, and the object in its body.
type Presentation struct {
	SignatureInput  string         `json:"signature_input"`
	Signature       string         `json:"signature"`
	AgenticConsumer ConsumerObject `json:"agentic_consumer"`
	ExpiresAt       time.Time      `json:"expires_at"`
	Verify          string         `json:"verify_url"`
}

func audience(authority string) string {
	return "https://" + strings.ToLower(strings.TrimSpace(authority))
}

// Present is agentID's credential for one request: valid for 8 minutes, or until the credential expires if sooner. A
// frozen or archived agent has none (*kya.StandingError).
func (s *Service) Present(ctx context.Context, workspaceID, agentID string, in Request) (Presentation, error) {
	if in.Tag != agentsig.TAPTagBrowse && in.Tag != agentsig.TAPTagPay {
		return Presentation{}, fmt.Errorf("%w: the tag is %q or %q", ErrBadRequest, agentsig.TAPTagBrowse, agentsig.TAPTagPay)
	}
	if err := agentsig.CheckTAPTarget(in.Authority, in.Path); err != nil {
		return Presentation{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	priv := s.agentKey(agentID)
	pub := priv.Public().(ed25519.PublicKey)
	kid := kya.JWKOf(pub).Kid
	nonce := make([]byte, 64)
	if _, err := rand.Read(nonce); err != nil {
		return Presentation{}, err
	}
	now := s.now().UTC().Truncate(time.Second)
	token, cred, err := s.creds.IDToken(ctx, workspaceID, agentID, audience(in.Authority), pub, now.Add(agentsig.TAPMaxLife))
	if err != nil {
		return Presentation{}, err
	}
	expires := now.Add(agentsig.TAPMaxLife)
	if cred.ExpiresAt.Before(expires) {
		expires = cred.ExpiresAt
	}
	p := Presentation{ExpiresAt: expires, Verify: VerifyPath,
		AgenticConsumer: ConsumerObject{Nonce: base64.StdEncoding.EncodeToString(nonce), IDToken: token, Kid: kid, Alg: Alg}}
	p.SignatureInput, p.Signature, err = agentsig.SignTAP(priv, in.Authority, in.Path, agentsig.TAPSignature{Tag: in.Tag,
		KeyID: kid, Nonce: p.AgenticConsumer.Nonce, Created: now, Expires: expires})
	if err != nil {
		return Presentation{}, err
	}
	p.AgenticConsumer.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, p.AgenticConsumer.signed()))
	return p, nil
}

// Check is a merchant's request to check: where the agent's request went, its two signature headers, and the object.
type Check struct {
	Authority       string         `json:"authority"`
	Path            string         `json:"path"`
	SignatureInput  string         `json:"signature_input"`
	Signature       string         `json:"signature"`
	AgenticConsumer ConsumerObject `json:"agentic_consumer"`
}

// Result is Talyvor's answer: valid, or why not, the tag, and what the credential says once its signature holds.
type Result struct {
	Valid  bool        `json:"valid"`
	Reason string      `json:"reason,omitempty"`
	Tag    string      `json:"tag,omitempty"`
	Claims *kya.Claims `json:"claims,omitempty"`
}

// Verify checks one: the ID token for this merchant, as a credential is checked; that it is bound to the agent's key;
// the object's signature with that key; the request signature as the specification checks it; and that both name
// the same key and nonce. The nonce is then used.
func (s *Service) Verify(ctx context.Context, in Check) (Result, error) {
	v, cnf, err := s.creds.VerifyIDToken(ctx, in.AgenticConsumer.IDToken, audience(in.Authority))
	if err != nil {
		return Result{}, err
	}
	if !v.Valid {
		return Result{Reason: v.Reason, Claims: v.Claims}, nil
	}
	refuse := func(reason string) (Result, error) { return Result{Reason: reason, Claims: v.Claims}, nil }
	pub := s.agentKey(v.Claims.Subject).Public().(ed25519.PublicKey)
	jwk := kya.JWKOf(pub)
	if cnf.JWK.Kid != jwk.Kid || cnf.JWK.X != jwk.X {
		return refuse("the ID token is bound to a key that is not the agent's")
	}
	obj := in.AgenticConsumer
	if obj.Kid != jwk.Kid || (obj.Alg != Alg && obj.Alg != "ed25519") {
		return refuse("the agentic consumer object is not signed with the agent's key")
	}
	raw, err := base64.StdEncoding.DecodeString(obj.Signature)
	if err != nil || !ed25519.Verify(pub, obj.signed(), raw) {
		return refuse("the agentic consumer object's signature does not verify")
	}
	keys := func(_ context.Context, keyID string) (ed25519.PublicKey, error) {
		if keyID != jwk.Kid {
			return nil, agentsig.ErrUnknownKey
		}
		return pub, nil
	}
	sig, err := agentsig.VerifyTAP(ctx, in.Authority, in.Path, in.SignatureInput, in.Signature, keys, s.nonces, s.now())
	var refused *agentsig.Error
	if errors.As(err, &refused) {
		return refuse(refused.Reason)
	} else if err != nil {
		return Result{}, err
	}
	if sig.Nonce != obj.Nonce {
		return refuse("the request signature and the agentic consumer object carry different nonces")
	}
	return Result{Valid: true, Tag: sig.Tag, Claims: v.Claims}, nil
}
