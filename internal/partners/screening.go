package partners

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ScreeningProvider screens names and payments against sanctions lists before any money moves (B30.6). A
// screening is an answer now: it has nothing to come back.
type ScreeningProvider interface {
	// Name identifies the implementation on every request it handles.
	Name() string
	// ScreenName screens one name.
	ScreenName(ctx context.Context, req NameScreen) (Screening, error)
	// ScreenPayment screens a payment's payer and payee.
	ScreenPayment(ctx context.Context, req PaymentScreen) (Screening, error)
}

// The outcomes of a screening.
const (
	ScreenClear  = "clear"  // no match: the money may move
	ScreenHit    = "hit"    // a match: the money must not move
	ScreenReview = "review" // a close match: the money waits for a person to decide
)

// ErrScreeningUnavailable: the provider could not screen. Nothing may move on it.
var ErrScreeningUnavailable = errors.New("partners: screening is unavailable")

// NameScreen is a name to screen.
type NameScreen struct {
	Name        string `json:"name"`
	Country     string `json:"country,omitempty"`
	DateOfBirth string `json:"date_of_birth,omitempty"`
}

// PaymentScreen is a payment to screen. A party left empty is not screened — the workspace's own side, verified
// already (B30.4) — but one party at least is named.
type PaymentScreen struct {
	ID     string `json:"id"` // the caller's id for the payment
	Payer  string `json:"payer,omitempty"`
	Payee  string `json:"payee,omitempty"`
	Amount Money  `json:"amount"`
}

// Screening is a screening's outcome, and what matched.
type Screening struct {
	Outcome string           `json:"outcome"`
	Matches []ScreeningMatch `json:"matches"`
	Detail  string           `json:"detail,omitempty"`
}

// ScreeningMatch is one list entry a name matched, and how closely, in basis points (10,000 is exact).
type ScreeningMatch struct {
	Name     string `json:"name"`
	List     string `json:"list"`
	Entry    string `json:"entry"`
	ScoreBPS int    `json:"score_bps"`
	Listed   string `json:"listed,omitempty"` // the name or alias as the list prints it
	Weak     bool   `json:"weak,omitempty"`   // a low-quality alias: a match on it is held for review, never a hit
}

// ScreeningList is the sanctions lists a Test screening provider screens against (internal/screening): each entry a
// name is close enough to, and how close. It answers ErrScreeningUnavailable while a list has never been loaded.
type ScreeningList interface {
	Match(ctx context.Context, name string) ([]ScreeningMatch, error)
}

// TestScreeningProvider is test mode for screening. It screens against List — the UK and US sanctions lists Lens
// downloads each day (internal/screening) — and the test words: a name containing TESTSANCTION is a hit,
// TESTPENDING is held for review, TESTFAIL makes the provider unavailable. An exact match on a listed name or a good
// alias is a hit; a close match, or an exact one on a low-quality alias, is held for review; anything else is clear.
// With no List it screens the test words alone. B48.7: a crypto address in Addresses is a hit, whatever it is flagged
// for.
type TestScreeningProvider struct {
	List      ScreeningList
	Addresses map[string]string // lower-case address → why it is flagged: AddressSanctioned, AddressMixer, …
}

// Why a crypto address is flagged (B48.7). Each is refused when money goes to it and quarantined when money comes from
// it.
const (
	AddressSanctioned    = "sanctioned"     // on a sanctions list
	AddressMixer         = "mixer"          // a mixer or tumbler
	AddressPrivacyBridge = "privacy_bridge" // a bridge into a privacy coin
	AddressRussiaBelarus = "russia_belarus" // linked to Russia or Belarus
)

// Name is "test".
func (TestScreeningProvider) Name() string { return "test" }

// ScreenName screens the name.
func (p TestScreeningProvider) ScreenName(ctx context.Context, req NameScreen) (Screening, error) {
	if strings.TrimSpace(req.Name) == "" {
		return Screening{}, fmt.Errorf("%w: a screening names who it screens", ErrInvalid)
	}
	return p.screen(ctx, req.Name)
}

