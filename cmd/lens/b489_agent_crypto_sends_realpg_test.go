package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/coinbase/cdpfake"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/tenant"
)

// fakeChain is one chain's node and ERC-4337 bundler as a send sees them: the account's code and nonce, a gas price,
// and a bundler that takes each operation once per nonce — a second at a taken nonce is refused, as the EntryPoint
// refuses it — refuses any paying refuseTo, takes the first paying hangUpTo and then drops the line before answering,
// and answers each one's receipt with its own transaction; every operation it took is in a finalized block, read
// from the EntryPoint's logs as SettleAgentCryptoPayments reads them.
type fakeChain struct {
	t        *testing.T
	chainID  int64
	refuseTo string
	hangUpTo string
	mu       sync.Mutex
	paid     []smartaccount.Payment
	hashes   map[string]string
	logs     []map[string]any
}

func (f *fakeChain) serve() string {
	f.hashes = map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		answer := func(result any, err error) {
			out := map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}
			if err != nil {
				out = map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32500, "message": err.Error()}}
			}
			_ = json.NewEncoder(w).Encode(out)
		}
		switch req.Method {
		case "eth_getCode":
			answer(map[bool]string{false: "0x", true: "0x01"}[len(f.paid) > 0], nil)
		case "eth_call": // getNonce
			answer(fmt.Sprintf("0x%x", len(f.paid)), nil)
		case "eth_gasPrice":
			answer("0x3b9aca00", nil)
		case "eth_blockNumber":
			answer("0x1", nil)
		case "eth_getBlockByNumber":
			answer(map[string]string{"number": "0x100"}, nil)
		case "eth_getLogs": // UserOperationEvent logs, matched on the topics asked for
			var filter struct {
				Topics []*string `json:"topics"`
			}
			_ = json.Unmarshal(req.Params[0], &filter)
			out := []map[string]any{}
			for _, l := range f.logs {
				topics, match := l["topics"].([]string), true
				for i, want := range filter.Topics {
					if i > 0 && want != nil && !strings.EqualFold(*want, topics[i]) {
						match = false
					}
				}
				if match {
					topics[0] = *filter.Topics[0]
					out = append(out, l)
				}
			}
			answer(out, nil)
		case "eth_sendUserOperation":
			var op smartaccount.UserOp
			if err := json.Unmarshal(req.Params[0], &op); err != nil {
				answer(nil, err)
				return
			}
			pay, err := smartaccount.PaymentOf(op.CallData)
			switch {
			case err != nil || len(op.Signature) == 0:
				answer(nil, fmt.Errorf("not a signed payment: %v", err))
				return
			case op.Nonce.Int64() != int64(len(f.paid)):
				answer(nil, fmt.Errorf("AA25 invalid account nonce"))
				return
			case strings.EqualFold(pay.To, f.refuseTo):
				answer(nil, fmt.Errorf("AA23 reverted"))
				return
			}
			h, _ := op.Hash(f.chainID)
			hash := fmt.Sprintf("0x%x", h)
			f.paid = append(f.paid, pay)
			f.hashes[hash] = fmt.Sprintf("0x%064x", len(f.paid))
			f.logs = append(f.logs, map[string]any{"address": smartaccount.EntryPoint, "transactionHash": f.hashes[hash],
				"topics": []string{"", hash, "0x000000000000000000000000" + strings.ToLower(strings.TrimPrefix(op.Sender, "0x")), "0x" + strings.Repeat("0", 64)},
				"data":   fmt.Sprintf("0x%064x%064x%064x%064x", op.Nonce, 1, 1, 1)})
			if strings.EqualFold(pay.To, f.hangUpTo) {
				f.hangUpTo = ""
				http.Error(w, "upstream connection reset", http.StatusBadGateway)
				return
			}
			answer(hash, nil)
		case "eth_getUserOperationReceipt":
			var hash string
			_ = json.Unmarshal(req.Params[0], &hash)
			tx, ok := f.hashes[hash]
			if !ok {
				answer(nil, nil)
				return
			}
			answer(map[string]any{"success": true, "actualGasCost": "0x1", "receipt": map[string]string{"transactionHash": tx}}, nil)
		default:
			answer(nil, fmt.Errorf("not offered: %s", req.Method))
		}
	}))
	f.t.Cleanup(srv.Close)
	return srv.URL
}

