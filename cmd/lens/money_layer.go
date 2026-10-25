package main

import (
	"context"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
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
		Reference: e.Reference, AmountMinor: e.Amount.Minor, Currency: e.Amount.Asset, Funding: funding})
	return entry.WorkspaceID, err
}
