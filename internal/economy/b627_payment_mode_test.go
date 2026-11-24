package economy

import (
	"slices"
	"testing"
)

// B62.7 — a request to be paid in USDC files the payer's approval with the mode of the money it would move: Live only
// while stablecoins and paying another owner are both Live, never the label of spending on Talyvor.
func TestB627_AStablecoinPaymentToAnotherOwnerMovesThroughStablecoinsAndPayingAnotherOwner(t *testing.T) {
	got := paymentCapabilities("stablecoin", Payee{Kind: "agent", ID: "agt_other"}, true)
	if want := []string{CapabilityStablecoins, CapabilityPayAnotherOwner}; !slices.Equal(got, want) {
		t.Fatalf("capabilities = %v, want %v", got, want)
	}
}