func (f *fakeChain) payments() []smartaccount.Payment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]smartaccount.Payment(nil), f.paid...)
}

// B48.9 — an agent's smart account sends USDC: to a new address at its owner's command, once however often it is
// retried; to a new address at the agent's own asking only after the owner approves it, with a test amount sent
// there first when they ask; to that address again without asking; to another workspace's Talyvor agent, named by its
// trust, after the owner approves it the first time, and in both agents' history; once when the bundler's answer is
// lost and it is asked again. A send over the owner's permission is refused before anything is signed; one the network
// refuses gives its use of the allowance back. Every check is on the rows kept and the operations the bundler took.
func TestB489_AnAgentSendsUSDCToAnAgentOrAnyAddress(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b489", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	reg := partners.NewRegistry(nil)
	store.SetAccountPartners(reg)
	store.SetScreener(screening.NewScreener(pool, reg))
	layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b489"), Clearances: store, Limits: store.MoneyLimits(),
		Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool)})
	if err != nil {
		t.Fatal(err)
	}
	store.SetMoneyLayer(layer)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	store.SetSmartAccounts(cdpfake.New(t).Signers(t), "coinbase", smartaccount.CoinbaseV11)
	const usdc = "0x036CbD53842c5426634e7929541eC2318f3dCF7e"
	const shop, friend, broken, flaky = "0x000000000000000000000000000000000000b0b0", "0x000000000000000000000000000000000000F00D",
		"0x000000000000000000000000000000000000dEaD", "0x000000000000000000000000000000000000F1a7"
	chain := &fakeChain{t: t, chainID: 84532, refuseTo: broken, hangUpTo: flaky}
	url := chain.serve()
	store.SetCryptoSendNetworks([]economy.CryptoSendNetwork{{Name: "base-sepolia", USDC: usdc, Funding: economy.FundingTest,
		Explorer: "https://sepolia.basescan.org", ArrivalSeconds: 6,
		Chain: smartaccount.Chain{ID: chain.chainID, RPCURL: url, BundlerURL: url}}})

	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountAgentCryptoSendRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, key, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("%s %s: %v in %s", method, path, err, w.Body.String())
			}
		}
		return w.Code
	}

	sign := b5018Passkey(t, store, ws, rpID, origin)
	keys, err := store.ListPasskeys(ctx, ws)
	if err != nil || len(keys) != 1 {
		t.Fatalf("passkeys = %v %v", keys, err)
	}
	var agent economy.Agent
	if code := call(http.MethodPost, "/v1/workspaces/"+ws+"/agents", "", `{"name":"Buyer","wallet_kind":"crypto"}`, &agent); code != http.StatusCreated {
		t.Fatalf("create agent = %d", code)
	}
	base := "/v1/workspaces/" + ws + "/agents/" + agent.ID
	owners := `"credential_id":"` + keys[0].CredentialID + `"`
	var ch struct{ Challenge string }
	if code := call(http.MethodPost, base+"/smart-account/challenge", "", "{"+owners+"}", &ch); code != http.StatusOK {
		t.Fatalf("smart account challenge = %d", code)
	}
	var acct economy.AgentSmartAccount
	if code := call(http.MethodPost, base+"/smart-account", "", "{"+owners+`,"assertion":`+jsonOf(sign(ch.Challenge))+"}", &acct); code != http.StatusOK {
		t.Fatalf("smart account = %d", code)
	}
	var perm struct{ Canonical, Challenge string }
	if code := call(http.MethodPost, base+"/spend-permission/prepare", "", `{"tokens":[{"token":"`+usdc+`","allowance":100000000,"period":"day",
		"per_payment":25000000}],"expires":"`+time.Now().Add(time.Hour).Format(time.RFC3339)+`"}`, &perm); code != http.StatusOK {
		t.Fatalf("prepare = %d", code)
	}
	b, _ := json.Marshal(map[string]any{"canonical": perm.Canonical, "assertion": sign(perm.Challenge)})
	if code := call(http.MethodPut, base+"/spend-permission", "", string(b), nil); code != http.StatusOK {
		t.Fatalf("sign the permission = %d", code)
	}
	// Another workspace's agent, whose account another Talyvor agent may pay.
	other, err := store.CreateAgent(ctx, "ws-b489-other", "Seller", "owner")
	if err != nil {
		t.Fatal(err)
	}
	otherWallet := smartaccount.Checksum("0x00000000000000000000000000000000000b4890")
	if _, err := pool.Exec(ctx, `INSERT INTO agent_smart_accounts (agent_id, workspace_id, address, factory, implementation, owner_credential_id,
		owner_public_key, agent_signer, provider, solana_address) VALUES ($1, 'ws-b489-other', $2, '0x', '0x', 'cred', $3, '0x', 'cdp', '')`,
		other.ID, otherWallet, make([]byte, 64)); err != nil {
		t.Fatal(err)
	}

	send := func(to string, amount int64) string {
		return fmt.Sprintf(`{"to":%q,"network":"base-sepolia","token":"USDC","amount_minor":%d}`, to, amount)
	}
	uses := func() (n, sum int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount), 0) FROM agent_spend_permission_uses WHERE agent_id = $1`,
			agent.ID).Scan(&n, &sum); err != nil {
			t.Fatal(err)
		}
		return n, sum
	}
	paidTo := func(i int, to string, amount int64) {
		t.Helper()
		p := chain.payments()
		if len(p) <= i || !strings.EqualFold(p[i].To, to) || p[i].Amount.Int64() != amount || !strings.EqualFold(p[i].Token, usdc) {
			t.Fatalf("operation %d the bundler took = %+v, want %d of USDC to %s", i, p, amount, to)
		}
	}

	// The quote: a new address, the most the fee can be, paid by the account, and when it arrives.
	var q economy.CryptoSendQuote
	if code := call(http.MethodPost, base+"/crypto-sends/quote", "", send(strings.ToLower(shop), 10_000_000), &q); code != http.StatusOK ||
		q.To != smartaccount.Checksum(shop) || q.ToKind != "address" || !q.NewAddress || q.MaxFeeWei == "" || q.MaxFeeWei == "0" ||
		q.FeePaidBy != economy.FeePaidByAccount || q.ArrivalSeconds != 6 {
		t.Fatalf("quote = %d %+v", code, q)
	}

	// 1. At the owner's command, to a new address: it lands once, however often it is asked again under its key.
	var first economy.CryptoSend
	for range 3 {
		if code := call(http.MethodPost, base+"/crypto-sends", "pay-shop-1", send(shop, 10_000_000), &first); code != http.StatusOK ||
			first.Status != "landed" || first.TxHash == "" || first.ExplorerURL != "https://sepolia.basescan.org/tx/"+first.TxHash {
			t.Fatalf("the owner's send = %d %+v", code, first)
		}
	}
	if len(chain.payments()) != 1 {
		t.Fatalf("the bundler took %d operations for one send asked three times", len(chain.payments()))
	}
	paidTo(0, shop, 10_000_000)
	if n, sum := uses(); n != 1 || sum != 10_000_000 {
		t.Fatalf("the allowance counts %d payments of %d, want 1 of 10000000", n, sum)
	}
	if code := call(http.MethodPost, base+"/crypto-sends", "pay-shop-1", send(shop, 11_000_000), nil); code != http.StatusConflict {
		t.Fatalf("its key used for another amount = %d, want 409", code)
	}

	// 2. At the agent's own asking, to a new address: it waits for the owner, who sends a test amount first, then
	// approves it.
	waiting, err := store.SendAgentCrypto(ctx, ws, agent.ID, "agent:"+agent.ID, false, "pay-friend", economy.CryptoSendRequest{To: friend,
		Network: "base-sepolia", Token: "USDC", AmountMinor: 20_000_000})
	if err != nil || waiting.Status != "awaiting_owner" || len(chain.payments()) != 1 {
		t.Fatalf("the agent's send to a new address = %+v %v, with %d operations taken", waiting, err, len(chain.payments()))
	}
	var test economy.CryptoSend
	if code := call(http.MethodPost, base+"/crypto-sends/"+waiting.ID+"/approve", "", `{"test_amount_minor":100}`, &test); code != http.StatusOK ||
		test.Status != "landed" || test.AmountMinor != 100 {
		t.Fatalf("a test amount first = %d %+v", code, test)
	}
	paidTo(1, friend, 100)
	if still, err := store.AgentCryptoSend(ctx, ws, agent.ID, waiting.ID); err != nil || still.Status != "awaiting_owner" {
		t.Fatalf("after the test amount the send is %+v %v, want still waiting", still, err)
	}
	var approved economy.CryptoSend
	if code := call(http.MethodPost, base+"/crypto-sends/"+waiting.ID+"/approve", "", "", &approved); code != http.StatusOK ||
		approved.Status != "landed" || approved.ApprovedBy != "user:owner" {
		t.Fatalf("approved = %d %+v", code, approved)
	}
	paidTo(2, friend, 20_000_000)
	if code := call(http.MethodPost, base+"/crypto-sends/"+waiting.ID+"/approve", "", "", nil); code != http.StatusBadRequest || len(chain.payments()) != 3 {
		t.Fatalf("approved again = %d, with %d operations taken; want 400 and nothing more sent", code, len(chain.payments()))
	}

	// 3. To an address the workspace has paid: no need to ask.
	again, err := store.SendAgentCrypto(ctx, ws, agent.ID, "agent:"+agent.ID, false, "pay-shop-2", economy.CryptoSendRequest{To: shop,
		Network: "base-sepolia", Token: "USDC", AmountMinor: 5_000_000})
	if err != nil || again.Status != "landed" {
		t.Fatalf("the agent's send to a paid address = %+v %v", again, err)
	}
	paidTo(3, shop, 5_000_000)

	// 4. To another workspace's Talyvor agent: named by its trust only, waiting for the owner the first time — anyone
	// can make an agent — and in both agents' history.
	toAgent, err := store.SendAgentCrypto(ctx, ws, agent.ID, "agent:"+agent.ID, false, "pay-seller", economy.CryptoSendRequest{To: otherWallet,
		Network: "base-sepolia", Token: "USDC", AmountMinor: 7_000_000})
	if err != nil || toAgent.Status != "awaiting_owner" || toAgent.ToKind != "agent" || toAgent.ToLabel != "Unverified agent" || len(chain.payments()) != 4 {
		t.Fatalf("the agent's first send to another workspace's agent = %+v %v", toAgent, err)
	}
	if code := call(http.MethodPost, base+"/crypto-sends/"+toAgent.ID+"/approve", "", "", &toAgent); code != http.StatusOK || toAgent.Status != "landed" {
		t.Fatalf("approved = %d %+v", code, toAgent)
	}
	paidTo(4, otherWallet, 7_000_000)
	got, err := store.AgentCryptoSends(ctx, "ws-b489-other", other.ID)
	if err != nil || len(got) != 1 || got[0].Direction != "in" || got[0].To != acct.Address || got[0].AmountMinor != 7_000_000 || got[0].TxHash != toAgent.TxHash {
		t.Fatalf("the receiving agent's history = %+v %v", got, err)
	}
	var mine struct{ Sends []economy.CryptoSend }
	if code := call(http.MethodGet, base+"/crypto-sends", "", "", &mine); code != http.StatusOK || len(mine.Sends) != 5 {
		t.Fatalf("the sender's history = %d %d sends, want 5", code, len(mine.Sends))
	}

	// 4b. The bundler takes one and the line drops before it answers: the send is not given up, its use still counts,
	// and asking again sends nothing new — it reads that very operation's receipt.
	n0, sum0 := uses()
	var lost economy.CryptoSend
	if code := call(http.MethodPost, base+"/crypto-sends", "pay-flaky", send(flaky, 2_000_000), &lost); code != http.StatusAccepted ||
		lost.Status != "sending" || len(chain.payments()) != 6 {
		t.Fatalf("a send whose answer was lost = %d %+v, with %d operations taken", code, lost, len(chain.payments()))
	}
	if n1, sum1 := uses(); n1 != n0+1 || sum1 != sum0+2_000_000 {
		t.Fatalf("a send whose answer was lost no longer counts: %d/%d → %d/%d", n0, sum0, n1, sum1)
	}
	if code := call(http.MethodPost, base+"/crypto-sends", "pay-flaky", send(flaky, 2_000_000), &lost); code != http.StatusOK ||
		lost.Status != "landed" || len(chain.payments()) != 6 {
		t.Fatalf("asked again = %d %+v, with %d operations taken; want it landed, once", code, lost, len(chain.payments()))
	}
	paidTo(5, flaky, 2_000_000)

	// 5. Over the owner's permission: refused before anything is signed, and nothing counted.
	n, sum := uses()
	var over economy.CryptoSend
	if code := call(http.MethodPost, base+"/crypto-sends", "too-much", send(shop, 25_000_001), &over); code != http.StatusForbidden ||
		over.Status != "refused" || !strings.Contains(over.Reason, "cap") || len(chain.payments()) != 6 {
		t.Fatalf("a send over the cap = %d %+v, with %d operations taken", code, over, len(chain.payments()))
	}
	if n2, sum2 := uses(); n2 != n || sum2 != sum {
		t.Fatalf("a refused send counted: %d/%d → %d/%d", n, sum, n2, sum2)
	}

	// 6. One the network refuses: failed, and it counts until the chain shows it never moved — its nonce taken by the
	// next send — and then its use of the allowance is given back.
	var failed economy.CryptoSend
	if code := call(http.MethodPost, base+"/crypto-sends", "to-broken", send(broken, 1_000_000), &failed); code != http.StatusBadGateway ||
		failed.Status != "failed" || len(chain.payments()) != 6 {
		t.Fatalf("a send the network refused = %d %+v", code, failed)
	}
	if n2, sum2 := uses(); n2 != n+1 || sum2 != sum+1_000_000 {
		t.Fatalf("a send the network refused before the chain shows it never moved: %d/%d → %d/%d", n, sum, n2, sum2)
	}
	var next economy.CryptoSend
	if code := call(http.MethodPost, base+"/crypto-sends", "after-broken", send(shop, 1_000_000), &next); code != http.StatusOK ||
		next.Status != "landed" || len(chain.payments()) != 7 {
		t.Fatalf("the next send = %d %+v", code, next)
	}
	settled, err := store.SettleAgentCryptoPayments(ctx, smartaccount.Chain{ID: chain.chainID, RPCURL: url, BundlerURL: url})
	if err != nil || settled != 8 {
		t.Fatalf("settled %d, %v; want the 7 that landed and the 1 that never did", settled, err)
	}
	var outcome string
	if err := pool.QueryRow(ctx, `SELECT u.outcome FROM agent_crypto_sends s JOIN agent_spend_permission_uses u ON u.id = s.use_id
		WHERE s.id = $1`, failed.ID).Scan(&outcome); err != nil || outcome != "dropped" {
		t.Fatalf("the refused send's use = %q, %v; want dropped", outcome, err)
	}
	if n2, sum2 := uses(); n2 != n+2 || sum2 != sum+1_000_000 {
		t.Fatalf("after settling, the refused send still counts: %d/%d → %d/%d", n, sum, n2, sum2)
	}

	// 7. The owner declines one: nothing is sent.
	declined, err := store.SendAgentCrypto(ctx, ws, agent.ID, "agent:"+agent.ID, false, "pay-stranger", economy.CryptoSendRequest{
		To: "0x0000000000000000000000000000000000005a5a", Network: "base-sepolia", Token: "USDC", AmountMinor: 1_000_000})
	if err != nil || declined.Status != "awaiting_owner" {
		t.Fatalf("the agent's send to a stranger = %+v %v", declined, err)
	}
	if code := call(http.MethodPost, base+"/crypto-sends/"+declined.ID+"/decline", "", "", &declined); code != http.StatusForbidden ||
		declined.Status != "refused" || len(chain.payments()) != 7 {
		t.Fatalf("declined = %d %+v", code, declined)
	}
	var kept int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_crypto_sends WHERE agent_id = $1 AND status = 'landed'`, agent.ID).Scan(&kept); err != nil || kept != 7 {
		t.Fatalf("%d sends landed, %v; want 7", kept, err)
	}
}
