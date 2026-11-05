package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
)

// B46.3 — the money layer's wiring: a verified partner webhook's money is posted as a payment in, through the one path
// every payment in takes (economy.ReceivePayment). Money leaving through the layer is judged first by the owner's
// limits (economy.DualTokenStore.MoneyLimits, B46.4).

// moneyLedger posts a webhook's money in to the account at the partner it names.
type moneyLedger struct{ store *economy.DualTokenStore }

func (l moneyLedger) PostCredit(ctx context.Context, partner string, e money.Event) (string, error) {
	funding := economy.FundingLive // posted live only once the capability is cleared: PostMoney asks it
	if partner == money.SandboxName {
		funding = economy.FundingTest
	}
	entry, err := l.store.ReceivePayment(ctx, economy.InboundPayment{PartnerAccountRef: e.To, PartnerRef: e.PartnerRef, Payer: e.Payer,
		Reference: e.Reference, AmountMinor: e.Amount.Minor, Currency: e.Amount.Asset, Funding: funding, Partner: partner})
	if errors.Is(err, economy.ErrDepositQuarantined) { // B48.7: kept for its case, so the notification is taken
		return entry.WorkspaceID, nil
	}
	return entry.WorkspaceID, err
}

// SettleTransfer moves the payment out a partner's webhook settles (B46.8), or the earnings payout (B46.11).
func (l moneyLedger) SettleTransfer(ctx context.Context, partner string, e money.Event) (string, string, error) {
	p, err := l.store.SettleOutsidePayment(ctx, partner, e)
	if errors.Is(err, money.ErrNotFound) {
		po, err := l.store.SettleEarningsPayout(ctx, partner, e)
		return po.WorkspaceID, po.ID, err
	}
	return p.WorkspaceID, p.ID, err
}

// newTravelRule is LENS_TRAVEL_RULE's adapter (B48.7): nil when it is unset, the step off.
func newTravelRule(raw string) (partners.TravelRuleProvider, error) {
	kind, list, _ := strings.Cut(strings.TrimSpace(raw), ":")
	switch kind {
	case "":
		return nil, nil
	case "test":
		vasps := map[string]string{}
		for _, pair := range strings.Split(list, ",") {
			if addr, name, ok := strings.Cut(pair, "="); ok {
				vasps[strings.ToLower(strings.TrimSpace(addr))] = strings.TrimSpace(name)
			}
		}
		return &partners.TestTravelRuleProvider{VASPs: vasps}, nil
	}
	return nil, fmt.Errorf("LENS_TRAVEL_RULE is \"test\" or \"test:<address>=<provider>,…\", not %q", raw)
}
