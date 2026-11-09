package partners

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AgentTokenProvider issues card-network agent tokens: a card credential an agent pays with, which the network
// knows is an agent's, bound to one merchant and a spending limit, and which its owner can revoke — the capability
// agent_card. This is the shape of Visa Intelligent Commerce and Mastercard Agent Pay tokens.
type AgentTokenProvider interface {
	// Name identifies the implementation on every request it handles.
	Name() string
	// Provision issues a token for an agent. Idempotent on req.ID.
	Provision(ctx context.Context, req AgentTokenRequest) (AgentToken, error)
	// Authorise asks the network to approve a payment with a token. Idempotent on req.ID.
	Authorise(ctx context.Context, req Authorisation) (Result, error)
	// Status says where a token or an authorisation stands now.
	Status(ctx context.Context, ref string) (Result, error)
	// Revoke stops a token: every later authorisation with it is declined.
	Revoke(ctx context.Context, tokenRef string) error
}

// AgentTokenRequest asks for a token for an agent, to pay one merchant up to a limit.
type AgentTokenRequest struct {
	ID        string    `json:"id"` // the caller's id for the token
	AgentID   string    `json:"agent_id"`
	AgentName string    `json:"agent_name"`
	Merchant  string    `json:"merchant"`
	Limit     Money     `json:"limit"` // the most the token pays, all its payments together, in the one currency it pays in
	ExpiresAt time.Time `json:"expires_at"`
}

// AgentToken is a token an agent pays with: never the card number, only its network and last four digits.
type AgentToken struct {
	Result
	Network   string    `json:"network"`
	Last4     string    `json:"last4"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Authorisation is a payment with a token.
type Authorisation struct {
	ID       string `json:"id"` // the caller's id for the payment
	TokenRef string `json:"token_ref"`
	Amount   Money  `json:"amount"`
	Merchant string `json:"merchant"`
}

// TestAgentTokenProvider is test mode for agent tokens: it reaches no card network. A token is provisioned by the
// agent's name, and an authorisation answers by its amount (see the package comment); a revoked or expired token,
// a payment to another merchant or in another currency, and one that takes the token past its limit are declined.
type TestAgentTokenProvider struct {
	book    testBook
	revoked map[string]bool
}

// Name is "test".
func (*TestAgentTokenProvider) Name() string { return "test" }

// Provision issues a token on the "test" network.
func (p *TestAgentTokenProvider) Provision(_ context.Context, req AgentTokenRequest) (AgentToken, error) {
	if err := checkMoney(req.Limit); err != nil {
		return AgentToken{}, err
	}
	if strings.TrimSpace(req.AgentID) == "" || strings.TrimSpace(req.Merchant) == "" {
		return AgentToken{}, fmt.Errorf("%w: a token is for an agent and a merchant", ErrInvalid)
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	if !req.ExpiresAt.After(p.book.clock()) {
		if _, retry := p.book.ops[testRef("token", req.ID)]; !retry {
			return AgentToken{}, fmt.Errorf("%w: a token expires in the future", ErrInvalid)
		}
	}
	op, _, err := p.book.record("token", req.ID, req, byName(req.AgentName))
	if err != nil {
		return AgentToken{}, err
	}
	return AgentToken{Result: op.result, Network: "test", Last4: testDigits(op.result.Ref, 4), ExpiresAt: req.ExpiresAt}, nil
}

// Authorise approves nothing for real: it answers by the amount.
func (p *TestAgentTokenProvider) Authorise(_ context.Context, req Authorisation) (Result, error) {
	if err := checkMoney(req.Amount); err != nil {
		return Result{}, err
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	token, err := p.book.get("token", req.TokenRef)
	if err != nil {
		return Result{}, err
	}
	t := token.request.(AgentTokenRequest)
	decide := byAmount(req.Amount)
	switch {
	case token.status().Status != StatusCompleted || p.revoked[req.TokenRef]:
		decide = declined("test mode: the token is not active")
	case !p.book.clock().Before(t.ExpiresAt):
		decide = declined("test mode: the token has expired")
	case !strings.EqualFold(strings.TrimSpace(req.Merchant), strings.TrimSpace(t.Merchant)):
		decide = declined("test mode: the token pays only " + t.Merchant)
	case t.Limit.Currency != req.Amount.Currency:
		decide = declined("test mode: the token pays in " + t.Limit.Currency)
	case req.Amount.Minor > t.Limit.Minor-p.spent(req.TokenRef): // never overflows: spent stays within the limit
		decide = declined("test mode: the payment takes the token past its limit")
	}
	op, _, err := p.book.record("auth", req.ID, req, decide)
	if err != nil {
		return Result{}, err
	}
	return op.result, nil
}

// spent is what the token's approved and pending payments add up to. The caller holds p.book.mu.
func (p *TestAgentTokenProvider) spent(tokenRef string) (minor int64) {
	for ref, op := range p.book.ops {
		if !strings.HasPrefix(ref, "test_auth_") || op.request.(Authorisation).TokenRef != tokenRef {
			continue
		}
		if s := op.status().Status; s == StatusCompleted || s == StatusPending {
			minor += op.request.(Authorisation).Amount.Minor
		}
	}
	return minor
}

// Status is where a token or an authorisation stands now: a revoked token reads failed.
func (p *TestAgentTokenProvider) Status(_ context.Context, ref string) (Result, error) {
	if strings.HasPrefix(ref, "test_token_") {
		r, err := p.book.status("token", ref)
		p.book.mu.Lock()
		defer p.book.mu.Unlock()
		if err == nil && p.revoked[ref] {
			r.Status, r.Detail = StatusFailed, "revoked"
		}
		return r, err
	}
	return p.book.status("auth", ref)
}

// Revoke stops the token.
func (p *TestAgentTokenProvider) Revoke(_ context.Context, tokenRef string) error {
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	if _, err := p.book.get("token", tokenRef); err != nil {
		return err
	}
	if p.revoked == nil {
		p.revoked = map[string]bool{}
	}
	p.revoked[tokenRef] = true
	return nil
}
