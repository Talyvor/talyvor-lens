package economy

import "testing"

// B17.224 — GPT-4.1 nano ($0.10 / $0.40 per 1M) costs a whole number of µLXC for every token count, and is
// charged exactly that; a genuine fraction of a µLXC still rounds up.
func TestChargeULXC_WholePriceIsNotRoundedUpByFloatNoise(t *testing.T) {
	for in := int64(0); in <= 1220; in++ {
		for out := int64(0); out <= 22; out++ {
			usd := float64(in)*0.10/1e6 + float64(out)*0.40/1e6
			if got, want := ChargeULXC(usd), in+4*out; got != want {
				t.Fatalf("%d in / %d out: charged %d µLXC, catalog price is %d", in, out, got, want)
			}
		}
	}
	if got := ChargeULXC(1.275e-6); got != 13 {
		t.Fatalf("12.75 µLXC charged as %d, want 13", got)
	}
}
