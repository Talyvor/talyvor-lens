package economy

import "testing"

// B30.33 — a USDC transfer over LENS_TRAVEL_RULE_MINOR carries the payer's and the payee's names; at or under it, neither.
// Unset, the threshold is 0, so every transfer carries both.
func TestStablecoinTransfer_CarriesBothNamesOverTheTravelRuleThreshold(t *testing.T) {
	p := OutsidePayment{ID: "mpay_1", AmountMinor: 5_000_000, Currency: CurrencyUSDC}
	payee := OutsidePayee{Name: "Ada Lovelace", Network: "base", Address: "0xada"}
	for _, c := range []struct {
		over                    int64
		originator, beneficiary string
	}{{0, "Acme Ltd", "Ada Lovelace"}, {4_999_999, "Acme Ltd", "Ada Lovelace"}, {5_000_000, "", ""}} {
		got := stablecoinTransfer(p, payingAccount{partnerRef: "0xacme"}, payee, "Acme Ltd", c.over)
		if got.Originator != c.originator || got.Beneficiary != c.beneficiary || got.From != "0xacme" || got.To != "0xada" {
			t.Errorf("over %d: %+v, want originator %q and beneficiary %q", c.over, got, c.originator, c.beneficiary)
		}
	}
}
