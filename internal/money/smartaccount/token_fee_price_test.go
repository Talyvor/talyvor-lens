package smartaccount

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// B54.12 — Lens reads the price only in a paymasterAndData layout it knows, and takes a fee in a token only at a price
// within the chain's spread of an independent price of its coin.
func TestSponsorInTokenBoundsThePaymastersPrice(t *testing.T) {
	const paymaster = "0x000000000000000000000000000000000000FEE0"
	priced := func(price int64) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pw, _ := addressW(paymaster)
			tw, _ := addressW(usdc)
			data := append(append(pw[12:], tuple{uintW(0), uintW(0), tw, uintW(uint64(price))}.enc()...), make([]byte, 65)...)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]string{"paymasterAndData": hexBytes(data)}})
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	// The independent price: 3,000 USDC per ETH, in USDC base units per 1e18 wei.
	independent := func(context.Context, string) (*big.Rat, error) { return big.NewRat(3_000_000_000, 1), nil }
	sponsor := func(c Chain) (*big.Int, error) {
		op := UserOp{CallData: transfer(t, usdc, payee, 250_000), CallGasLimit: callGas, VerificationGasLimit: verificationGas,
			PreVerificationGas: preVerification, MaxFeePerGas: big.NewInt(2e9)}
		return c.SponsorInToken(context.Background(), &op, usdc)
	}
	c := Chain{ID: 1, TokenPaymaster: paymaster, TokenPaymasterLayout: LayoutSignedPrice, CoinPrice: independent, TokenPriceSpreadBps: 500}

	c.PaymasterURL = priced(3_150_000_000) // 5% over: inside the spread
	if fee, err := sponsor(c); err != nil || fee.Sign() <= 0 {
		t.Fatalf("a price inside the spread: %v, %v", fee, err)
	}
	c.PaymasterURL = priced(3_150_000_001)
	if _, err := sponsor(c); err == nil || !strings.Contains(err.Error(), "spread") {
		t.Fatalf("a price beyond the spread: %v, want refused", err)
	}
	noPrice := c
	noPrice.CoinPrice = nil
	if _, err := sponsor(noPrice); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "independent price") {
		t.Fatalf("a chain with no independent price: %v, want refused", err)
	}
	for _, layout := range []string{"", "pimlico-erc20"} {
		other := c
		other.TokenPaymasterLayout = layout
		if _, err := sponsor(other); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), `"`+layout+`"`) {
			t.Fatalf("a paymaster whose layout Lens does not read (%q): %v, want refused by name", layout, err)
		}
	}
}
