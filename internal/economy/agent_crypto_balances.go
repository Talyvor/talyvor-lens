package economy

import (
	"context"
	"fmt"

	"github.com/talyvor/lens/internal/money"
	tokens "github.com/talyvor/lens/internal/money/holdings"
)

// agent_crypto_balances.go — B48.4: THE NETWORKS AND TOKENS A FULL WALLET HOLDS. An agent's smart account is at the
// same address on every EVM network and its Solana wallet beside it (B48.2); each network's node says what they hold,
// and internal/money/holdings turns that into balances per token and network, valued in the owner's currency.

type cryptoHoldings struct {
	readers map[string]tokens.Reader
	list    tokens.List
	prices  tokens.Prices
}

// SetCryptoHoldings reads agents' balances from readers, one per network id in tokens.Networks, shown from list and
// valued by prices.
func (s *DualTokenStore) SetCryptoHoldings(readers map[string]tokens.Reader, list tokens.List, prices tokens.Prices) {
	s.cryptoHoldings = &cryptoHoldings{readers: readers, list: list, prices: prices}
}

// AgentCryptoBalances is what agentID's crypto wallet holds on every network, valued in currency — "" for the one
// where its owner is verified — and offered as that country allows.
func (s *DualTokenStore) AgentCryptoBalances(ctx context.Context, workspaceID, agentID, currency string) (tokens.Statement, error) {
	ch := s.cryptoHoldings
	if ch == nil {
		return tokens.Statement{}, ErrSmartAccountsOff
	}
	a, err := s.AgentSmartAccount(ctx, workspaceID, agentID)
	if err != nil {
		return tokens.Statement{}, err
	}
	country, err := ownerCountry(ctx, s.pool, workspaceID)
	if err != nil {
		return tokens.Statement{}, err
	}
	if currency == "" {
		currency = tokens.CurrencyFor(country)
	}
	held := map[string][]tokens.Held{}
	for network, r := range ch.readers {
		addr := a.Address
		if network == "solana" {
			addr = a.SolanaAddress
		}
		if addr == "" {
			continue
		}
		hs, err := r.Holdings(ctx, addr)
		if err != nil {
			return tokens.Statement{}, fmt.Errorf("%w: %v", money.ErrPartnerDown, err)
		}
		held[network] = hs
	}
	return ch.list.Statement(ctx, country, currency, held, ch.prices)
}
