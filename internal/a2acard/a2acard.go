// Package a2acard gives every Talyvor agent an identity card (B45.23): an A2A v1.0 Agent Card, always signed, that
// names the verified person or company who answers for the agent — an agent is never the legal principal. Talyvor's
// own card is at /.well-known/agent-card.json. docs/agent-card.md is the format, for whoever checks one.
//
// A card is signed as A2A §8.4 says: a JWS (RFC 7515, EdDSA) over the card without its signatures, canonicalised by
// RFC 8785 (internal/jcs), in its signatures array. Each agent signs with a key of its own, derived inside Lens from
// Lens's signing secret, the agent's id and the key's generation, so no private key is stored or shown; only public
// keys are kept (agent_card_keys), each published at its agent's jwks.json until it is rotated, which revokes it.
// A signature proves Talyvor signed the card, not which company stands behind the agent: the card says that, and
// how far Talyvor has verified it.
package a2acard

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/jcs"
	"github.com/talyvor/lens/internal/kya"
)

// ProtocolVersion is the A2A version the cards follow.
const ProtocolVersion = "1.0"

// PrincipalExtension names the card extension that says who answers for an agent.
const PrincipalExtension = "https://talyvor.com/a2a/extensions/principal/v1"

// The published paths.
const (
	TalyvorJWKSPath = "/.well-known/talyvor-agent-cards/jwks.json"
	VerifyPath      = "/v1/a2a/verify"
)

// CardPath is agentID's card; JWKSPath the key it is signed with.
func CardPath(agentID string) string { return "/v1/a2a/agents/" + agentID + "/agent-card.json" }
func JWKSPath(agentID string) string { return "/v1/a2a/agents/" + agentID + "/jwks.json" }

// ErrArchived: the agent is archived, so it has no card.
var ErrArchived = errors.New("the agent is archived, so it has no Agent Card")

// RevokedRotated is why a rotated key is revoked; RevokedSecretChanged why one Lens can no longer sign with is.
const (
	RevokedRotated       = "rotated"
	RevokedSecretChanged = "Lens's signing secret changed"
)

// Store is what cards need of the economy store.
type Store interface {
	AgentCardFacts(ctx context.Context, agentID string) (economy.AgentCardFacts, error)
	AgentCardKeyInForce(ctx context.Context, agentID string) (economy.AgentCardKey, bool, int, error)
	AddAgentCardKey(ctx context.Context, k economy.AgentCardKey) (bool, error)
	RevokeAgentCardKey(ctx context.Context, agentID, reason string) error
	AgentCardKey(ctx context.Context, kid string) (economy.AgentCardKey, error)
}

// Card is an A2A v1.0 Agent Card, in its JSON form (camelCase, A2A §5.5).
type Card struct {
	Name                string       `json:"name"`
	Description         string       `json:"description"`
	SupportedInterfaces []Interface  `json:"supportedInterfaces"`
	Provider            Provider     `json:"provider"`
	Version             string       `json:"version"`
	DocumentationURL    string       `json:"documentationUrl,omitempty"`
	Capabilities        Capabilities `json:"capabilities"`
	DefaultInputModes   []string     `json:"defaultInputModes"`
	DefaultOutputModes  []string     `json:"defaultOutputModes"`
	Skills              []Skill      `json:"skills"`
	Signatures          []Signature  `json:"signatures,omitempty"`
}

// Interface is where and how the agent is reached over A2A.
type Interface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
	ProtocolVersion string `json:"protocolVersion"`
}

// Provider is who answers for the agent.
type Provider struct {
	Organization string `json:"organization"`
	URL          string `json:"url"`
}

// Capabilities are the A2A capabilities; Talyvor's cards declare only the principal extension.
type Capabilities struct {
	Extensions []Extension `json:"extensions,omitempty"`
}

// Extension is one A2A protocol extension the card declares.
type Extension struct {
	URI         string     `json:"uri"`
	Description string     `json:"description,omitempty"`
	Params      *Principal `json:"params,omitempty"`
}

