package economy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// agent_rule_simulator.go — B28.306: "WOULD THIS REQUEST PASS MY RULES?", ANSWERED WITHOUT MOVING MONEY.
//
// SimulateAgentRules judges a request the agent has not made — a question to a model, or a payment to a payee —
// with enforceAgentRules itself, the very judgement a hold or a debit makes, inside a transaction that is always
// rolled back. Whatever the judgement writes on the way (the payee ledger's row, an unusual-spend alert, a used
// approval) goes with it, so a simulation posts nothing, files no approval and pauses no one. The spend it counts
// is the agent's real spend so far. A licence to a listing (B32.22) is judged by the licence rules too. With AlwaysAllow
// it is judged as though the owner had always allowed its payee so (B41.104): the preview before that rule is saved.

// ErrBadSimulation: the simulated request cannot be judged as given.
var ErrBadSimulation = errors.New("economy: the simulated request cannot be judged")

// SimulatedRequest is a request to judge. With Payee it is a payment to that payee — a listing's is a use of the
// listing, or with Licence a licence to it (B32.22): its kind and licence, committing the agent to AmountULXC — and
// without one a question to Model through Provider.
type SimulatedRequest struct {
	AmountULXC int64       `json:"amount_ulxc"`
	Model      string      `json:"model"`
	Provider   string      `json:"provider"`
	Payee      *Payee      `json:"payee"`
	Licence    *Commitment `json:"licence"`
	At         *time.Time  `json:"at"` // when it is asked, for the active hours and the periods; now when absent
	// AlwaysAllow judges a payment as though this allowance for its payee were saved (B41.104); it is not.
	AlwaysAllow *PayeeAllowance `json:"always_allow"`
}

// RuleSimulation is the rules' answer: allowed, refused, or approval_required (it would wait for the workspace's
// owner). Reason says why when it is not allowed. BalanceULXC is what the agent holds now, which the rules do not
// judge: a request they allow is still refused beyond it.
type RuleSimulation struct {
	Verdict     string    `json:"verdict"`
	Reason      string    `json:"reason"`
	AmountULXC  int64     `json:"amount_ulxc"`
	At          time.Time `json:"at"`
	BalanceULXC int64     `json:"balance_ulxc"`
}

// SimulateAgentRules answers whether the agent's rules would let in through, and moves nothing.
func (s *DualTokenStore) SimulateAgentRules(ctx context.Context, workspaceID, agentID string, in SimulatedRequest) (RuleSimulation, error) {
	out := RuleSimulation{AmountULXC: in.AmountULXC, At: time.Now()}
	if in.At != nil {
		out.At = *in.At
	}
	if in.AmountULXC < 0 {
		return out, fmt.Errorf("%w: amount_ulxc cannot be negative", ErrBadSimulation)
	}
	req := AgentRequest{Model: in.Model, Provider: in.Provider, At: out.At, Fingerprint: "simulated:" + uuid.NewString()}
	what := "request"
	if in.Payee != nil {
		switch in.Payee.Kind {
		case "agent", "listing", "company", "merchant":
		default:
			return out, fmt.Errorf("%w: a payee's kind is agent, listing, company or merchant, not %q", ErrBadSimulation, in.Payee.Kind)
		}
		if in.Payee.ID == "" {
			return out, fmt.Errorf("%w: the payee needs its id", ErrBadSimulation)
		}
		req.Payment, req.Payee, what = true, *in.Payee, "payment"
		if in.Payee.Kind == "listing" {
			req.Listing = in.Payee.ID
		}
	}
	if in.Licence != nil {
		switch {
		case in.Payee == nil || in.Payee.Kind != "listing":
			return out, fmt.Errorf("%w: a licence is to a listing: name it as the payee", ErrBadSimulation)
		case in.Licence.Kind != "buy" && in.Licence.Kind != "rent" && in.Licence.Kind != "subscribe":
			return out, fmt.Errorf("%w: a licence's kind is buy, rent or subscribe, not %q", ErrBadSimulation, in.Licence.Kind)
		case in.Licence.Licence != "personal" && !slices.Contains(AgentLicences, in.Licence.Licence):
			return out, fmt.Errorf("%w: a licence is personal, commercial or enterprise, not %q", ErrBadSimulation, in.Licence.Licence)
		}
		req.Commitment, what = Commitment{Kind: in.Licence.Kind, Licence: in.Licence.Licence, ULXC: in.AmountULXC}, in.Licence.Kind
	}

	if in.AlwaysAllow != nil && in.Payee == nil {
		return out, fmt.Errorf("%w: always_allow is for a payment: name its payee", ErrBadSimulation)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // always: the simulation keeps nothing it wrote
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`,
		agentID, workspaceID).Scan(&exists); err != nil {
		return out, fmt.Errorf("economy: simulate agent rules: %w", err)
	}
	if !exists {
		return out, ErrAgentNotFound
	}
	if out.BalanceULXC, err = accountBalance(ctx, tx, workspaceID, agentAccount(agentID)); err != nil {
		return out, err
	}
	if in.AlwaysAllow != nil { // saved in tx only, which is rolled back
		if err := allowPayeeTx(ctx, tx, workspaceID, agentID, in.Payee.ID, *in.AlwaysAllow); errors.Is(err, ErrAgentRule) {
			return out, fmt.Errorf("%w: %s", ErrBadSimulation, strings.TrimPrefix(err.Error(), ErrAgentRule.Error()+": "))
		} else if err != nil {
			return out, err
		}
	}
	// The fingerprint is new, so no approval matches it: a request above the approval amount answers that it
	// needs one rather than using one a person granted to a real request.
	err = enforceAgentRules(WithAgentRequest(ctx, req), tx, workspaceID, agentID, in.AmountULXC, "simulated")
	var need *ApprovalNeededError
	switch {
	case err == nil:
		out.Verdict = "allowed"
	case errors.As(err, &need):
		out.Verdict = "approval_required"
		out.Reason = fmt.Sprintf("this %s would cost up to %s LXC, above the agent's approval amount: it would wait for the workspace's owner to approve it",
			what, lxcString(in.AmountULXC))
	case errors.Is(err, ErrAgentRule):
		out.Verdict = "refused"
		out.Reason = strings.TrimPrefix(err.Error(), ErrAgentRule.Error()+": ")
	default:
		return out, fmt.Errorf("economy: simulate agent rules: %w", err)
	}
	return out, nil
}
