// Package holdings is B48.4: THE NETWORKS AND TOKENS A FULL WALLET HOLDS.
//
// An agent's crypto wallet holds tokens on Base (the default), Ethereum, Arbitrum, Optimism, Polygon and Solana: USDC,
// EURC and each network's own coin, and anything else sent to it. What a token is comes only from the verified list
// (its contract or mint, decimals and logo) — never from what a token says about itself — so a token off the list is
// shown under Other with no value, and a known spam token is not shown at all. A token not allowed where the owner
// lives is not offered to them. Each balance is valued in the owner's currency through a price adapter.
package holdings

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// Network is one chain a wallet holds tokens on.
type Network struct {
	ID      string `json:"network"`
	Name    string `json:"name"`
	ChainID int64  `json:"chain_id,omitempty"` // EVM networks; 0 on Solana
}

// defaultNetwork is where a wallet receives and pays unless told otherwise.
const defaultNetwork = "base"

// Networks are the networks a wallet holds tokens on, the default first.
var Networks = []Network{
	{"base", "Base", 8453}, {"ethereum", "Ethereum", 1}, {"arbitrum", "Arbitrum", 42161},
	{"optimism", "Optimism", 10}, {"polygon", "Polygon", 137}, {"solana", "Solana", 0},
}

// Token is one token on one network. Address is its contract or mint, "" for the network's own coin.
type Token struct {
	Network  string `json:"network"`
	Address  string `json:"address"`
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Decimals int    `json:"decimals"`
	Logo     string `json:"logo,omitempty"`
	// NotIn are the countries (ISO 3166-1 alpha-2, or "EU" for every member state) it is not offered in.
	NotIn []string `json:"-"`
}

// List is a verified token list and the tokens known to be spam.
type List struct {
	Tokens []Token
	Spam   map[string]bool // Key(network, address)
}

// Key names a token on a network: an EVM address in lower case (its mixed case is only a checksum), a Solana mint as
// it is (base58 is case-sensitive).
func Key(network, address string) string {
	if strings.HasPrefix(address, "0x") {
		address = strings.ToLower(address)
	}
	return network + ":" + address
}

const logos = "https://raw.githubusercontent.com/trustwallet/assets/master/blockchains/"

var (
	eth  = func(n string) Token { return Token{n, "", "ETH", "Ether", 18, logos + "ethereum/info/logo.png", nil} }
	usdc = func(n, addr, chain string) Token {
		return Token{n, addr, "USDC", "USD Coin", 6, logos + chain + "/assets/" + addr + "/logo.png", nil}
	}
	eurc = func(n, addr, chain string) Token {
		return Token{n, addr, "EURC", "Euro Coin", 6, logos + chain + "/assets/" + addr + "/logo.png", nil}
	}
	// USDT is not an e-money token authorised under MiCA, so it is not offered to EU residents.
	usdt = func(n, addr, chain string) Token {
		return Token{n, addr, "USDT", "Tether USD", 6, logos + chain + "/assets/" + addr + "/logo.png", []string{"EU"}}
	}
)

// Verified is the verified token list: each network's coin and the issuers' own contracts (circle.com and tether.to
// publish them).
func Verified() List {
	return List{Tokens: []Token{
		eth("base"), usdc("base", "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", "base"),
		eurc("base", "0x60a3E35Cc302bFA44Cb288Bc5a4F316Fdb1adb42", "base"),
		eth("ethereum"), usdc("ethereum", "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48", "ethereum"),
		eurc("ethereum", "0x1aBaEA1f7C830bD89Acc67eC4af516284b1bC33c", "ethereum"),
		usdt("ethereum", "0xdAC17F958D2ee523a2206206994597C13D831ec7", "ethereum"),
		eth("arbitrum"), usdc("arbitrum", "0xaf88d065e77c8cC2239327C5EDb3A432268e5831", "arbitrum"),
		usdt("arbitrum", "0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9", "arbitrum"),
		eth("optimism"), usdc("optimism", "0x0b2C639c533813f4Aa9D7837CAf62653d097Ff85", "optimism"),
		usdt("optimism", "0x94b008aA00579c1307B0EF2c499aD98a8ce58e58", "optimism"),
		{"polygon", "", "POL", "Polygon Ecosystem Token", 18, logos + "polygon/info/logo.png", nil},
		usdc("polygon", "0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359", "polygon"),
		usdt("polygon", "0xc2132D05D31c914a87C6611C10748AEb04B58e8F", "polygon"),
		{"solana", "", "SOL", "Solana", 9, logos + "solana/info/logo.png", nil},
		usdc("solana", "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", "solana"),
		eurc("solana", "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr", "solana"),
		usdt("solana", "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", "solana"),
	}, Spam: map[string]bool{}}
}

