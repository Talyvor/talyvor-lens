package smartaccount

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// transfers.go — B48.6: what a node says of the token transfers into and out of agents' accounts, so each movement is
// seen (TransferTxs, the fallback to the provider's webhooks), confirmed or found reorged out (TxTransfers), and every
// account's balance read back for reconciliation (BalanceOf).
//
// A transfer is known by its transaction and its place among that transaction's logs: unlike a log's index in its
// block, that place stays the same when a reorg puts the transaction in another block.

// TransferTopic is the ERC-20 Transfer(address,address,uint256) event's topic.
const TransferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

// Transfer is one ERC-20 Transfer log.
type Transfer struct {
	TxHash      string   // 0x, lower case
	Position    int      // its place among the transaction's logs
	BlockNumber int64    //
	BlockHash   string   // 0x, lower case
	Token       string   // EIP-55
	From, To    string   // EIP-55
	Value       *big.Int // in the token's smallest unit
}

type rpcLog struct {
	Address string   `json:"address"`
	Topics  []string `json:"topics"`
	Data    string   `json:"data"`
	TxHash  string   `json:"transactionHash"`
	Removed bool     `json:"removed"`
}

// transfer reads l as a Transfer, but for where it is; ok is false when it is not one.
func (l rpcLog) transfer() (Transfer, bool) {
	if len(l.Topics) != 3 || !strings.EqualFold(l.Topics[0], TransferTopic) || l.Removed {
		return Transfer{}, false
	}
	from, err1 := hex.DecodeString(strings.TrimPrefix(l.Topics[1], "0x"))
	to, err2 := hex.DecodeString(strings.TrimPrefix(l.Topics[2], "0x"))
	v, ok := new(big.Int).SetString(strings.TrimPrefix(l.Data, "0x"), 16)
	if err1 != nil || err2 != nil || !ok || len(from) != 32 || len(to) != 32 {
		return Transfer{}, false
	}
	return Transfer{Token: Checksum(l.Address), From: checksum(from[12:]), To: checksum(to[12:]), Value: v}, true
}

// Head is the chain's latest block number.
func (c Chain) Head(ctx context.Context) (int64, error) {
	n, err := c.quantity(ctx, "eth_blockNumber", nil)
	if err != nil {
		return 0, err
	}
	return n.Int64(), nil
}

// TransferTxs is every transaction with a Transfer of token into or out of one of accounts from block from to block to,
// inclusive, each once.
func (c Chain) TransferTxs(ctx context.Context, token string, accounts []string, from, to int64) ([]string, error) {
	if len(accounts) == 0 || from > to {
		return nil, nil
	}
	topics := make([]string, 0, len(accounts))
	for _, a := range accounts {
		w, err := addressW(a)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		topics = append(topics, hexBytes(w[:]))
	}
	var out []string
	seen := map[string]bool{}
	// Two asks, as a node filters each topic by itself: the transfers into the accounts, then those out of them.
	for _, filter := range [][]any{{TransferTopic, nil, topics}, {TransferTopic, topics}} {
		var logs []rpcLog
		if err := c.rpc(ctx, c.RPCURL, "eth_getLogs", []any{map[string]any{"address": token, "topics": filter,
			"fromBlock": quantity(big.NewInt(from)), "toBlock": quantity(big.NewInt(to))}}, &logs); err != nil {
			return nil, err
		}
		for _, l := range logs {
			if h := strings.ToLower(l.TxHash); !l.Removed && !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out, nil
}

// TxTransfers is every Transfer in transaction txHash as the chain has it now; mined is false when the transaction is
// not on the chain — never mined, or reorged out — or failed.
func (c Chain) TxTransfers(ctx context.Context, txHash string) (ts []Transfer, mined bool, err error) {
	var r *struct {
		Status      string   `json:"status"`
		BlockNumber string   `json:"blockNumber"`
		BlockHash   string   `json:"blockHash"`
		Logs        []rpcLog `json:"logs"`
	}
	if err := c.rpc(ctx, c.RPCURL, "eth_getTransactionReceipt", []any{txHash}, &r); err != nil {
		return nil, false, err
	}
	if r == nil || r.Status != "0x1" {
		return nil, false, nil
	}
	block, err := strconv.ParseInt(strings.TrimPrefix(r.BlockNumber, "0x"), 16, 64)
	if err != nil {
		return nil, false, fmt.Errorf("smartaccount: transaction %s's block %q", txHash, r.BlockNumber)
	}
	for i, l := range r.Logs {
		if t, ok := l.transfer(); ok {
			t.TxHash, t.Position, t.BlockNumber, t.BlockHash = strings.ToLower(txHash), i, block, strings.ToLower(r.BlockHash)
			ts = append(ts, t)
		}
	}
	return ts, true, nil
}

// BalanceOf is what account holds of token at block, in the token's smallest unit.
func (c Chain) BalanceOf(ctx context.Context, token, account string, block int64) (*big.Int, error) {
	a, err := addressW(account)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return c.quantity(ctx, "eth_call", []any{map[string]string{"to": token, "data": hexBytes(call("balanceOf(address)", a))},
		quantity(big.NewInt(block))})
}