// Principal is who answers for an agent: the name their verification confirmed and the levels it reaches (docs/kya.md
// has the levels). Name is empty until a check confirms one.
type Principal struct {
	AgentID   string                    `json:"agentId"`
	Name      string                    `json:"name,omitempty"`
	Level     economy.VerificationLevel `json:"level"`
	LiveLevel economy.VerificationLevel `json:"liveLevel"`
}

// Skill is one thing the agent does.
type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

// Signature is one JWS over the card (A2A AgentCardSignature): its protected header and signature, base64url.
type Signature struct {
	Protected string `json:"protected"`
	Signature string `json:"signature"`
}

// Service signs, publishes and verifies cards.
type Service struct {
	store  Store
	derive func(info string) ed25519.PrivateKey
	base   string
	own    ed25519.PrivateKey
	ownKid string
}

// New is the service deriving its keys with derive (kya.Key.Derive) and naming its URLs under baseURL.
func New(store Store, derive func(info string) ed25519.PrivateKey, baseURL string) *Service {
	own := derive("talyvor a2a agent card v1 talyvor")
	return &Service{store: store, derive: derive, base: strings.TrimRight(baseURL, "/"), own: own,
		ownKid: kya.JWKOf(own.Public().(ed25519.PublicKey)).Kid}
}