// ScreenPayment screens the payer and the payee it names: the worse outcome of the two.
func (p TestScreeningProvider) ScreenPayment(ctx context.Context, req PaymentScreen) (Screening, error) {
	if err := checkID(req.ID); err != nil {
		return Screening{}, err
	}
	if err := checkMoney(req.Amount); err != nil {
		return Screening{}, err
	}
	if strings.TrimSpace(req.Payer) == "" && strings.TrimSpace(req.Payee) == "" {
		return Screening{}, fmt.Errorf("%w: a payment screening names its payer or its payee", ErrInvalid)
	}
	out := Screening{Outcome: ScreenClear, Matches: []ScreeningMatch{}}
	for _, name := range []string{req.Payer, req.Payee} {
		if strings.TrimSpace(name) == "" {
			continue
		}
		s, err := p.screen(ctx, name)
		if err != nil {
			return Screening{}, err
		}
		out = worse(out, s)
	}
	return out, nil
}

// screen is one name against the test words and then the list.
func (p TestScreeningProvider) screen(ctx context.Context, name string) (Screening, error) {
	out, err := testScreen(name)
	if why, ok := p.Addresses[strings.ToLower(strings.TrimSpace(name))]; ok && err == nil {
		out = Screening{Outcome: ScreenHit, Matches: []ScreeningMatch{{Name: name, List: why, Entry: name, ScoreBPS: 10_000}},
			Detail: fmt.Sprintf("%s is flagged: %s", name, why)}
	}
	if err != nil || p.List == nil {
		return out, err
	}
	matches, err := p.List.Match(ctx, name)
	if err != nil {
		return Screening{}, err
	}
	listed := Screening{Outcome: ScreenClear, Matches: matches}
	for _, m := range matches {
		switch {
		case m.ScoreBPS >= 10_000 && !m.Weak:
			listed.Outcome, listed.Detail = ScreenHit, fmt.Sprintf("%s is on the %s sanctions list", name, m.List)
		case listed.Outcome == ScreenClear:
			listed.Outcome, listed.Detail = ScreenReview, fmt.Sprintf("%s is close to %q on the %s sanctions list", name, m.Listed, m.List)
		}
	}
	return worse(out, listed), nil
}

// worse is a and b together: every match of both, and the worse outcome — a hit, then a review, then clear.
func worse(a, b Screening) Screening {
	out := Screening{Outcome: a.Outcome, Detail: a.Detail, Matches: append(append([]ScreeningMatch{}, a.Matches...), b.Matches...)}
	if b.Outcome == ScreenHit && a.Outcome != ScreenHit || b.Outcome == ScreenReview && a.Outcome == ScreenClear {
		out.Outcome, out.Detail = b.Outcome, b.Detail
	}
	return out
}

func testScreen(name string) (Screening, error) {
	n := strings.ToUpper(name)
	match := func(score int) []ScreeningMatch {
		return []ScreeningMatch{{Name: name, List: "TEST", Entry: "TESTSANCTION", ScoreBPS: score}}
	}
	switch {
	case strings.Contains(n, "TESTFAIL"):
		return Screening{}, fmt.Errorf("%w: test mode: names containing TESTFAIL make screening unavailable", ErrScreeningUnavailable)
	case strings.Contains(n, "TESTSANCTION"):
		return Screening{Outcome: ScreenHit, Matches: match(10_000), Detail: "test mode: names containing TESTSANCTION are on a sanctions list"}, nil
	case strings.Contains(n, "TESTPENDING"):
		return Screening{Outcome: ScreenReview, Matches: match(9_000), Detail: "test mode: names containing TESTPENDING are held for review"}, nil
	}
	return Screening{Outcome: ScreenClear, Matches: []ScreeningMatch{}}, nil
}
