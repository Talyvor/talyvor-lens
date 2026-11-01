package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/sha3"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/coinbase"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B48.6 on a local devnet and the real schema: an agent's wallet receives USDC (a mock token) on an anvil chain. The
// provider's signed webhook, sent twice, makes one movement, posted once and only after its confirmations — a large
// one after more. A forced reorg takes a posted deposit off the chain and the ledger posts its reversal; when it comes
// back it posts again, once. Money moved out of the wallet behind Talyvor's back is caught by the nightly
// reconciliation and raised to the operator; the node's logs then find it and the ledger agrees again. Every check is on
// the ledger's rows. CI's devnet job runs it (LENS_TEST_DEVNET_CONTRACTS and LENS_TEST_DATABASE_URL).
func TestB486_EveryMovementSeenConfirmedAndReconciled(t *testing.T) {
	dir := os.Getenv("LENS_TEST_DEVNET_CONTRACTS")
	if dir == "" {
		t.Skip("LENS_TEST_DEVNET_CONTRACTS is not set: run scripts/devnet-contracts.sh DIR and set it to DIR")
	}
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, secret = "ws-b486", "whsec_b486"

	store := economy.NewDualTokenStore(nil, pool, nil)
	reg := partners.NewRegistry(nil)
	store.SetAccountPartners(reg)
	store.SetScreener(screening.NewScreener(pool, reg))
	layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b486_sandbox"), Clearances: store,
		Limits: store.MoneyLimits(), Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool)})
	if err != nil {
		t.Fatal(err)
	}
	store.SetMoneyLayer(layer)
	agent, err := store.CreateAgent(ctx, ws, "treasurer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	usdcWallet, err := store.AgentWallet(ctx, ws, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	wallet := smartaccount.Checksum("0x00000000000000000000000000000000000b4860")
	if _, err := pool.Exec(ctx, `INSERT INTO agent_smart_accounts (agent_id, workspace_id, address, factory, implementation, owner_credential_id,
		owner_public_key, agent_signer, provider, solana_address) VALUES ($1, $2, $3, '0x', '0x', 'cred', $4, '0x', 'cdp', '')`,
		agent.ID, ws, wallet, make([]byte, 64)); err != nil {
		t.Fatal(err)
	}

	chain := devnet(t)
	token := chain.deploy(t, dir, "USD Coin", "USDC", 6)
	chain.send(t, devnetFunded, token, "mint(address,uint256)", devnetFunded, big.NewInt(1_000_000_000_000))
	cfg := fmt.Sprintf(`{"webhook_secret": %q, "networks": [{"name": "base", "chain_id": 8453, "rpc_url": %q, "usdc": %q,
		"confirmations": 3, "large_confirmations": 6, "large_from_minor": 1000000000, "funding": "test", "webhook_network": "base-mainnet"}]}`,
		secret, chain.RPCURL, token)
	idx, err := newChainIndex(store, cfg)
	if err != nil {
		t.Fatal(err)
	}

	webhook := func(tx string) int {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"id": "evt_" + tx[:10], "type": "onchain.activity.detected", "data": map[string]any{
			"networkId": "base-mainnet", "transactionHash": tx, "logIndex": 0, "contractAddress": token, "eventName": "Transfer",
			"from": devnetFunded, "to": wallet, "value": "1"}}) // what it says of the transfer is not taken: the node is read
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		m := hmac.New(sha256.New, []byte(secret))
		m.Write([]byte(ts + "."))
		m.Write(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/money/chain/webhooks/coinbase", bytes.NewReader(body))
		req.Header.Set(coinbase.SignatureHeader, "t="+ts+",v0="+hex.EncodeToString(m.Sum(nil)))
		w := httptest.NewRecorder()
		idx.serveWebhook(w, req)
		return w.Code
	}
	// ledger is what the agent's USDC account holds, and how many entries of each kind moved it.
	ledger := func() (int64, map[string]int) {
		t.Helper()
		var held int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_minor), 0)::bigint FROM money_postings WHERE account_id = $1`,
			usdcWallet.AccountID).Scan(&held); err != nil {
			t.Fatal(err)
		}
		kinds := map[string]int{}
		rows, err := pool.Query(ctx, `SELECT e.kind, count(DISTINCT e.id) FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
			WHERE p.account_id = $1 GROUP BY 1`, usdcWallet.AccountID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var n int
			if err := rows.Scan(&k, &n); err != nil {
				t.Fatal(err)
			}
			kinds[k] = n
		}
		return held, kinds
	}
	confirm := func() economy.ChainConfirmed {
		t.Helper()
		res, err := idx.x.Confirm(ctx, "base")
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	want := func(step string, held int64, kinds map[string]int) {
		t.Helper()
		got, gotKinds := ledger()
		if got != held || fmt.Sprint(gotKinds) != fmt.Sprint(kinds) {
			t.Fatalf("%s: the agent's USDC account holds %d by %v; want %d by %v", step, got, gotKinds, held, kinds)
		}
	}

	// A deposit posts once, after its confirmations; the webhook again changes nothing.
	deposit := chain.send(t, devnetFunded, token, "transfer(address,uint256)", wallet, big.NewInt(25_000000))
	if code := webhook(deposit); code != http.StatusOK {
		t.Fatalf("the provider's webhook = %d, want 200", code)
	}
	if code := webhook(deposit); code != http.StatusOK {
		t.Fatalf("the same webhook again = %d, want 200", code)
	}
	var movements int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chain_movements WHERE tx_hash = $1`, deposit).Scan(&movements); err != nil || movements != 1 {
		t.Fatalf("movements of the deposit = %d, %v; want 1", movements, err)
	}
	if res := confirm(); res.Posted != 0 {
		t.Fatalf("with 1 confirmation of 3: %+v, want nothing posted", res)
	}
	want("before its confirmations", 0, map[string]int{})
	chain.mine(t, 2)
	if res := confirm(); res.Posted != 1 {
		t.Fatalf("with 3 confirmations: %+v, want the deposit posted", res)
	}
	want("after its confirmations", 25_000000, map[string]int{"chain_deposit": 1})
	if code := webhook(deposit); code != http.StatusOK {
		t.Fatalf("the webhook a third time = %d, want 200", code)
	}
	chain.mine(t, 1)
	confirm()
	want("after the webhook again", 25_000000, map[string]int{"chain_deposit": 1})
	if code := webhook("0x" + strings.Repeat("ab", 32)); code != http.StatusServiceUnavailable {
		t.Fatalf("a webhook naming a transaction the node does not have = %d, want 503", code)
	}
	bad := httptest.NewRequest(http.MethodPost, "/v1/money/chain/webhooks/coinbase", strings.NewReader(`{}`))
	bad.Header.Set(coinbase.SignatureHeader, "t="+strconv.FormatInt(time.Now().Unix(), 10)+",v0=00")
	if w := httptest.NewRecorder(); func() int { idx.serveWebhook(w, bad); return w.Code }() != http.StatusUnauthorized {
		t.Fatal("a webhook whose signature does not verify was not refused 401")
	}

	// A large deposit waits for the large confirmations; the node's logs find it without a webhook.
	large := chain.send(t, devnetFunded, token, "transfer(address,uint256)", wallet, big.NewInt(2000_000000))
	chain.mine(t, 2)
	if n, err := idx.x.Poll(ctx, "base"); err != nil || n != 1 {
		t.Fatalf("poll = %d, %v; want the large deposit seen", n, err)
	}
	if res := confirm(); res.Posted != 0 {
		t.Fatalf("a large deposit with 3 of 6 confirmations: %+v, want nothing posted", res)
	}
	chain.mine(t, 3)
	if res := confirm(); res.Posted != 1 {
		t.Fatalf("a large deposit with 6 confirmations: %+v, want it posted", res)
	}
	want("after the large deposit "+large, 2025_000000, map[string]int{"chain_deposit": 2})

	// A forced reorg reverses a posted deposit; when the transaction comes back, it posts again, once.
	snapshot := chain.snapshot(t)
	raw := chain.signed(t, devnetFunded, token, "transfer(address,uint256)", wallet, big.NewInt(7_000000))
	reorged := chain.sendRaw(t, raw)
	chain.mine(t, 2)
	if n, err := idx.x.Poll(ctx, "base"); err != nil || n != 1 {
		t.Fatalf("poll = %d, %v; want the deposit seen", n, err)
	}
	confirm()
	want("before the reorg", 2032_000000, map[string]int{"chain_deposit": 3})
	chain.revert(t, snapshot)
	chain.mine(t, 4)
	if res := confirm(); res.Reversed != 1 {
		t.Fatalf("after the reorg: %+v, want the deposit reversed", res)
	}
	want("after the reorg", 2025_000000, map[string]int{"chain_deposit": 3, "chain_reversal": 1})
	if again := chain.sendRaw(t, raw); again != reorged {
		t.Fatalf("the same transaction again is %s, not %s", again, reorged)
	}
	chain.mine(t, 2)
	if res := confirm(); res.Posted != 1 {
		t.Fatalf("when the deposit is back on the chain: %+v, want it posted again", res)
	}
	chain.mine(t, 3)
	confirm()
	want("after the deposit came back", 2032_000000, map[string]int{"chain_deposit": 4, "chain_reversal": 1})

	// Reconciliation agrees; money moved out behind Talyvor's back is caught and raised; the logs then find it.
	alerts := &recordingAlerter{}
	if err := idx.reconcileOnce(ctx, alerts); err != nil || len(alerts.sent) != 0 {
		t.Fatalf("reconciliation of a mirrored wallet: %v, alerts %v; want none", err, alerts.sent)
	}
	const outsider = "0x00000000000000000000000000000000000B4861"
	chain.impersonate(t, wallet)
	chain.send(t, wallet, token, "transfer(address,uint256)", outsider, big.NewInt(5_000000))
	chain.mine(t, 3)
	if err := idx.reconcileOnce(ctx, alerts); err != nil || len(alerts.sent) != 1 || !strings.Contains(alerts.sent[0], "-5000000") {
		t.Fatalf("reconciliation after money moved behind Talyvor's back: %v, alerts %v; want one naming -5000000", err, alerts.sent)
	}
	var diff int64
	if err := pool.QueryRow(ctx, `SELECT difference_minor FROM chain_reconciliations WHERE agent_id = $1 ORDER BY created_at DESC LIMIT 1`,
		agent.ID).Scan(&diff); err != nil || diff != -5_000000 {
		t.Fatalf("the reconciliation kept = %d, %v; want -5000000", diff, err)
	}
	if _, err := idx.x.Poll(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	confirm()
	want("once the logs found the transfer out", 2027_000000, map[string]int{"chain_deposit": 4, "chain_reversal": 1, "chain_withdrawal": 1})
	alerts.sent = nil
	if err := idx.reconcileOnce(ctx, alerts); err != nil || len(alerts.sent) != 0 {
		t.Fatalf("reconciliation once the transfer out was posted: %v, alerts %v; want none", err, alerts.sent)
	}
}

