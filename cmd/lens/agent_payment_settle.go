package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money/smartaccount"
)

// agent_payment_settle.go — B54.26: agents' smart-account payments settled from their operations' receipts. The leader
// settles every chain LENS_AGENT_CHAINS names each minute; unset, nothing runs.

// agentChainConfig is one chain of LENS_AGENT_CHAINS.
type agentChainConfig struct {
	ChainID              int64                   `json:"chain_id"`
	RPCURL               string                  `json:"rpc_url"`
	BundlerURL           string                  `json:"bundler_url"`
	PaymasterURL         string                  `json:"paymaster_url"`
	TokenPaymaster       string                  `json:"token_paymaster"`
	TokenPaymasterLayout string                  `json:"token_paymaster_layout"`
	TokenPriceSpreadBps  int64                   `json:"token_price_spread_bps"` // Nick's setting
	PriceFeeds           smartaccount.PriceFeeds `json:"price_feeds"`
}

// agentChains reads LENS_AGENT_CHAINS's raw JSON; none when it is unset.
func agentChains(raw string) ([]smartaccount.Chain, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var cfg []agentChainConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("LENS_AGENT_CHAINS: %w", err)
	}
	chains := make([]smartaccount.Chain, 0, len(cfg))
	for _, n := range cfg {
		if n.ChainID <= 0 || n.RPCURL == "" || n.BundlerURL == "" {
			return nil, fmt.Errorf("LENS_AGENT_CHAINS: each chain names its chain_id, rpc_url and bundler_url")
		}
		if n.TokenPriceSpreadBps < 0 || n.TokenPriceSpreadBps > 10_000 || n.PriceFeeds.MaxAgeSeconds < 0 {
			return nil, fmt.Errorf("LENS_AGENT_CHAINS: chain %d's token_price_spread_bps is 0 to 10000 and its max_age_seconds not below 0", n.ChainID)
		}
		c := smartaccount.Chain{ID: n.ChainID, RPCURL: n.RPCURL, BundlerURL: n.BundlerURL, PaymasterURL: n.PaymasterURL,
			TokenPaymaster: n.TokenPaymaster, TokenPaymasterLayout: n.TokenPaymasterLayout, TokenPriceSpreadBps: n.TokenPriceSpreadBps}
		c.CoinPrice = c.FeedPrice(n.PriceFeeds)
		chains = append(chains, c)
	}
	return chains, nil
}

// settleAgentPayments settles each chain's sent payments from the chain every minute.
func settleAgentPayments(ctx context.Context, store *economy.DualTokenStore, chains []smartaccount.Chain) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		for _, c := range chains {
			if _, err := store.SettleAgentCryptoPayments(ctx, c); err != nil {
				slog.Warn("agent payments: some were not settled from the chain; the next run tries again", "chain", c.ID, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
