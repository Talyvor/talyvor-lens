package money

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// B59.22 — 3-D SECURE. A merchant online may ask the card's issuer to have the cardholder confirm a payment. For an
// agent's card the cardholder is its owner: the issuer sends Lens the challenge (Lens.ReceiveCardChallenge, through
// the adapter's notifications), Lens files it as an approval of its own kind and pushes it to the owner, and the
// owner's signed yes or no goes back through the issuer's ThreeDSProvider. No answer by the challenge's deadline is a
// no.

// ThreeDSChallenge is an issuer asking for the cardholder's yes to one online payment on one of its cards, by ExpiresAt.
type ThreeDSChallenge struct {
	ID        string    `json:"id"`          // the issuer's
	Card      string    `json:"partner_ref"` // the card's
	Merchant  string    `json:"merchant"`
	Amount    Amount    `json:"amount"`
	ExpiresAt time.Time `json:"expires_at"` // the issuer's deadline for the answer
}

// Valid says what c is missing.
func (c ThreeDSChallenge) Valid() error {
	switch {
	case strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Card) == "":
		return fmt.Errorf("%w: a 3-D Secure challenge names itself and its card", ErrInvalid)
	case strings.TrimSpace(c.Merchant) == "":
		return fmt.Errorf("%w: a 3-D Secure challenge names its merchant", ErrInvalid)
	case c.Amount.Minor <= 0 || c.Amount.Asset == "":
		return fmt.Errorf("%w: a 3-D Secure challenge is for a positive amount in a currency", ErrInvalid)
	case c.ExpiresAt.IsZero():
		return fmt.Errorf("%w: a 3-D Secure challenge carries the issuer's deadline", ErrInvalid)
	}
	return nil
}

// ThreeDSProvider is an issuer's 3-D Secure: Lens answers each of its challenges once, approved or declined.
type ThreeDSProvider interface {
	Answer(ctx context.Context, challengeID string, approved bool) error
}

// ThreeDSIntake is Lens taking partner's challenge (economy.DualTokenStore.ReceiveCardChallenge).
type ThreeDSIntake func(ctx context.Context, partner string, c ThreeDSChallenge) error

// ThreeDS is the 3-D Secure of the issuer partner, the sandbox or the registry's Issuer included.
func (r *Registry) ThreeDS(partner string) (ThreeDSProvider, error) {
	a := r.adapter(partner)
	if a == nil && r.cfg.Issuer != nil && r.cfg.Issuer.Name == partner {
		a = r.cfg.Issuer
	}
	if a == nil || a.ThreeDS == nil {
		return nil, fmt.Errorf("%w: %q answers no 3-D Secure challenge", ErrInvalid, partner)
	}
	return a.ThreeDS, nil
}

// SandboxThreeDSWindow is how long the Sandbox's challenges wait for an answer: EMV 3-D Secure's challenge timeout.
const SandboxThreeDSWindow = 10 * time.Minute

// SandboxThreeDS is the Sandbox's 3-D Secure, as an issuer's would be: Challenge sends Lens a challenge on one of
// Partner's cards, and Answer keeps Lens's answer to it, once. It never calls a partner.
type SandboxThreeDS struct {
	Partner string
	Intake  ThreeDSIntake

	mu      sync.Mutex
	asked   map[string]ThreeDSChallenge
	answers map[string]bool
}

// Challenge asks Lens for the cardholder's yes to c; with no deadline, the Sandbox's window from now.
func (s *SandboxThreeDS) Challenge(ctx context.Context, c ThreeDSChallenge) (ThreeDSChallenge, error) {
	if c.ExpiresAt.IsZero() {
		c.ExpiresAt = time.Now().Add(SandboxThreeDSWindow)
	}
	if err := c.Valid(); err != nil {
		return c, err
	}
	if s.Intake == nil {
		return c, fmt.Errorf("%w: the Sandbox's 3-D Secure has nowhere to send its challenges", ErrInvalid)
	}
	s.mu.Lock()
	if s.asked == nil {
		s.asked = map[string]ThreeDSChallenge{}
	}
	s.asked[c.ID] = c
	s.mu.Unlock()
	return c, s.Intake(ctx, s.Partner, c)
}

// Answer keeps Lens's answer to challenge id. Answered again the same, it is the same; answered the other way, refused.
func (s *SandboxThreeDS) Answer(_ context.Context, id string, approved bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.asked[id]; !ok {
		return fmt.Errorf("%w: the Sandbox sent no 3-D Secure challenge %q", ErrNotFound, id)
	}
	if prev, ok := s.answers[id]; ok && prev != approved {
		return fmt.Errorf("%w: 3-D Secure challenge %q is already answered", ErrInvalid, id)
	}
	if s.answers == nil {
		s.answers = map[string]bool{}
	}
	s.answers[id] = approved
	return nil
}

// Answered is Lens's answer to challenge id, and whether there is one.
func (s *SandboxThreeDS) Answered(id string) (approved, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	approved, ok = s.answers[id]
	return approved, ok
}
