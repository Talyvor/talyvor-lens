package money

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// B59.24 — CARD DISPUTES AND REFUNDS. The owner disputes a payment on an agent's card: Lens opens the dispute with the
// card's issuer through its DisputeProvider, and the issuer reports, through its notifications (DisputeIntake), the
// outcome of each dispute it took and every refund a merchant makes on one of its cards' payments.

// CardDispute is a dispute Lens opens with an issuer on one payment on one of its cards.
type CardDispute struct {
	ID      string `json:"id"`          // Lens's
	Card    string `json:"partner_ref"` // the card's
	Payment string `json:"payment_id"`  // the issuer's id for the payment (its authorisation)
	Reason  string `json:"reason"`
	Amount  Amount `json:"amount"`
}

// Valid says what d is missing.
func (d CardDispute) Valid() error {
	switch {
	case strings.TrimSpace(d.ID) == "" || strings.TrimSpace(d.Card) == "" || strings.TrimSpace(d.Payment) == "":
		return fmt.Errorf("%w: a card dispute names itself, its card and its payment", ErrInvalid)
	case strings.TrimSpace(d.Reason) == "":
		return fmt.Errorf("%w: a card dispute gives its reason", ErrInvalid)
	case d.Amount.Minor <= 0 || d.Amount.Asset == "":
		return fmt.Errorf("%w: a card dispute is for a positive amount in a currency", ErrInvalid)
	}
	return nil
}

// DisputeOutcome is the issuer's decision on a dispute it took: won, the payment comes back.
type DisputeOutcome struct {
	Ref string `json:"ref"` // the issuer's, as Open answered it
	Won bool   `json:"won"`
}

// CardRefund is a merchant giving back some or all of one payment on one of an issuer's cards.
type CardRefund struct {
	ID      string `json:"id"`         // the issuer's
	Payment string `json:"payment_id"` // the issuer's id for the payment it refunds
	Amount  Amount `json:"amount"`
}

// Valid says what r is missing.
func (r CardRefund) Valid() error {
	switch {
	case strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.Payment) == "":
		return fmt.Errorf("%w: a card refund names itself and its payment", ErrInvalid)
	case r.Amount.Minor <= 0 || r.Amount.Asset == "":
		return fmt.Errorf("%w: a card refund is a positive amount in a currency", ErrInvalid)
	}
	return nil
}

// DisputeProvider is an issuer's card disputes: Open hands it a dispute, once under d.ID, and answers its reference.
type DisputeProvider interface {
	Open(ctx context.Context, d CardDispute) (ref string, err error)
}

// DisputeIntake is Lens taking partner's notifications: a dispute's outcome and a merchant's refund
// (economy.DualTokenStore.SetSandboxDisputes).
type DisputeIntake struct {
	Outcome func(ctx context.Context, partner string, o DisputeOutcome) error
	Refund  func(ctx context.Context, partner string, r CardRefund) error
}

// Disputes is the card disputes of the issuer partner, the sandbox or the registry's Issuer included.
func (r *Registry) Disputes(partner string) (DisputeProvider, error) {
	a := r.adapter(partner)
	if a == nil && r.cfg.Issuer != nil && r.cfg.Issuer.Name == partner {
		a = r.cfg.Issuer
	}
	if a == nil || a.Disputes == nil {
		return nil, fmt.Errorf("%w: %q takes no card dispute", ErrInvalid, partner)
	}
	return a.Disputes, nil
}

// SandboxDisputes is the Sandbox's card disputes, as an issuer's would be: Open takes a dispute, once, and Decide and
// Refund send Lens a dispute's outcome and a merchant's refund. It never calls a partner.
type SandboxDisputes struct {
	Partner string
	Intake  DisputeIntake

	mu     sync.Mutex
	opened map[string]CardDispute // by reference
}

// Open takes d and answers its reference; opened again under the same id, the same reference.
func (s *SandboxDisputes) Open(_ context.Context, d CardDispute) (string, error) {
	if err := d.Valid(); err != nil {
		return "", err
	}
	ref := "sbx_dp_" + d.ID
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opened == nil {
		s.opened = map[string]CardDispute{}
	}
	s.opened[ref] = d
	return ref, nil
}

// Decide sends Lens the outcome of the dispute it took as ref.
func (s *SandboxDisputes) Decide(ctx context.Context, ref string, won bool) error {
	s.mu.Lock()
	_, ok := s.opened[ref]
	s.mu.Unlock()
	switch {
	case !ok:
		return fmt.Errorf("%w: the Sandbox took no card dispute %q", ErrNotFound, ref)
	case s.Intake.Outcome == nil:
		return fmt.Errorf("%w: the Sandbox's disputes have nowhere to send their outcomes", ErrInvalid)
	}
	return s.Intake.Outcome(ctx, s.Partner, DisputeOutcome{Ref: ref, Won: won})
}

// Refund sends Lens a merchant's refund r.
func (s *SandboxDisputes) Refund(ctx context.Context, r CardRefund) error {
	if err := r.Valid(); err != nil {
		return err
	}
	if s.Intake.Refund == nil {
		return fmt.Errorf("%w: the Sandbox's disputes have nowhere to send their refunds", ErrInvalid)
	}
	return s.Intake.Refund(ctx, s.Partner, r)
}
