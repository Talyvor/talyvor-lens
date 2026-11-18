package economy

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// payment_mode.go — B62.7: EACH PAYMENT'S RECORD SAYS WHETHER IT WAS SANDBOX OR LIVE.
//
// Labels (B56.7) are worked out from the rails and clearances of the moment they are read, so a record that kept no
// mode could not say afterwards which it was. A payment_proofs row and an agent_approvals row keep their mode as they
// are written, in their own transaction: Live only while every money capability the payment moves through is Live for
// the workspace, and its money, where the payment names it, is live money. Anything else is Sandbox.

// paymentCapabilities is every money capability a payment on rail to payee moves money through; otherOwner says the
// payee is an agent of another owner.
func paymentCapabilities(rail string, payee Payee, otherOwner bool) []string {
	var keys []string
	switch rail {
	case "card":
		keys = append(keys, CapabilityAgentCard)
	case "account":
		keys = append(keys, CapabilityCurrencyAccounts)
	case "marketplace_bill":
		keys = append(keys, CapabilityBuyListings)
	default: // the agent's LXC
		switch {
		case payee.Kind == "agent" && !otherOwner:
			keys = append(keys, "move_between_own_agents")
		case payee.Kind == "listing":
			keys = append(keys, CapabilityBuyListings)
		default:
			keys = append(keys, "spend_on_talyvor")
		}
	}
	switch {
	case payee.Kind == "outside":
		keys = append(keys, CapabilityPaymentsOut)
	case payee.Kind == "person":
		keys = append(keys, CapabilityPayoutsToPeople)
	case payee.Kind == "company" || otherOwner:
		keys = append(keys, CapabilityPayAnotherOwner)
	}
	return keys
}

// paymentMode is the mode, LabelLive or LabelSandbox, of workspaceID's payment on rail to payee, read through q — the
// payment's own transaction — as it is now. funding is the money's own funding where the payment names it ("" when it
// does not): anything but live money is Sandbox whatever the labels say.
func (s *DualTokenStore) paymentMode(ctx context.Context, q pgxDB, workspaceID, rail string, payee Payee, funding string) (string, error) {
	if funding != "" && funding != FundingLive {
		return LabelSandbox, nil
	}
	otherOwner := false
	if payee.Kind == "agent" && payee.ID != "" {
		var payeeWorkspace string
		err := q.QueryRow(ctx, `SELECT workspace_id FROM agent_accounts WHERE id = $1`, payee.ID).Scan(&payeeWorkspace)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("economy: payment mode: %w", err)
		}
		otherOwner = err == nil && payeeWorkspace != workspaceID
	}
	labels, err := s.workspaceLabelsIn(ctx, q, workspaceID)
	if err != nil {
		return "", fmt.Errorf("economy: payment mode: %w", err)
	}
	for _, key := range paymentCapabilities(rail, payee, otherOwner) {
		if labels[key] != LabelLive {
			return LabelSandbox, nil
		}
	}
	return LabelLive, nil
}
