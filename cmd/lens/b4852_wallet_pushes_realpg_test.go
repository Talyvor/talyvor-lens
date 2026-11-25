package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// b4852Node is a node that has mined each of txs, one USDC transfer each at block 100, is at block 100, and says
// USDC's EIP-712 domain.
type b4852Node struct {
	token string
	mu    sync.Mutex
	txs   map[string][3]string // hash → from, to, value in micro-USDC as hex
}

func (n *b4852Node) mine(hash, from, to string, value int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.txs[hash] = [3]string{from, to, fmt.Sprintf("0x%064x", value)}
}

func (n *b4852Node) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	pad := func(a string) string {
		return "0x" + strings.Repeat("0", 24) + strings.ToLower(strings.TrimPrefix(a, "0x"))
	}
	abi := func(s string) string {
		return fmt.Sprintf("0x%064x%064x%s", 32, len(s), hex.EncodeToString([]byte(s))+strings.Repeat("00", 32-len(s)%32))
	}
	var result any
	switch req.Method {
	case "eth_blockNumber":
		result = "0x64"
	case "eth_call":
		var call struct{ Data string }
		_ = json.Unmarshal(req.Params[0], &call)
		result = map[string]string{"0x06fdde03": abi("USD Coin"), "0x54fd4d50": abi("2")}[call.Data]
	case "eth_getTransactionReceipt":
		var hash string
		_ = json.Unmarshal(req.Params[0], &hash)
		n.mu.Lock()
		tx, ok := n.txs[hash]
		n.mu.Unlock()
		if ok {
			result = map[string]any{"status": "0x1", "blockNumber": "0x64", "blockHash": "0x" + strings.Repeat("b4", 32), "logs": []map[string]any{{
				"address": n.token, "topics": []string{smartaccount.TransferTopic, pad(tx[0]), pad(tx[1])}, "data": tx[2], "transactionHash": hash}}}
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

// B48.52 — Lens tells the owner's phones when money lands in an agent's wallet, when money leaves it, and when a
// request to be paid is paid in full: once to each phone whose key is on, with the type the phone opens the wallet
// on, the agent, the amount, and the other side's address only shortened. A chain event seen again, or a movement
// confirmed again, tells nobody twice.
func TestB4852_TheOwnersPhonesAreToldOfMoneyInMoneyOutAndARequestPaid(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b4852"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(partners.NewRegistry(nil))
	store.SetScreener(screening.NewScreener(pool, b487Screening{}))
	pushes := make(b468Pushes, 16)
	store.SetNativePush(pushes)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, platform, public_key, attestation, push_token, revoked_at)
		VALUES ('dev_b4852_a', $1, 'test', '\x00', '\x00', 'tok-a', NULL), ('dev_b4852_b', $1, 'test', '\x00', '\x00', 'tok-b', NULL),
		('dev_b4852_off', $1, 'test', '\x00', '\x00', 'tok-off', now())`, ws); err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(ctx, ws, "Treasurer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	wallet := smartaccount.Checksum("0x00000000000000000000000000000000000b4852")
	if _, err := pool.Exec(ctx, `INSERT INTO agent_smart_accounts (agent_id, workspace_id, address, factory, implementation, owner_credential_id,
		owner_public_key, agent_signer, provider, solana_address) VALUES ($1, $2, $3, '0x', '0x', 'cred', $4, '0x', 'cdp', '')`,
		agent.ID, ws, wallet, make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	node := &b4852Node{token: smartaccount.Checksum("0x00000000000000000000000000000000000c0de2"), txs: map[string][3]string{}}
	srv := httptest.NewServer(node)
	defer srv.Close()
	x, err := store.NewChainIndexer([]economy.ChainNetwork{{Name: "base", Chain: smartaccount.Chain{ID: 8453, RPCURL: srv.URL}, USDC: node.token,
		Confirmations: 1, Funding: economy.FundingTest}})
	if err != nil {
		t.Fatal(err)
	}
	held := func() int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(p.amount_minor), 0)::bigint FROM money_postings p
			JOIN money_accounts a ON a.id = p.account_id WHERE a.agent_id = $1 AND a.purpose = 'chain'`, agent.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// post observes tx, confirms it, then sees the same chain event again and confirms again.
	post := func(tx string) {
		t.Helper()
		if n, err := x.Observe(ctx, "base", tx, "webhook"); err != nil || n != 1 {
			t.Fatalf("observe %s = %d, %v; want it seen", tx, n, err)
		}
		if res, err := x.Confirm(ctx, "base"); err != nil || res.Posted != 1 {
			t.Fatalf("confirm %s = %+v, %v; want it posted", tx, res, err)
		}
		if n, err := x.Observe(ctx, "base", tx, "poll"); err != nil || n != 0 {
			t.Fatalf("the same chain event again = %d, %v; want nothing new", n, err)
		}
		if res, err := x.Confirm(ctx, "base"); err != nil || res.Posted != 0 {
			t.Fatalf("confirm %s again = %+v, %v; want nothing posted", tx, res, err)
		}
	}
	// told is what both phones on were told about one event: once each, and nothing more.
	told := func(kind, amount, other string) nativepush.Message {
		t.Helper()
		m := pushes.next(t)
		if again := pushes.next(t); again.Data["type"] != m.Data["type"] || again.Body != m.Body {
			t.Fatalf("the second phone was told %+v, the first %+v", again, m)
		}
		select {
		case extra := <-pushes:
			t.Fatalf("a third push: %+v (a revoked phone, or told twice)", extra)
		case <-time.After(300 * time.Millisecond):
		}
		if m.Data["type"] != kind || m.Data["agent_id"] != agent.ID || m.Data["workspace_id"] != ws {
			t.Fatalf("pushed %+v, want type %s for agent %s", m.Data, kind, agent.ID)
		}
		if !strings.Contains(m.Body, "Treasurer") || !strings.Contains(m.Body, amount) || !strings.Contains(strings.ToLower(m.Body), strings.ToLower(other[:6]+"…"+other[len(other)-4:])) {
			t.Fatalf("pushed %q, want the agent, %s and %s shortened", m.Body, amount, other)
		}
		for _, v := range append([]string{m.Title, m.Body}, fmt.Sprint(m.Data)) {
			if strings.Contains(strings.ToLower(v), strings.ToLower(other)) {
				t.Fatalf("the push shows %s in full: %q", other, v)
			}
		}
		return m
	}

	const payer = "0x00000000000000000000000000000000000Fa4e2"
	node.mine("0x"+strings.Repeat("a1", 32), payer, wallet, 40_000000)
	post("0x" + strings.Repeat("a1", 32))
	if n := held(); n != 40_000000 {
		t.Fatalf("the wallet's account holds %d after the deposit, want 40 USDC", n)
	}
	told("agent_wallet_money_in", "40 USDC", payer)

	const payee = "0x00000000000000000000000000000000000Fa4e3"
	node.mine("0x"+strings.Repeat("a2", 32), wallet, payee, 15_500000)
	post("0x" + strings.Repeat("a2", 32))
	if n := held(); n != 24_500000 {
		t.Fatalf("the wallet's account holds %d after the send, want 24.5 USDC", n)
	}
	told("agent_wallet_money_out", "15.5 USDC", payee)

	r, err := x.CreatePaymentRequest(ctx, economy.CryptoRequestIn{WorkspaceID: ws, AgentID: agent.ID, Token: "USDC", Network: "base",
		AmountMinor: 5_000000, Memo: "report", ExpiresAt: time.Now().Add(time.Hour), IdempotencyKey: "b4852", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	node.mine("0x"+strings.Repeat("a3", 32), payer, wallet, 5_000000)
	post("0x" + strings.Repeat("a3", 32))
	if got, err := store.CryptoPaymentRequest(ctx, ws, r.ID); err != nil || got.Status != "paid" || held() != 29_500000 {
		t.Fatalf("the request is %+v, %v with %d held; want it paid and 29.5 USDC held", got, err, held())
	}
	if m := told("crypto_request_paid", "5 USDC", payer); m.Data["request_id"] != r.ID {
		t.Fatalf("the request paid push names request %q, want %s", m.Data["request_id"], r.ID)
	}
}
