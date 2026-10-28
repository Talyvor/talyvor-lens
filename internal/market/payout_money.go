package market

import (
	"context"

	"github.com/talyvor/lens/internal/economy"
)

// PayoutMoney is a payout made through the money layer's payouts (B46.22).
const PayoutMoney = "money"

// EarningsPayer pays earnings out on the money layer. *economy.DualTokenStore satisfies it.
type EarningsPayer interface {
	PayEarnings(ctx context.Context, workspaceID string, e economy.Earnings) ([]economy.EarningsPayout, error)
}

// SetMoneyPayouts pays sellers who saved payout details on the money layer through payer, from workspace from's accounts.
func (s *Store) SetMoneyPayouts(payer EarningsPayer, from string) {}