func agentKeyInfo(agentID string, generation int) string {
	return "talyvor a2a agent card v1 agent " + agentID + " " + strconv.Itoa(generation)
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// keyInForce is agentID's signing key, made when it has none in force, and replaced when Lens can no longer derive it.
func (s *Service) keyInForce(ctx context.Context, f economy.AgentCardFacts) (ed25519.PrivateKey, string, error) {
	for range 3 { // another request may make the key between our read and our write; then we read theirs
		k, ok, generation, err := s.store.AgentCardKeyInForce(ctx, f.AgentID)
		if err != nil {
			return nil, "", err
		}
		if ok {
			priv := s.derive(agentKeyInfo(f.AgentID, k.Generation))
			if b64url(priv.Public().(ed25519.PublicKey)) == k.PublicKey {
				return priv, k.Kid, nil
			}
			if err := s.store.RevokeAgentCardKey(ctx, f.AgentID, RevokedSecretChanged); err != nil {
				return nil, "", err
			}
		}
		priv := s.derive(agentKeyInfo(f.AgentID, generation+1))
		pub := priv.Public().(ed25519.PublicKey)
		kid := kya.JWKOf(pub).Kid
		added, err := s.store.AddAgentCardKey(ctx, economy.AgentCardKey{Kid: kid, WorkspaceID: f.WorkspaceID, AgentID: f.AgentID,
			Generation: generation + 1, PublicKey: b64url(pub)})
		if err != nil {
			return nil, "", err
		}
		if added {
			return priv, kid, nil
		}
	}
	return nil, "", fmt.Errorf("a2acard: agent %s's signing key kept changing while it was read", f.AgentID)
}

// agentCard is what agent f's card says, unsigned.
func (s *Service) agentCard(f economy.AgentCardFacts) Card {
	desc := strings.TrimSpace(f.Description)
	if desc == "" {
		desc = f.Name + " is an AI agent with a Talyvor wallet."
	}
	org := f.Owner.Name
	if org == "" {
		org = "Owner not yet verified"
		desc += " Talyvor has not yet confirmed who answers for it."
	} else {
		desc += " " + org + " answers for it; Talyvor has verified them to level " + f.Owner.Level.String()
		if f.Owner.LiveLevel < f.Owner.Level { // the Test provider's checks count toward level, never live level
			desc += " with test checks, and to " + f.Owner.LiveLevel.String() + " with live ones"
		}
		desc += "."
	}
	return Card{Name: f.Name, Description: desc, SupportedInterfaces: []Interface{},
		Provider: Provider{Organization: org, URL: s.base + CardPath(f.AgentID)}, Version: "1",
		Capabilities: Capabilities{Extensions: []Extension{{URI: PrincipalExtension,
			Description: "Who answers for this agent, and how far Talyvor has verified them.",
			Params:      &Principal{AgentID: f.AgentID, Name: f.Owner.Name, Level: f.Owner.Level, LiveLevel: f.Owner.LiveLevel}}}},
		DefaultInputModes: []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"}, Skills: []Skill{}}
}

// talyvorCard is what Talyvor's own card says, unsigned.
func (s *Service) talyvorCard() Card {
	return Card{Name: "Talyvor",
		Description:         "Money and accounts for AI agents. Every Talyvor agent has a signed Agent Card naming who answers for it.",
		SupportedInterfaces: []Interface{}, Provider: Provider{Organization: "Talyvor", URL: "https://talyvor.com"}, Version: "1",
		DefaultInputModes: []string{"application/json"}, DefaultOutputModes: []string{"application/json"},
		Skills: []Skill{{ID: "verify-agent-card", Name: "Check a Talyvor agent's card",
			Description: "Send a Talyvor agent's Agent Card to " + s.base + VerifyPath + " to learn whether Talyvor signed it, it is unchanged and its key is not revoked.",
			Tags:        []string{"identity", "agent-card"}}}}
}

// sign is c with its one signature: EdDSA over its RFC 8785 form, the key named by kid and published at jku.
func sign(c Card, priv ed25519.PrivateKey, kid, jku string) (Card, error) {
	c.Signatures = nil
	payload, err := jcs.Marshal(c)
	if err != nil {
		return Card{}, err
	}
	header, err := jcs.Marshal(map[string]string{"alg": "EdDSA", "typ": "JOSE", "kid": kid, "jku": jku})
	if err != nil {
		return Card{}, err
	}
	protected := b64url(header)
	sig := ed25519.Sign(priv, []byte(protected+"."+b64url(payload)))
	c.Signatures = []Signature{{Protected: protected, Signature: b64url(sig)}}
	return c, nil
}

// Card is agentID's card, signed with its key.
func (s *Service) Card(ctx context.Context, agentID string) (Card, error) {
	f, err := s.store.AgentCardFacts(ctx, agentID)
	if err != nil {
		return Card{}, err
	}
	if f.Archived {
		return Card{}, ErrArchived
	}
	priv, kid, err := s.keyInForce(ctx, f)
	if err != nil {
		return Card{}, err
	}
	return sign(s.agentCard(f), priv, kid, s.base+JWKSPath(agentID))
}

// TalyvorCard is Talyvor's own card, signed with Talyvor's key.
func (s *Service) TalyvorCard() (Card, error) {
	return sign(s.talyvorCard(), s.own, s.ownKid, s.base+TalyvorJWKSPath)
}

// Rotate revokes agentID's key in force, so no card signed with it verifies again, and is its card signed with the
// next. The agent must be workspaceID's.
func (s *Service) Rotate(ctx context.Context, workspaceID, agentID string) (Card, error) {
	f, err := s.store.AgentCardFacts(ctx, agentID)
	if err != nil {
		return Card{}, err
	}
	if f.WorkspaceID != workspaceID {
		return Card{}, economy.ErrAgentNotFound
	}
	if f.Archived {
		return Card{}, ErrArchived
	}
	if err := s.store.RevokeAgentCardKey(ctx, agentID, RevokedRotated); err != nil {
		return Card{}, err
	}
	return s.Card(ctx, agentID)
}

// JWKS is the key agentID's card is signed with now. A revoked key is not published.
func (s *Service) JWKS(ctx context.Context, agentID string) (kya.JWKS, error) {
	f, err := s.store.AgentCardFacts(ctx, agentID)
	if err != nil {
		return kya.JWKS{}, err
	}
	if f.Archived {
		return kya.JWKS{}, ErrArchived
	}
	priv, _, err := s.keyInForce(ctx, f)
	if err != nil {
		return kya.JWKS{}, err
	}
	return kya.JWKS{Keys: []kya.JWK{kya.JWKOf(priv.Public().(ed25519.PublicKey))}}, nil
}

// TalyvorJWKS is the key Talyvor's own card is signed with.
func (s *Service) TalyvorJWKS() kya.JWKS {
	return kya.JWKS{Keys: []kya.JWK{kya.JWKOf(s.own.Public().(ed25519.PublicKey))}}
}

// Verification is Talyvor's answer on a card: valid, or why not, and whose card it is.
type Verification struct {
	Valid     bool       `json:"valid"`
	Reason    string     `json:"reason,omitempty"`
	SignedBy  string     `json:"signed_by,omitempty"` // "talyvor" for Talyvor's own card, else the agent's id
	Principal *Principal `json:"principal,omitempty"`
}

// Verify checks a card: that a signature on it is Talyvor's, over exactly what it says, with a key not revoked, and
// that it still says what is true — a card whose agent was renamed or whose owner's verification changed is
// superseded, and fetched again from its agent's path.
func (s *Service) Verify(ctx context.Context, raw []byte) (Verification, error) {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	var card map[string]any
	if err := d.Decode(&card); err != nil || card == nil {
		return Verification{Reason: "not an Agent Card: it is not a JSON object"}, nil
	}
	sigs, _ := card["signatures"].([]any)
	if len(sigs) == 0 {
		return Verification{Reason: "the card is not signed"}, nil
	}
	delete(card, "signatures")
	payload, err := jcs.Value(card)
	if err != nil {
		return Verification{Reason: "not an Agent Card: " + err.Error()}, nil
	}
	reason := "it is signed with a key Talyvor never published"
	for _, one := range sigs {
		sig, _ := one.(map[string]any)
		protected, _ := sig["protected"].(string)
		signature, _ := sig["signature"].(string)
		var header struct{ Alg, Kid string }
		h, err := base64.RawURLEncoding.DecodeString(protected)
		if err != nil || json.Unmarshal(h, &header) != nil {
			reason = "a signature's protected header is not base64url JSON"
			continue
		}
		var pub ed25519.PublicKey
		var key economy.AgentCardKey
		switch {
		case header.Alg != "EdDSA":
			continue
		case header.Kid == s.ownKid:
			pub = s.own.Public().(ed25519.PublicKey)
		default:
			key, err = s.store.AgentCardKey(ctx, header.Kid)
			if errors.Is(err, economy.ErrAgentCardKeyNotFound) {
				continue
			}
			if err != nil {
				return Verification{}, err
			}
			pub, _ = base64.RawURLEncoding.DecodeString(key.PublicKey)
		}
		sb, err := base64.RawURLEncoding.DecodeString(signature)
		if err != nil || len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, []byte(protected+"."+b64url(payload)), sb) {
			return Verification{Reason: "the signature does not match the card: it was changed after Talyvor signed it"}, nil
		}
		if key.Kid == "" {
			return s.current(payload, s.talyvorCard(), Verification{SignedBy: "talyvor"})
		}
		if key.RevokedAt != nil {
			return Verification{Reason: "revoked: the key it is signed with was revoked (" + key.RevokedReason + ")", SignedBy: key.AgentID}, nil
		}
		f, err := s.store.AgentCardFacts(ctx, key.AgentID)
		if errors.Is(err, economy.ErrAgentNotFound) {
			return Verification{Reason: "the agent no longer exists", SignedBy: key.AgentID}, nil
		}
		if err != nil {
			return Verification{}, err
		}
		if f.Archived {
			return Verification{Reason: ErrArchived.Error(), SignedBy: key.AgentID}, nil
		}
		now := s.agentCard(f)
		return s.current(payload, now, Verification{SignedBy: key.AgentID, Principal: now.Capabilities.Extensions[0].Params})
	}
	return Verification{Reason: reason}, nil
}

// current is v, valid when payload is what card says now.
func (s *Service) current(payload []byte, card Card, v Verification) (Verification, error) {
	want, err := jcs.Marshal(card)
	if err != nil {
		return Verification{}, err
	}
	if string(want) != string(payload) {
		v.Reason = "superseded: the card no longer says what is true; fetch it again"
		return v, nil
	}
	v.Valid = true
	return v, nil
}
