package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/sha3"

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

// spendManager is the SpendPermissionManager on a fake chain, as the calls it is sent read: the person's account
// approves and revokes spend permissions, the agent's own account spends under one approved and not revoked, inside its
// allowance, and revokes it as its spender. Each permission is named by its words (account … salt).
type spendManager struct {
	mu                sync.Mutex
	approved, revoked map[string]bool
	spent             map[string]int64
	paid              []smartaccount.Payment
}

func selectorOf(sig string) string {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(sig))
	return hex.EncodeToString(h.Sum(nil)[:4])
}

const spendTuple = "(address,address,address,uint160,uint48,uint48,uint48,uint256,bytes)"

var (
	selApprove         = selectorOf("approve(" + spendTuple + ")")
	selRevoke          = selectorOf("revoke(" + spendTuple + ")")
	selRevokeAsSpender = selectorOf("revokeAsSpender(" + spendTuple + ")")
	selSpend           = selectorOf("spend(" + spendTuple + ",uint160)")
	selAddOwner        = selectorOf("addOwnerAddress(address)")
	selRemoveOwner     = selectorOf("removeOwnerAtIndex(uint256,bytes)")
	selExecuteBatch    = selectorOf("executeBatch((address,uint256,bytes)[])")
)

// batchCalls reads executeBatch's calls: each one's target and data.
func batchCalls(t *testing.T, call string) []money.Call {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(call, "0x"))
	if err != nil || len(b) < 4+64 || hex.EncodeToString(b[:4]) != selExecuteBatch {
		t.Fatalf("not an executeBatch: %s", call)
	}
	args := b[4:]
	word := func(at int) int { return int(new(big.Int).SetBytes(args[at : at+32]).Int64()) }
	arr := word(0)
	n := word(arr)
	out := make([]money.Call, n)
	for i := range n {
		item := arr + 32 + word(arr+32+32*i)
		data := item + word(item+64)
		size := word(data)
		out[i] = money.Call{To: smartaccount.Checksum("0x" + hex.EncodeToString(args[item+12:item+32])), Value: "0",
			Data: "0x" + hex.EncodeToString(args[data+32:data+32+size])}
	}
	return out
}

// spendPermissionOf is the permission a manager call names, by its words, and the amount a spend pays.
func spendPermissionOf(data []byte) (fn, key string, amount int64) {
	fn = hex.EncodeToString(data[:4])
	words := data[4+32:]
	if fn == selSpend {
		amount, words = new(big.Int).SetBytes(data[4+32:4+64]).Int64(), data[4+64:]
	}
	return fn, hex.EncodeToString(words[:8*32]), amount
}

// run is the manager's part of an operation's calls: false when it reverts.
func (m *spendManager) run(calls []money.Call) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range calls {
		data, _ := hex.DecodeString(strings.TrimPrefix(c.Data, "0x"))
		if !strings.EqualFold(c.To, smartaccount.SpendPermissionManager) {
			continue
		}
		fn, key, amount := spendPermissionOf(data)
		switch fn {
		case selApprove:
			m.approved[key] = true
		case selRevoke, selRevokeAsSpender:
			m.revoked[key] = true
		case selSpend:
			allowance := new(big.Int).SetBytes(data[4+64+32*3 : 4+64+32*4]).Int64()
			if !m.approved[key] || m.revoked[key] || m.spent[key]+amount > allowance || i+1 >= len(calls) {
				return false
			}
			out, _ := hex.DecodeString(strings.TrimPrefix(calls[i+1].Data, "0x"))
			m.spent[key] += amount
			m.paid = append(m.paid, smartaccount.Payment{Token: smartaccount.Checksum(calls[i+1].To),
				To: smartaccount.Checksum("0x" + hex.EncodeToString(out[16:36])), Amount: new(big.Int).SetBytes(out[36:68])})
		}
	}
	return true
}

