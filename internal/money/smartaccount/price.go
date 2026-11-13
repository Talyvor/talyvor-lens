package smartaccount

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// price.go — B54.26: the independent price an ERC-20 paymaster's is bounded by (Chain.CoinPrice), read from price
// feeds on the chain itself, each refused once it is older than the chain's setting.

// PriceFeeds names a chain's price feeds — aggregators answering latestRoundData() and decimals(), as Chainlink's do —
// of its coin in USD and of each token in USD, by the token's contract.
type PriceFeeds struct {
	CoinUSD   string            `json:"coin_usd"`
	TokensUSD map[string]string `json:"tokens_usd"`
	// MaxAgeSeconds is Nick's setting: a price last updated longer ago is not taken. At 0 none is.
	MaxAgeSeconds int64 `json:"max_age_seconds"`
}

// FeedPrice is a CoinPrice read from f on c: the token's base units per 1e18 wei, the coin's price over the token's.
func (c Chain) FeedPrice(f PriceFeeds) func(ctx context.Context, token string) (*big.Rat, error) {
	return func(ctx context.Context, token string) (*big.Rat, error) {
		var feed string
		for t, a := range f.TokensUSD {
			if strings.EqualFold(t, token) {
				feed = a
			}
		}
		if f.CoinUSD == "" || feed == "" {
			return nil, fmt.Errorf("%w: chain %d has no price feed of its coin and of %s", ErrInvalid, c.ID, token)
		}
		coin, err := c.feedUSD(ctx, f.CoinUSD, f.MaxAgeSeconds)
		if err != nil {
			return nil, err
		}
		usd, err := c.feedUSD(ctx, feed, f.MaxAgeSeconds)
		if err != nil {
			return nil, err
		}
		decimals, err := c.uint8Call(ctx, token, "decimals()")
		if err != nil {
			return nil, err
		}
		price := new(big.Rat).Quo(coin, usd)
		return price.Mul(price, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(decimals), nil))), nil
	}
}

// feedUSD is feed's latest answer, refused when it is not above 0 or is older than maxAge seconds.
func (c Chain) feedUSD(ctx context.Context, feed string, maxAge int64) (*big.Rat, error) {
	out, err := c.ethCallData(ctx, feed, selector("latestRoundData()"))
	if err != nil {
		return nil, err
	}
	if len(out) != 5*32 {
		return nil, fmt.Errorf("smartaccount: the price feed %s answered %d bytes", feed, len(out))
	}
	answer, updated := new(big.Int).SetBytes(out[32:64]), new(big.Int).SetBytes(out[96:128])
	if out[32]&0x80 != 0 || answer.Sign() == 0 { // an int256 at or below 0
		return nil, fmt.Errorf("smartaccount: the price feed %s answers no price", feed)
	}
	if age := time.Since(time.Unix(updated.Int64(), 0)); maxAge <= 0 || !updated.IsInt64() || age > time.Duration(maxAge)*time.Second {
		return nil, fmt.Errorf("smartaccount: the price feed %s was last updated %s ago; a price is taken for %ds", feed, age.Round(time.Second), maxAge)
	}
	decimals, err := c.uint8Call(ctx, feed, "decimals()")
	if err != nil {
		return nil, err
	}
	return new(big.Rat).SetFrac(answer, new(big.Int).Exp(big.NewInt(10), big.NewInt(decimals), nil)), nil
}

// uint8Call is to's answer to fn, a uint8 such as decimals().
func (c Chain) uint8Call(ctx context.Context, to, fn string) (int64, error) {
	out, err := c.ethCallData(ctx, to, selector(fn))
	if err != nil {
		return 0, err
	}
	n := new(big.Int).SetBytes(out)
	if len(out) != 32 || n.Cmp(big.NewInt(255)) > 0 {
		return 0, fmt.Errorf("smartaccount: %s answered %x to %s", to, out, fn)
	}
	return n.Int64(), nil
}
