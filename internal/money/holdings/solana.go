package holdings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"time"
)

// Solana reads what a Solana address holds from a node's JSON-RPC (solana.com/docs/rpc): its SOL by getBalance and its
// tokens by getTokenAccountsByOwner under both token programs.
type Solana struct {
	RPCURL string
	HTTP   *http.Client
}

var (
	solanaAddress = regexp.MustCompile(`^[1-9A-HJ-NP-Za-km-z]{32,44}$`)
	tokenPrograms = []string{"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA", "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"}
)

func (s Solana) rpc(ctx context.Context, method string, params []any, out any) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.RPCURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("holdings: the Solana node's URL") // it carries the node's key: never in an error
	}
	req.Header.Set("Content-Type", "application/json")
	h := s.HTTP
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := h.Do(req)
	if err != nil {
		return fmt.Errorf("holdings: %s: the Solana node did not answer", method)
	}
	defer resp.Body.Close()
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&r); err != nil {
		return fmt.Errorf("holdings: %s answered %d", method, resp.StatusCode)
	}
	if r.Error != nil {
		return fmt.Errorf("holdings: %s: %d %s", method, r.Error.Code, r.Error.Message)
	}
	return json.Unmarshal(r.Result, out)
}

// Holdings is address's SOL (Address "") and each mint it holds tokens of.
func (s Solana) Holdings(ctx context.Context, address string) ([]Held, error) {
	if !solanaAddress.MatchString(address) {
		return nil, fmt.Errorf("holdings: %q is not a Solana address", address)
	}
	var bal struct {
		Value uint64 `json:"value"`
	}
	if err := s.rpc(ctx, "getBalance", []any{address}, &bal); err != nil {
		return nil, err
	}
	out := []Held{{Address: "", Amount: new(big.Int).SetUint64(bal.Value), Decimals: 9}}
	for _, program := range tokenPrograms {
		var accts struct {
			Value []struct {
				Account struct {
					Data struct {
						Parsed struct {
							Info struct {
								Mint        string `json:"mint"`
								TokenAmount struct {
									Amount   string `json:"amount"`
									Decimals int    `json:"decimals"`
								} `json:"tokenAmount"`
							} `json:"info"`
						} `json:"parsed"`
					} `json:"data"`
				} `json:"account"`
			} `json:"value"`
		}
		if err := s.rpc(ctx, "getTokenAccountsByOwner", []any{address, map[string]string{"programId": program},
			map[string]string{"encoding": "jsonParsed"}}, &accts); err != nil {
			return nil, err
		}
		for _, a := range accts.Value {
			info := a.Account.Data.Parsed.Info
			n, ok := new(big.Int).SetString(info.TokenAmount.Amount, 10)
			if !ok {
				return nil, fmt.Errorf("holdings: getTokenAccountsByOwner answered amount %q", info.TokenAmount.Amount)
			}
			// shortcut: an unlisted mint has no symbol or name here (they live in its metadata account); read the
			// Metaplex metadata when Other should name Solana tokens.
			out = append(out, Held{Address: info.Mint, Decimals: info.TokenAmount.Decimals, Amount: n})
		}
	}
	return out, nil
}