// spamName is how unsolicited airdrops advertise themselves: a link or a call to claim, in the token's own name.
var spamName = regexp.MustCompile(`(?i)https?:|www\.|\.(com|io|xyz|org|net|app|site|top|finance)\b|claim|visit|reward|airdrop|voucher`)

var eu = map[string]bool{"AT": true, "BE": true, "BG": true, "HR": true, "CY": true, "CZ": true, "DK": true, "EE": true,
	"FI": true, "FR": true, "DE": true, "GR": true, "HU": true, "IE": true, "IT": true, "LV": true, "LT": true, "LU": true,
	"MT": true, "NL": true, "PL": true, "PT": true, "RO": true, "SK": true, "SI": true, "ES": true, "SE": true}

// offered is whether t is offered to an owner in country; a token restricted anywhere is not offered while the
// owner's country is not yet known.
func (t Token) offered(country string) bool {
	for _, c := range t.NotIn {
		if country == "" || c == country || (c == "EU" && eu[country]) {
			return false
		}
	}
	return true
}

// Held is a token a wallet holds on one network, as the network reports it. Symbol, Name and Decimals are what the
// token says of itself, which is trusted only when it is not on the list.
type Held struct {
	Address  string
	Symbol   string
	Name     string
	Decimals int
	Amount   *big.Int
}

// Reader reads what an address holds on one network.
type Reader interface {
	Holdings(ctx context.Context, address string) ([]Held, error)
}

// Prices is the price adapter: what one whole token of a verified symbol is worth in currency, in whole units.
type Prices interface {
	Price(ctx context.Context, symbol, currency string) (*big.Rat, error)
}

var (
	// ErrNoPrice: the adapter has no price for that token in that currency.
	ErrNoPrice = errors.New("holdings: no price")
	// ErrCurrency: balances are not valued in that currency.
	ErrCurrency = errors.New("holdings: balances are valued in GBP, EUR or USD")
)

// Currencies an owner's balances are valued in, and their minor-unit decimals.
var Currencies = map[string]int{"GBP": 2, "EUR": 2, "USD": 2}

// CurrencyFor is the currency an owner in country sees their balances in.
func CurrencyFor(country string) string {
	switch {
	case country == "GB":
		return "GBP"
	case eu[country]:
		return "EUR"
	}
	return "USD"
}

// Balance is how much of a token a wallet holds, in the token's smallest unit, and what it is worth in the owner's
// currency's minor units (null under Other: a token off the list has no price).
type Balance struct {
	Token
	Amount     string `json:"amount"`
	ValueMinor *int64 `json:"value_minor"`
}

// NetworkBalances is what a wallet holds on one network: the tokens offered there, the other tokens it was sent, and
// their value.
type NetworkBalances struct {
	Network
	Default    bool      `json:"default"`
	Tokens     []Balance `json:"tokens"`
	Other      []Balance `json:"other"`
	ValueMinor int64     `json:"value_minor"`
}

// Statement is what a wallet holds on every network it was read on, valued in Currency.
type Statement struct {
	Currency   string            `json:"currency"`
	Country    string            `json:"country"`
	Networks   []NetworkBalances `json:"networks"`
	ValueMinor int64             `json:"value_minor"`
}

