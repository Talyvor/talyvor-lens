package issuerfake_test

import (
	"testing"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/issuerfake"
	"github.com/talyvor/lens/internal/money/moneytest"
)

// TestCardContract: the fake issuer passes the card-partner contract every real card adapter must pass (B53.10).
func TestCardContract(t *testing.T) {
	moneytest.CardContract(t, func(decide moneytest.Decide) (money.CardIssuer, moneytest.CardPartner) {
		i := issuerfake.New(decide)
		return i, i
	}, money.GBP)
}
