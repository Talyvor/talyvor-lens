package smartaccount

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// B54.26 — the independent price is read from the chain's price feeds: ETH at $3,000 and USDC at $1 is 3,000 USDC
// (6 decimals) per 1e18 wei; a price older than the chain's setting, a chain whose setting is unset and a feed answering
// no price are refused.
func TestFeedPriceIsFreshOrRefused(t *testing.T) {
	const ethFeed, usdcFeed = "0x00000000000000000000000000000000000E7400", "0x00000000000000000000000000000000000C0100"
	var updated time.Time
	ethAnswer := big.NewInt(3_000_00000000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params []json.RawMessage `json:"params"`
		}
		var tx struct{ To, Data string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.Unmarshal(req.Params[0], &tx)
		var out []byte
		switch {
		case tx.Data == hexBytes(selector("decimals()")) && strings.EqualFold(tx.To, usdc):
			out = uintW(6).enc()
		case tx.Data == hexBytes(selector("decimals()")):
			out = uintW(8).enc()
		case strings.EqualFold(tx.To, ethFeed):
			out = tuple{uintW(1), bigW(ethAnswer), uintW(0), uintW(uint64(updated.Unix())), uintW(1)}.enc()
		default:
			out = tuple{uintW(1), uintW(1_00000000), uintW(0), uintW(uint64(updated.Unix())), uintW(1)}.enc()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": hexBytes(out)})
	}))
	t.Cleanup(srv.Close)
	c := Chain{ID: 8453, RPCURL: srv.URL}
	feeds := PriceFeeds{CoinUSD: ethFeed, TokensUSD: map[string]string{strings.ToLower(usdc): usdcFeed}, MaxAgeSeconds: 3600}
	price := func(f PriceFeeds) (*big.Rat, error) { return c.FeedPrice(f)(context.Background(), usdc) }

	updated = time.Now().Add(-time.Minute)
	if p, err := price(feeds); err != nil || p.Cmp(big.NewRat(3_000_000_000, 1)) != 0 {
		t.Fatalf("a fresh price: %v, %v; want 3000000000 USDC units per 1e18 wei", p, err)
	}
	updated = time.Now().Add(-61 * time.Minute)
	if _, err := price(feeds); err == nil || !strings.Contains(err.Error(), "last updated") {
		t.Fatalf("a price older than the setting: %v, want refused", err)
	}
	updated = time.Now().Add(-time.Minute)
	unset := feeds
	unset.MaxAgeSeconds = 0
	if _, err := price(unset); err == nil {
		t.Fatal("a price taken with no max age set")
	}
	ethAnswer = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)) // -1 as an int256
	if _, err := price(feeds); err == nil || !strings.Contains(err.Error(), "no price") {
		t.Fatalf("a feed answering below 0: %v, want refused", err)
	}
	if _, err := c.FeedPrice(PriceFeeds{CoinUSD: ethFeed, MaxAgeSeconds: 3600})(context.Background(), usdc); err == nil {
		t.Fatal("a token with no feed was priced")
	}
}