// B48.37 — the owner's spend permission, signed through the app's routes, is held on chain too. Its answer carries
// the grant call the person's passkey signs: the first one hands the agent's key over to the SpendPermissionManager and
// approves the permission for the agent's own account, which the provider made; a later one revokes the one before and
// approves the next. The provider's policy for the agent's key is set to match. A payment inside it is spent from the
// agent's own account through the provider, lands on chain and is counted at the gate; one over the cap is refused at
// Talyvor's gate before the provider is asked. Freeze answers the person's revoke call and has the agent's own account
// revoke the permissions on chain as their spender, with no tap.
func TestB4837_TheOwnersSpendPermissionOnChainThroughTheApp(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b4837", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	reg := partners.NewRegistry(nil)
	store.SetAccountPartners(reg)
	store.SetScreener(screening.NewScreener(pool, reg))
	layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b4837"), Clearances: store, Limits: store.MoneyLimits(),
		Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool)})
	if err != nil {
		t.Fatal(err)
	}
	store.SetMoneyLayer(layer)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)

	const usdc, shop = "0x036CbD53842c5426634e7929541eC2318f3dCF7e", "0x000000000000000000000000000000000000b0b0"
	chain := &fakeChain{t: t, chainID: 84532}
	url := chain.serve()
	manager := &spendManager{approved: map[string]bool{}, revoked: map[string]bool{}, spent: map[string]int64{}}
	cdp := cdpfake.New(t)
	asked := 0
	cdp.Land = func(_, _ string, calls []money.Call, _ func([32]byte) []byte) (string, error) {
		asked++
		if !manager.run(calls) {
			return "", fmt.Errorf("the chain reverted the operation")
		}
		hash := fmt.Sprintf("0x%064x", 0xb4837000+asked)
		chain.mu.Lock()
		chain.hashes[hash] = fmt.Sprintf("0x%064x", 0xc4837000+asked)
		chain.mu.Unlock()
		return hash, nil
	}
	store.SetSmartAccounts(cdp.Signers(t), "coinbase", smartaccount.CoinbaseV11)
	store.SetAgentSpenders(cdp.Spenders(t), "")
	store.SetCryptoSendNetworks([]economy.CryptoSendNetwork{{Name: "base-sepolia", USDC: usdc, Funding: economy.FundingTest,
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
		if out != nil && (w.Code < 300 || w.Code == http.StatusForbidden) { // a refused send is answered with it
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
	grantFor := func(perPayment int64) economy.SpendPermission {
		t.Helper()
		var prep struct{ Canonical, Challenge string }
		if code := call(http.MethodPost, base+"/spend-permission/prepare", "", fmt.Sprintf(`{"tokens":[{"token":%q,"allowance":100000000,"period":"day",
			"per_payment":%d}],"expires":%q}`, usdc, perPayment, time.Now().Add(time.Hour).Format(time.RFC3339)), &prep); code != http.StatusOK {
			t.Fatalf("prepare = %d", code)
		}
		b, _ := json.Marshal(map[string]any{"canonical": prep.Canonical, "assertion": sign(prep.Challenge)})
		var out economy.SpendPermission
		if code := call(http.MethodPut, base+"/spend-permission", "", string(b), &out); code != http.StatusOK {
			t.Fatalf("sign the permission = %d", code)
		}
		return out
	}
	uses := func() (n, sum int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount), 0) FROM agent_spend_permission_uses WHERE agent_id = $1`,
			agent.ID).Scan(&n, &sum); err != nil {
			t.Fatal(err)
		}
		return n, sum
	}
	fns := func(calls []money.Call) []string {
		out := make([]string, len(calls))
		for i, c := range calls {
			out[i] = c.Data[2:10]
		}
		return out
	}

	// The first grant: the manager in, the permission approved for the agent's own account, the agent's key out.
	first := grantFor(25_000_000)
	if first.Spender == "" || strings.EqualFold(first.Spender, acct.Address) {
		t.Fatalf("the permission names spender %q, want the agent's own account", first.Spender)
	}
	grant := batchCalls(t, first.GrantCall)
	if got := fns(grant); strings.Join(got, ",") != strings.Join([]string{selAddOwner, selApprove, selRemoveOwner}, ",") ||
		!strings.EqualFold(grant[0].To, acct.Address) || !strings.EqualFold(grant[1].To, smartaccount.SpendPermissionManager) {
		t.Fatalf("the first grant's calls = %v to %+v, want addOwnerAddress(manager), approve and removeOwnerAtIndex", got, grant)
	}
	if !manager.run(grant) || len(manager.approved) != 1 {
		t.Fatalf("the owner's grant did not land: %d approved", len(manager.approved))
	}
	// A later one revokes what the first approved and approves the next.
	second := grantFor(20_000_000)
	grant = batchCalls(t, second.GrantCall)
	if got := fns(grant); strings.Join(got, ",") != selRevoke+","+selApprove || second.Spender != first.Spender {
		t.Fatalf("the next grant's calls = %v, spender %s; want revoke then approve, the same spender", got, second.Spender)
	}
	if !manager.run(grant) || len(manager.approved) != 2 || len(manager.revoked) != 1 {
		t.Fatalf("the next grant did not land: %d approved, %d revoked", len(manager.approved), len(manager.revoked))
	}

	send := func(key string, amount int64, want int) economy.CryptoSend {
		t.Helper()
		var out economy.CryptoSend
		body := fmt.Sprintf(`{"to":%q,"network":"base-sepolia","token":"USDC","amount_minor":%d}`, shop, amount)
		if code := call(http.MethodPost, base+"/crypto-sends", key, body, &out); code != want {
			t.Fatalf("send %d = %d, want %d", amount, code, want)
		}
		return out
	}
	// Inside it: spent from the agent's own account through the provider, landed on chain, counted at the gate.
	paid := send("pay-shop-1", 10_000_000, http.StatusOK)
	if paid.Status != "landed" || paid.UserOpHash == "" || asked != 1 || len(manager.paid) != 1 ||
		manager.paid[0].Amount.Int64() != 10_000_000 || manager.paid[0].To != smartaccount.Checksum(shop) || manager.paid[0].Token != usdc {
		t.Fatalf("a payment inside the permission = %+v, the chain paid %+v after %d asks", paid, manager.paid, asked)
	}
	var useHash string
	if n, sum := uses(); n != 1 || sum != 10_000_000 {
		t.Fatalf("the gate counts %d payments of %d, want 1 of 10000000", n, sum)
	}
	if err := pool.QueryRow(ctx, `SELECT op_hash FROM agent_spend_permission_uses WHERE agent_id = $1`, agent.ID).Scan(&useHash); err != nil ||
		useHash != paid.UserOpHash {
		t.Fatalf("the use is kept with operation %q (%v), want the send's %s", useHash, err, paid.UserOpHash)
	}
	if again := send("pay-shop-1", 10_000_000, http.StatusOK); again.ID != paid.ID || asked != 1 {
		t.Fatalf("the same send asked again = %+v after %d asks, want the same one, sent once", again, asked)
	}

	// Outside it: over the cap on one payment, refused at Talyvor's gate before the provider is asked.
	refusedBefore := cdp.Refused()
	if over := send("pay-shop-2", 20_000_001, http.StatusForbidden); over.Status != "refused" || !strings.Contains(over.Reason, "cap") {
		t.Fatalf("a payment over the cap = %+v, want refused at the gate", over)
	}
	if asked != 1 || cdp.Refused() != refusedBefore || len(manager.paid) != 1 {
		t.Fatalf("the provider was asked %d times and refused %d, want once and none more", asked, cdp.Refused()-refusedBefore)
	}
	if n, sum := uses(); n != 1 || sum != 10_000_000 {
		t.Fatalf("after a refusal the gate counts %d payments of %d, want still 1 of 10000000", n, sum)
	}

	// The gate bypassed, the provider's policy for the agent's key refuses the same payment.
	var raw string
	var sps []smartaccount.SpendPermission
	if err := pool.QueryRow(ctx, `SELECT on_chain FROM agent_spend_permissions WHERE agent_id = $1 AND number = 2`, agent.ID).Scan(&raw); err != nil ||
		json.Unmarshal([]byte(raw), &sps) != nil || len(sps) != 1 {
		t.Fatalf("the permission on chain = %s %v", raw, err)
	}
	over, err := smartaccount.Chain{}.SpendCalls(sps[0], shop, big.NewInt(20_000_001))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cdp.Spenders(t).SendCalls(ctx, "bypass", second.Spender, over); !errors.Is(err, money.ErrRefused) || cdp.Refused() != refusedBefore+1 || asked != 1 {
		t.Fatalf("a payment over the cap with the gate bypassed: %v, want refused by the provider's policy", err)
	}

	// Freeze: the person's revoke call, and the agent's own account revokes the permission on chain at once.
	var frozen economy.AgentFreeze
	if code := call(http.MethodPost, base+"/freeze", "", "", &frozen); code != http.StatusOK || frozen.RevokedAsSpender == "" ||
		frozen.RevokeAsSpenderError != "" || !strings.HasPrefix(frozen.KeyRevokeCall, "0xb61d27f6") {
		t.Fatalf("freeze = %d %+v", code, frozen)
	}
	if got := fns(batchCalls(t, frozen.RevokeCall)); strings.Join(got, ",") != selRevoke {
		t.Fatalf("the person's revoke call = %v, want revoke of the permission on chain", got)
	}
	if asked != 2 || len(manager.revoked) != 2 {
		t.Fatalf("after the freeze the chain holds %d permissions revoked, after %d asks; want both, the second as its spender", len(manager.revoked), asked)
	}
	for k := range manager.approved {
		if !manager.revoked[k] {
			t.Fatal("a permission the owner approved is still live on chain after the freeze")
		}
	}
	if after := send("pay-shop-3", 1_000_000, http.StatusForbidden); after.Status != "refused" || asked != 2 {
		t.Fatalf("a payment after the freeze = %+v after %d asks, want refused at the gate", after, asked)
	}
}
