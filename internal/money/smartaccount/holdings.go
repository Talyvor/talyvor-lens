package smartaccount

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/talyvor/lens/internal/money/holdings"
)

// holdings.go — B48.4: what an account holds on an EVM chain — its coin, and every ERC-20 token anyone sent it.

var transferTopic = keccak([]byte("Transfer(address,address,uint256)"))

// Holdings is what address holds on the chain: its coin (Address "") and each ERC-20 token it was sent, with what the
// token says of itself (holdings.List decides whether to believe it).
func (c Chain) Holdings(ctx context.Context, address string) ([]holdings.Held, error) {
	w, err := addressW(address)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	coin, err := c.quantity(ctx, "eth_getBalance", []any{address, "latest"})
	if err != nil {
		return nil, err
	}
	out := []holdings.Held{{Address: "", Decimals: 18, Amount: coin}}
	var logs []struct {
		Address string   `json:"address"`
		Topics  []string `json:"topics"`
	}
	// shortcut: one eth_getLogs from the first block; a live node caps the range it searches, so read the provider's
	// token balances instead when its keys are on the server.
	filter := map[string]any{"fromBlock": "0x0", "toBlock": "latest", "topics": []any{hexBytes(transferTopic[:]), nil, hexBytes(w[:])}}
	if err := c.rpc(ctx, c.RPCURL, "eth_getLogs", []any{filter}, &logs); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, l := range logs {
		token := Checksum(l.Address)
		if len(l.Topics) != 3 || token == "" || seen[token] { // an NFT's Transfer indexes its token id as a fourth topic
			continue
		}
		seen[token] = true
		bal, err := c.ethCallData(ctx, token, call("balanceOf(address)", w))
		if err != nil || len(bal) != 32 {
			continue // not an ERC-20 after all
		}
		h := holdings.Held{Address: token, Amount: new(big.Int).SetBytes(bal)}
		if d, err := c.ethCallData(ctx, token, selector("decimals()")); err == nil && len(d) == 32 {
			if n := new(big.Int).SetBytes(d); n.Cmp(big.NewInt(36)) <= 0 {
				h.Decimals = int(n.Int64())
			}
		}
		h.Symbol = c.tokenText(ctx, token, "symbol()")
		h.Name = c.tokenText(ctx, token, "name()")
		out = append(out, h)
	}
	return out, nil
}

func (c Chain) ethCallData(ctx context.Context, to string, data []byte) ([]byte, error) {
	var s string
	if err := c.rpc(ctx, c.RPCURL, "eth_call", []any{map[string]string{"to": to, "data": hexBytes(data)}, "latest"}, &s); err != nil {
		return nil, err
	}
	return hex.DecodeString(strings.TrimPrefix(s, "0x"))
}

// tokenText is a token's symbol() or name(): an ABI string, or the bytes32 older tokens answer; "" when neither.
func (c Chain) tokenText(ctx context.Context, token, fn string) string {
	b, err := c.ethCallData(ctx, token, selector(fn))
	if err != nil {
		return ""
	}
	return abiText(b)
}

// abiText is an ABI string, or a bytes32, as text of at most 64 bytes; "" when b is neither. b is whatever a token
// chose to answer.
func abiText(b []byte) string {
	var s []byte
	switch {
	case len(b) == 32:
		s = bytes.TrimRight(b, "\x00")
	case len(b) >= 64:
		off, n := new(big.Int).SetBytes(b[:32]), new(big.Int).SetBytes(b[32:64])
		if off.Cmp(big.NewInt(32)) != 0 || n.Cmp(big.NewInt(int64(len(b)-64))) > 0 { // compared as big: 64+n overflows
			return ""
		}
		s = b[64 : 64+n.Int64()]
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.ToValidUTF8(string(s), "")
}
