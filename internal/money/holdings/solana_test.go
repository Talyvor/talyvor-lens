package holdings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A Solana wallet read from a local fake of the node's JSON-RPC (solana.com/docs/rpc): its SOL and USDC are listed
// and valued, a mint off the list is under Other.
func TestSolanaHoldings(t *testing.T) {
	const owner, unknown = "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU", "9wFFyRfZBsuAha4YcuxcXLKwMxJR43S7fPfQLusDBzvT"
	var usdc Token
	for _, tk := range Verified().Tokens {
		if tk.Network == "solana" && tk.Symbol == "USDC" {
			usdc = tk
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Params[0] != owner {
			t.Errorf("%s asked about %v", req.Method, req.Params[0])
		}
		acct := func(mint, amount string, decimals int) map[string]any {
			return map[string]any{"account": map[string]any{"data": map[string]any{"parsed": map[string]any{"info": map[string]any{
				"mint": mint, "tokenAmount": map[string]any{"amount": amount, "decimals": decimals}}}}}}
		}
		var result any
		switch req.Method {
		case "getBalance":
			result = map[string]any{"value": 2_500_000_000} // 2.5 SOL
		case "getTokenAccountsByOwner":
			value := []any{}
			if req.Params[1].(map[string]any)["programId"] == tokenPrograms[0] {
				value = append(value, acct(usdc.Address, "20000000", 6), acct(unknown, "5", 0))
			}
			result = map[string]any{"value": value}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	defer srv.Close()

	hs, err := Solana{RPCURL: srv.URL}.Holdings(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	st, err := Verified().Statement(context.Background(), "US", "USD", map[string][]Held{"solana": hs}, TestPrices{})
	if err != nil {
		t.Fatal(err)
	}
	// 2.5 SOL at $150 is $375.00, 20 USDC $20.00; EURC is listed at 0; USDT is offered in the US, at 0.
	nb := st.Networks[0]
	got := map[string]int64{}
	for _, b := range nb.Tokens {
		got[b.Symbol] = *b.ValueMinor
	}
	if len(st.Networks) != 1 || len(got) != 4 || got["SOL"] != 37500 || got["USDC"] != 2000 || got["EURC"] != 0 || got["USDT"] != 0 || st.ValueMinor != 39500 {
		t.Errorf("Solana: %v, total %d", got, st.ValueMinor)
	}
	if len(nb.Other) != 1 || nb.Other[0].Address != unknown || nb.Other[0].Amount != "5" || nb.Other[0].ValueMinor != nil {
		t.Errorf("Solana Other: %+v", nb.Other)
	}
}
