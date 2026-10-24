package main

import (
	"context"
	"fmt"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
)

// B46.3 — the money layer's wiring: a verified partner webhook's money is posted as a payment in, through the one path
// every payment in takes (economy.ReceivePayment), and no money leaves through the layer until the owner's limits are
// wired into it.

// moneyLedger posts a webhook's money in to the account at the partner it names.
type moneyLedger struct{ store *economy.DualTokenStore }

func (l moneyLedger) PostCredit(ctx context.Context, partner string, e money.Event) (string, error) {
	funding := economy.FundingLive // posted live only once the capability is cleared: PostMoney asks it
	if partner == money.SandboxName {
		funding = economy.FundingTest
	}
	entry, err := l.store.ReceivePayment(ctx, economy.InboundPayment{PartnerAccountRef: e.To, PartnerRef: e.PartnerRef, Payer: e.Payer,
		Reference: e.Reference, AmountMinor: e.Amount.Minor, Currency: e.Amount.Asset, Funding: funding})
	return entry.WorkspaceID, err
}

// noLimitsYet holds every payment out: nothing calls the layer to move money until agent wallets (B46.4) and payments
// (B46.8) bring the owner's limits and one-tap approval with them.
type noLimitsYet struct{}

func (noLimitsYet) Allow(context.Context, money.Op) error {
	return fmt.Errorf("%w: the owner's limits are not wired into the money layer yet", money.ErrLimit)
}