// Statement is what held, by network, comes to for an owner in country, valued in currency: networks absent from held
// were not read and are left out.
func (l List) Statement(ctx context.Context, country, currency string, held map[string][]Held, prices Prices) (Statement, error) {
	dp, ok := Currencies[currency]
	if !ok {
		return Statement{}, fmt.Errorf("%w, not %q", ErrCurrency, currency)
	}
	st := Statement{Currency: currency, Country: country, Networks: []NetworkBalances{}}
	for _, n := range Networks {
		hs, read := held[n.ID]
		if !read {
			continue
		}
		amounts := map[string]Held{}
		for _, h := range hs {
			k := Key(n.ID, h.Address)
			if prev, dup := amounts[k]; dup {
				h.Amount = new(big.Int).Add(prev.Amount, h.Amount)
			}
			amounts[k] = h
		}
		nb := NetworkBalances{Network: n, Default: n.ID == defaultNetwork, Tokens: []Balance{}, Other: []Balance{}}
		value := func(t Token, amount *big.Int) (*int64, error) {
			p, err := prices.Price(ctx, t.Symbol, currency)
			if err != nil {
				return nil, fmt.Errorf("holdings: %s on %s: %w", t.Symbol, n.Name, err)
			}
			// minor = amount × price × 10^dp / 10^decimals, rounded down
			v := new(big.Rat).Mul(new(big.Rat).SetInt(amount), p)
			v.Mul(v, new(big.Rat).SetFrac(pow10(dp), pow10(t.Decimals)))
			m := new(big.Int).Quo(v.Num(), v.Denom())
			if !m.IsInt64() {
				return nil, fmt.Errorf("holdings: %s on %s is worth more than a balance can show", t.Symbol, n.Name)
			}
			minor := m.Int64()
			nb.ValueMinor += minor
			return &minor, nil
		}
		for _, t := range l.Tokens {
			if t.Network != n.ID {
				continue
			}
			k := Key(n.ID, t.Address)
			h, has := amounts[k]
			delete(amounts, k)
			if !has {
				h.Amount = new(big.Int)
			}
			if !t.offered(country) {
				if h.Amount.Sign() > 0 { // never hide money the wallet already holds: it is shown, not offered
					v, err := value(t, h.Amount)
					if err != nil {
						return Statement{}, err
					}
					nb.Other = append(nb.Other, Balance{t, h.Amount.String(), v})
				}
				continue
			}
			v, err := value(t, h.Amount)
			if err != nil {
				return Statement{}, err
			}
			nb.Tokens = append(nb.Tokens, Balance{t, h.Amount.String(), v})
		}
		for _, h := range hs { // the rest, in the order the network reported them
			k := Key(n.ID, h.Address)
			h, left := amounts[k]
			delete(amounts, k)
			if !left || h.Amount.Sign() <= 0 || l.Spam[k] || spamName.MatchString(h.Name+" "+h.Symbol) {
				continue
			}
			nb.Other = append(nb.Other, Balance{Token{Network: n.ID, Address: h.Address, Symbol: h.Symbol, Name: h.Name, Decimals: h.Decimals}, h.Amount.String(), nil})
		}
		st.ValueMinor += nb.ValueMinor
		st.Networks = append(st.Networks, nb)
	}
	return st, nil
}

func pow10(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

// TestPrices is the price adapter in tests: fixed test figures, never a market price.
type TestPrices struct{}

var testUSD = map[string]*big.Rat{"USDC": big.NewRat(1, 1), "USDT": big.NewRat(1, 1), "EURC": big.NewRat(11, 10),
	"ETH": big.NewRat(3000, 1), "POL": big.NewRat(1, 2), "SOL": big.NewRat(150, 1)}
var testUSDPerCurrency = map[string]*big.Rat{"USD": big.NewRat(1, 1), "EUR": big.NewRat(11, 10), "GBP": big.NewRat(5, 4)}

// Price is symbol's test figure in currency.
func (TestPrices) Price(_ context.Context, symbol, currency string) (*big.Rat, error) {
	usd, per := testUSD[symbol], testUSDPerCurrency[currency]
	if usd == nil || per == nil {
		return nil, fmt.Errorf("%w: %s in %s", ErrNoPrice, symbol, currency)
	}
	return new(big.Rat).Quo(usd, per), nil
}
