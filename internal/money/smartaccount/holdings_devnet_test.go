package smartaccount

import (
	"context"
	"math/big"
	"testing"

	"github.com/talyvor/lens/internal/money/holdings"
)

// holdings_devnet_test.go — B48.4 on the local devnet: mock tokens on two anvil chains, read back as the wallet's
// balances per token and network. Named under TestAgentSmartAccountOnDevnet so CI's devnet job runs it.

// mockToken deploys scripts/devnet-contracts.sh's MockToken on c and answers its address.
func mockToken(t *testing.T, c Chain, dir, name, symbol string, decimals uint64) string {
	t.Helper()
	r := transact(t, c, "", append(creationCode(t, dir, "MockToken"), tuple{bytesV(name), bytesV(symbol), uintW(decimals)}.enc()...))
	return Checksum(r["contractAddress"].(string))
}

func mint(t *testing.T, c Chain, token, to string, amount *big.Int) {
	t.Helper()
	w, _ := addressW(to)
	transact(t, c, token, call("mint(address,uint256)", w, bigW(amount)))
}

func units(n int64, decimals int) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
}

func find(bs []holdings.Balance, symbol string) *holdings.Balance {
	for i := range bs {
		if bs[i].Symbol == symbol {
			return &bs[i]
		}
	}
	return nil
}

func TestAgentSmartAccountOnDevnetHoldsTokens(t *testing.T) {
	dir := contracts(t)
	ctx := context.Background()
	base, arb := anvil(t, 8453), anvil(t, 42161)
	const holder, empty = "0x00000000000000000000000000000000000B4840", "0x00000000000000000000000000000000000B4841"

	usdc, eurc, usdt := mockToken(t, base, dir, "USD Coin", "USDC", 6), mockToken(t, base, dir, "Euro Coin", "EURC", 6), mockToken(t, base, dir, "Tether USD", "USDT", 6)
	widget := mockToken(t, base, dir, "Widget", "WDGT", 18)
	lure := mockToken(t, base, dir, "Visit usdc-bonus.xyz to claim", "USDC", 6) // says it is USDC
	arbUSDC, fake := mockToken(t, arb, dir, "USD Coin", "USDC", 6), mockToken(t, arb, dir, "Totally Fine", "FINE", 18)
	if err := base.rpc(ctx, base.RPCURL, "anvil_setBalance", []any{holder, "0x" + units(2, 18).Text(16)}, nil); err != nil {
		t.Fatal(err)
	}
	mint(t, base, usdc, holder, big.NewInt(125_500000))
	mint(t, base, eurc, holder, units(40, 6))
	mint(t, base, usdt, holder, units(10, 6))
	mint(t, base, widget, holder, units(7, 18))
	mint(t, base, lure, holder, units(1000, 6))
	mint(t, arb, arbUSDC, holder, big.NewInt(4_500000))
	mint(t, arb, fake, holder, units(99, 18))

	eth := func(n string) holdings.Token {
		return holdings.Token{Network: n, Symbol: "ETH", Name: "Ether", Decimals: 18}
	}
	list := holdings.List{Tokens: []holdings.Token{
		eth("base"),
		{Network: "base", Address: usdc, Symbol: "USDC", Name: "USD Coin", Decimals: 6},
		{Network: "base", Address: eurc, Symbol: "EURC", Name: "Euro Coin", Decimals: 6},
		{Network: "base", Address: usdt, Symbol: "USDT", Name: "Tether USD", Decimals: 6, NotIn: []string{"EU"}},
		eth("arbitrum"),
		{Network: "arbitrum", Address: arbUSDC, Symbol: "USDC", Name: "USD Coin", Decimals: 6},
	}, Spam: map[string]bool{holdings.Key("arbitrum", fake): true}}
	read := func(addr string) map[string][]holdings.Held {
		held := map[string][]holdings.Held{}
		for n, c := range map[string]Chain{"base": base, "arbitrum": arb} {
			hs, err := c.Holdings(ctx, addr)
			if err != nil {
				t.Fatal(err)
			}
			held[n] = hs
		}
		return held
	}

	st, err := list.Statement(ctx, "GB", "GBP", read(holder), holdings.TestPrices{})
	if err != nil {
		t.Fatal(err)
	}
	// At the test prices in pounds (a dollar is 80p): 2 ETH £4,800.00, 125.50 USDC £100.40, 40 EURC £35.20,
	// 10 USDT £8.00 on Base; 4.50 USDC £3.60 on Arbitrum.
	if len(st.Networks) != 2 || st.Networks[0].ID != "base" || !st.Networks[0].Default || st.Networks[1].ID != "arbitrum" {
		t.Fatalf("networks: %+v", st.Networks)
	}
	want := map[string]map[string]int64{"base": {"ETH": 480000, "USDC": 10040, "EURC": 3520, "USDT": 800}, "arbitrum": {"ETH": 0, "USDC": 360}}
	var total int64
	for _, nb := range st.Networks {
		var sum int64
		for _, b := range nb.Tokens {
			if b.ValueMinor == nil || *b.ValueMinor != want[nb.ID][b.Symbol] {
				t.Errorf("%s %s: value %v, want %d", nb.ID, b.Symbol, b.ValueMinor, want[nb.ID][b.Symbol])
				continue
			}
			sum += *b.ValueMinor
		}
		if len(nb.Tokens) != len(want[nb.ID]) || nb.ValueMinor != sum {
			t.Errorf("%s: %d tokens worth %d, want %d worth their sum %d", nb.ID, len(nb.Tokens), nb.ValueMinor, len(want[nb.ID]), sum)
		}
		total += nb.ValueMinor
	}
	if st.ValueMinor != total || total != 494720 {
		t.Errorf("total %d, networks add up to %d, want 494720", st.ValueMinor, total)
	}
	if b := find(st.Networks[0].Tokens, "USDC"); b == nil || b.Amount != "125500000" || b.Address != usdc {
		t.Errorf("Base USDC: %+v", b)
	}
	// The token off the list is under Other with no value; the lure that calls itself USDC and the listed spam are not
	// shown anywhere.
	other := st.Networks[0].Other
	if len(other) != 1 || other[0].Address != widget || other[0].Symbol != "WDGT" || other[0].Amount != units(7, 18).String() || other[0].ValueMinor != nil {
		t.Errorf("Base Other: %+v", other)
	}
	if len(st.Networks[1].Other) != 0 {
		t.Errorf("Arbitrum Other shows the listed spam token: %+v", st.Networks[1].Other)
	}

	// An owner in Germany is not offered USDT: an empty wallet does not show it at all, one holding some shows it under
	// Other, still valued, rather than hide their money. An owner in the US is offered it.
	for _, c := range []struct {
		country, addr string
		offered       bool
	}{{"DE", empty, false}, {"US", empty, true}, {"DE", holder, false}} {
		st, err := list.Statement(ctx, c.country, holdings.CurrencyFor(c.country), read(c.addr), holdings.TestPrices{})
		if err != nil {
			t.Fatal(err)
		}
		inTokens, inOther := find(st.Networks[0].Tokens, "USDT") != nil, find(st.Networks[0].Other, "USDT")
		if inTokens != c.offered || (c.addr == empty && inOther != nil) || (c.addr == holder && (inOther == nil || inOther.ValueMinor == nil)) {
			t.Errorf("%s %s: USDT offered %v, under Other %+v", c.country, c.addr, inTokens, inOther)
		}
	}
}
