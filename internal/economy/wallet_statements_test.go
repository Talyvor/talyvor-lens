package economy

import (
	"math/big"
	"testing"
	"time"
)

// B48.14: the accountant's export, first in, first out, against a year worked by hand in pence.
//
//	10 Jan  in  2.0 ETH for £4,000                       lot A: 2.0 at £4,000
//	 5 Feb  in  1.0 ETH for £2,600                       lot B: 1.0 at £2,600
//	 1 Mar  out 2.5 ETH for £7,500  basis A £4,000 + half of B £1,300 = £5,300   gain £2,200
//	 1 Apr  out 0.5 ETH for £1,000  basis what is left of B, £1,300              loss £300
//	 1 May  out 0.25 ETH for £500   no lot left: no basis for 0.25 ETH           gain £500
//	 1 Jun  in  1.0 ETH, not valued                      unpriced, left out
func TestFIFOGains_AHandWorkedYear(t *testing.T) {
	const eth = 1_000_000_000_000_000_000
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 12, 0, 0, 0, time.UTC) }
	v := func(pence int64) *int64 { return &pence }
	moves := []TaxMovement{
		{ID: "a", At: day(1, 10), Asset: "ETH", Direction: "in", QuantityMinor: big.NewInt(2 * eth), ValueMinor: v(400000), ValueCurrency: "GBP"},
		{ID: "b", At: day(2, 5), Asset: "ETH", Direction: "in", QuantityMinor: big.NewInt(eth), ValueMinor: v(260000), ValueCurrency: "GBP"},
		{ID: "s1", At: day(3, 1), Asset: "ETH", Direction: "out", QuantityMinor: big.NewInt(5 * eth / 2), ValueMinor: v(750000), ValueCurrency: "GBP"},
		{ID: "s2", At: day(4, 1), Asset: "ETH", Direction: "out", QuantityMinor: big.NewInt(eth / 2), ValueMinor: v(100000), ValueCurrency: "GBP"},
		{ID: "s3", At: day(5, 1), Asset: "ETH", Direction: "out", QuantityMinor: big.NewInt(eth / 4), ValueMinor: v(50000), ValueCurrency: "GBP"},
		{ID: "u", At: day(6, 1), Asset: "ETH", Direction: "in", QuantityMinor: big.NewInt(eth)},
	}
	noBasis := func(d TaxDisposal) int64 {
		if d.NoBasisQuantityMinor == nil {
			return 0
		}
		return d.NoBasisQuantityMinor.Int64()
	}
	year := FIFOGains(moves, "GBP", day(1, 1), day(12, 31))
	want := []struct {
		id                   string
		proceeds, cost, gain int64
		noBasis              int64
	}{{"s1", 750000, 530000, 220000, 0}, {"s2", 100000, 130000, -30000, 0}, {"s3", 50000, 0, 50000, eth / 4}}
	if len(year.Disposals) != len(want) {
		t.Fatalf("disposals = %+v; want %d", year.Disposals, len(want))
	}
	for i, w := range want {
		d := year.Disposals[i]
		if d.ID != w.id || d.ProceedsMinor != w.proceeds || d.CostBasisMinor != w.cost || d.GainMinor != w.gain || noBasis(d) != w.noBasis {
			t.Errorf("disposal %d = %+v; want %+v", i, d, w)
		}
	}
	if year.ProceedsMinor != 900000 || year.CostBasisMinor != 660000 || year.GainMinor != 240000 || len(year.Unpriced) != 1 || year.Unpriced[0].ID != "u" {
		t.Fatalf("totals = %d − %d = %d, unpriced %v; want 900000 − 660000 = 240000 and u", year.ProceedsMinor, year.CostBasisMinor, year.GainMinor, year.Unpriced)
	}

	// From 15 March: the March sale is not in the period, but it still used up lot A and half of B.
	spring := FIFOGains(moves, "GBP", day(3, 15), day(12, 31))
	if len(spring.Disposals) != 2 || spring.Disposals[0].CostBasisMinor != 130000 || spring.GainMinor != 20000 {
		t.Fatalf("from 15 March: %+v, gain %d; want s2 at a basis of 130000 and s3, a gain of 20000", spring.Disposals, spring.GainMinor)
	}
}