type recordingAlerter struct{ sent []string }

func (a *recordingAlerter) NotifyAs(_ context.Context, kind, _, subject, body string) error {
	a.sent = append(a.sent, kind+": "+subject+": "+body)
	return nil
}

// devnetFunded is anvil's first account, unlocked.
const devnetFunded = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"

// devnetChain is an anvil chain.
type devnetChain struct{ smartaccount.Chain }

func devnet(t *testing.T) devnetChain {
	t.Helper()
	if _, err := exec.LookPath("anvil"); err != nil {
		t.Fatal("LENS_TEST_DEVNET_CONTRACTS is set but anvil is not on PATH")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.Command("anvil", "--silent", "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--chain-id", "8453")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	c := devnetChain{smartaccount.Chain{ID: 8453, RPCURL: fmt.Sprintf("http://127.0.0.1:%d", port)}}
	for i := 0; ; i++ {
		if _, err := c.Head(context.Background()); err == nil {
			return c
		} else if i == 100 {
			t.Fatalf("anvil did not start: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (c devnetChain) call(t *testing.T, method string, params []any, out any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, err := http.Post(c.RPCURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  any             `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || r.Error != nil {
		t.Fatalf("%s: %v %v", method, err, r.Error)
	}
	if out != nil {
		if err := json.Unmarshal(r.Result, out); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
}

// calldata is fn(args) ABI-encoded: each argument an address or a *big.Int.
func calldata(fn string, args ...any) string {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(fn))
	sel := h.Sum(nil)[:4]
	out := hex.EncodeToString(sel)
	for _, a := range args {
		w := make([]byte, 32)
		switch v := a.(type) {
		case string:
			b, _ := hex.DecodeString(strings.TrimPrefix(strings.ToLower(v), "0x"))
			copy(w[32-len(b):], b)
		case *big.Int:
			v.FillBytes(w)
		}
		out += hex.EncodeToString(w)
	}
	return "0x" + out
}

func (c devnetChain) tx(from, to, data string) map[string]string {
	tx := map[string]string{"from": from, "data": data, "gas": "0x1c9c380"}
	if to != "" {
		tx["to"] = to
	}
	return tx
}

func (c devnetChain) waitMined(t *testing.T, hash string) map[string]any {
	t.Helper()
	for range 100 {
		var r map[string]any
		c.call(t, "eth_getTransactionReceipt", []any{hash}, &r)
		if r != nil {
			if r["status"] != "0x1" {
				t.Fatalf("transaction %s failed: %v", hash, r)
			}
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("transaction %s was not mined", hash)
	return nil
}

// send calls fn(args) on to from from, an unlocked or impersonated account, and answers the mined transaction's hash.
func (c devnetChain) send(t *testing.T, from, to, fn string, args ...any) string {
	t.Helper()
	var hash string
	c.call(t, "eth_sendTransaction", []any{c.tx(from, to, calldata(fn, args...))}, &hash)
	c.waitMined(t, hash)
	return strings.ToLower(hash)
}

// signed is fn(args) on to signed by from, not yet sent: the same transaction can be sent again after a reorg.
func (c devnetChain) signed(t *testing.T, from, to, fn string, args ...any) string {
	t.Helper()
	var raw string
	c.call(t, "eth_signTransaction", []any{c.tx(from, to, calldata(fn, args...))}, &raw)
	return raw
}

func (c devnetChain) sendRaw(t *testing.T, raw string) string {
	t.Helper()
	var hash string
	c.call(t, "eth_sendRawTransaction", []any{raw}, &hash)
	c.waitMined(t, hash)
	return strings.ToLower(hash)
}

// deploy puts scripts/devnet-contracts.sh's MockToken on c and answers its address.
func (c devnetChain) deploy(t *testing.T, dir, name, symbol string, decimals int64) string {
	t.Helper()
	code, err := os.ReadFile(filepath.Join(dir, "MockToken.hex"))
	if err != nil {
		t.Fatal(err)
	}
	// constructor(string, string, uint8): two offsets, the decimals, then each string's length and its padded bytes.
	word := func(n int64) string { return fmt.Sprintf("%064x", n) }
	str := func(s string) string {
		b := []byte(s)
		return word(int64(len(b))) + hex.EncodeToString(append(b, make([]byte, (32-len(b)%32)%32)...))
	}
	nameEnc := str(name)
	args := word(96) + word(96+int64(len(nameEnc)/2)) + word(decimals) + nameEnc + str(symbol)
	var hash string
	c.call(t, "eth_sendTransaction", []any{c.tx(devnetFunded, "", "0x"+strings.TrimPrefix(strings.TrimSpace(string(code)), "0x")+args)}, &hash)
	return smartaccount.Checksum(c.waitMined(t, hash)["contractAddress"].(string))
}

func (c devnetChain) mine(t *testing.T, blocks int) {
	t.Helper()
	c.call(t, "anvil_mine", []any{fmt.Sprintf("0x%x", blocks)}, nil)
}

func (c devnetChain) snapshot(t *testing.T) string {
	t.Helper()
	var id string
	c.call(t, "evm_snapshot", nil, &id)
	return id
}

func (c devnetChain) revert(t *testing.T, id string) {
	t.Helper()
	var ok bool
	c.call(t, "evm_revert", []any{id}, &ok)
	if !ok {
		t.Fatalf("evm_revert %s did not revert", id)
	}
}

// impersonate lets the test send from addr, with gas to pay for it: money moved behind Talyvor's back.
func (c devnetChain) impersonate(t *testing.T, addr string) {
	t.Helper()
	c.call(t, "anvil_impersonateAccount", []any{addr}, nil)
	c.call(t, "anvil_setBalance", []any{addr, "0xde0b6b3a7640000"}, nil)
}
