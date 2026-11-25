package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

const b4849Registry, b4849Resolver = "0x00000000000C2E074eC69A0dFb2997BA6C7d2e1e", "0x0000000000000000000000000000000000E45e01"

// ensCall answers an eth_call to the fake ENS registry (resolver(bytes32)) or resolver (addr(bytes32)): the resolver
// or the address of the name whose namehash it asks for, the zero word for a name it does not hold.
func (f *fakeChain) ensCall(raw json.RawMessage) (string, bool) {
	var c struct{ To, Data string }
	_ = json.Unmarshal(raw, &c)
	if !strings.EqualFold(c.To, b4849Registry) && !strings.EqualFold(c.To, b4849Resolver) {
		return "", false
	}
	answer := "0x" + strings.Repeat("0", 64)
	for name, addr := range f.ens {
		h := smartaccount.ENSNamehash(name)
		if !strings.HasSuffix(strings.ToLower(c.Data), hex.EncodeToString(h[:])) {
			continue
		}
		if strings.EqualFold(c.To, b4849Registry) {
			addr = b4849Resolver
		}
		answer = "0x" + strings.Repeat("0", 24) + strings.ToLower(strings.TrimPrefix(addr, "0x"))
	}
	return answer, true
}

// b4849Signed is an owner's send body with their passkey's assertion over its challenge under key, as the app sends
// it; body as it is when the challenge route gives none for it (a destination that cannot be sent to).
func b4849Signed(t *testing.T, r http.Handler, owner *auth.AuthContext, sign func(string) *passkey.Assertion, base, key, body string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, base+"/crypto-sends/challenge", strings.NewReader(body))
	req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
	req.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		return body
	}
	var ch struct{ Challenge string }
	var in map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &ch); err != nil || json.Unmarshal([]byte(body), &in) != nil {
		t.Fatalf("challenge %s for %s: %v", w.Body.String(), body, err)
	}
	in["assertion"] = sign(ch.Challenge)
	b, _ := json.Marshal(in)
	return string(b)
}

// b4849SignedApproval is the owner's answer at path (…/crypto-sends/{id}/approve) with their passkey's assertion over
// its challenge, as the app sends it; body as it is when the challenge route gives none for it.
func b4849SignedApproval(t *testing.T, r http.Handler, owner *auth.AuthContext, sign func(string) *passkey.Assertion, path, body string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path+"/challenge", strings.NewReader(body))
	req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		return body
	}
	var ch struct{ Challenge string }
	in := map[string]any{}
	if err := json.Unmarshal(w.Body.Bytes(), &ch); err != nil || (body != "" && json.Unmarshal([]byte(body), &in) != nil) {
		t.Fatalf("challenge %s for %s: %v", w.Body.String(), body, err)
	}
	in["assertion"] = sign(ch.Challenge)
	b, _ := json.Marshal(in)
	return string(b)
}

// B48.49 — the owner's own send from an agent's crypto wallet, through the real routes on a migrated schema: to an ENS
// name, read on the network's own registry and shown in the quote with the address it resolved to; to an address the
// owner saved and named, by its name; and, once the workspace has a passkey, only signed by it over that send's own
// challenge. An unsigned send, and one signed for another amount, are refused and nothing is sent; the signed one is
// sent once however often it is asked under its key. The owner's yes to a send their agent asked for to a new address,
// and a test amount sent there first, are signed the same way. Every check is on the rows kept and the operations the bundler
// took.
func TestB4849_TheOwnerSendsToAnENSNameOrASavedAddressSignedByTheirPasskey(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b4849", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	reg := partners.NewRegistry(nil)
	store.SetAccountPartners(reg)
	store.SetScreener(screening.NewScreener(pool, reg))
	layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b4849"), Clearances: store, Limits: store.MoneyLimits(),
		Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool)})
	if err != nil {
		t.Fatal(err)
	}
	store.SetMoneyLayer(layer)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	store.SetSmartAccounts(cdpfake.New(t).Signers(t), "coinbase", smartaccount.CoinbaseV11)
	const usdc = "0x036CbD53842c5426634e7929541eC2318f3dCF7e"
	const shop, cold = "0x000000000000000000000000000000000000b0b0", "0x0000000000000000000000000000000000C01d00"
	chain := &fakeChain{t: t, chainID: 84532, ens: map[string]string{"shop.eth": shop}}
	url := chain.serve()
	store.SetCryptoSendNetworks([]economy.CryptoSendNetwork{{Name: "base-sepolia", USDC: usdc, Funding: economy.FundingTest,
		Chain: smartaccount.Chain{ID: chain.chainID, RPCURL: url, BundlerURL: url, ENSRegistry: b4849Registry}}})

	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountAgentCryptoSendRoutes(r, store)
	mountCryptoAddressBookRoutes(r, store)
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

	// The sending agent: its smart account and the owner's signed spend permission, as B48.9 sets them.
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
	if code := call(http.MethodPost, base+"/smart-account", "", "{"+owners+`,"assertion":`+jsonOf(sign(ch.Challenge))+"}", nil); code != http.StatusOK {
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

	send := func(to string, amount int64) string {
		return fmt.Sprintf(`{"to":%q,"network":"base-sepolia","token":"USDC","amount_minor":%d}`, to, amount)
	}
	sends := func() (n, sum int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount_minor), 0) FROM agent_crypto_sends WHERE agent_id = $1`,
			agent.ID).Scan(&n, &sum); err != nil {
			t.Fatal(err)
		}
		return n, sum
	}

	// 1. An ENS name quotes to the address it resolves to on the network's registry, and says which name it was.
	var q economy.CryptoSendQuote
	if code := call(http.MethodPost, base+"/crypto-sends/quote", "", send("Shop.ETH", 10_000_000), &q); code != http.StatusOK ||
		q.To != smartaccount.Checksum(shop) || q.ENSName != "shop.eth" || q.ToKind != "address" || q.ToLabel != "shop.eth" {
		t.Fatalf("quote to Shop.ETH = %d %+v", code, q)
	}
	if code := call(http.MethodPost, base+"/crypto-sends/quote", "", send("nobody.eth", 10_000_000), nil); code != http.StatusNotFound {
		t.Fatalf("quote to a name with no address = %d, want 404", code)
	}

	// 2. The workspace has a passkey: the owner's unsigned send is refused, and nothing is kept or sent.
	if code := call(http.MethodPost, base+"/crypto-sends", "ens-1", send("shop.eth", 10_000_000), nil); code != http.StatusForbidden {
		t.Fatalf("the owner's unsigned send = %d, want 403", code)
	}
	if n, _ := sends(); n != 0 || len(chain.payments()) != 0 {
		t.Fatalf("an unsigned send kept %d sends and the bundler took %d operations", n, len(chain.payments()))
	}
	// A signature over the same send for another amount sends nothing either.
	other := b4849Signed(t, r, owner, sign, base, "ens-1", send("shop.eth", 1_000_000))
	var in map[string]any
	_ = json.Unmarshal([]byte(other), &in)
	in["amount_minor"] = 10_000_000
	b, _ = json.Marshal(in)
	if code := call(http.MethodPost, base+"/crypto-sends", "ens-1", string(b), nil); code != http.StatusForbidden {
		t.Fatalf("a send signed for 1000000 asking 10000000 = %d, want 403", code)
	}
	if n, _ := sends(); n != 0 || len(chain.payments()) != 0 {
		t.Fatalf("a send signed for another amount kept %d sends and the bundler took %d operations", n, len(chain.payments()))
	}

	// 3. Signed over its own challenge: it lands at the name's address once, however often it is asked under its key.
	signed := b4849Signed(t, r, owner, sign, base, "ens-1", send("shop.eth", 10_000_000))
	var first economy.CryptoSend
	for i := range 3 {
		body := signed
		if i > 0 {
			body = send("shop.eth", 10_000_000) // a retry of a send already made needs no new signature
		}
		if code := call(http.MethodPost, base+"/crypto-sends", "ens-1", body, &first); code != http.StatusOK || first.Status != "landed" ||
			first.To != smartaccount.Checksum(shop) || first.ToLabel != "shop.eth" {
			t.Fatalf("the owner's signed send to shop.eth = %d %+v", code, first)
		}
	}
	if p := chain.payments(); len(p) != 1 || !strings.EqualFold(p[0].To, shop) || p[0].Amount.Int64() != 10_000_000 {
		t.Fatalf("the bundler took %+v, want 10000000 of USDC to shop.eth's address, once", p)
	}

	// 4. The owner saves an address and names it; a name that reads as something else a send takes is refused.
	var saved economy.CryptoAddress
	if code := call(http.MethodPost, "/v1/workspaces/"+ws+"/crypto-addresses", "", `{"name":"Cold storage","address":"`+strings.ToLower(cold)+`"}`,
		&saved); code != http.StatusOK || saved.Address != smartaccount.Checksum(cold) || saved.Name != "Cold storage" {
		t.Fatalf("save an address = %d %+v", code, saved)
	}
	for _, bad := range []string{`{"name":"vault.eth","address":"` + shop + `"}`, `{"name":"@grace","address":"` + shop + `"}`,
		`{"name":"Cold storage","address":"` + shop + `"}`, `{"name":"Shop","address":"0x1234"}`} {
		if code := call(http.MethodPost, "/v1/workspaces/"+ws+"/crypto-addresses", "", bad, nil); code != http.StatusBadRequest {
			t.Fatalf("save %s = %d, want 400", bad, code)
		}
	}
	var book struct{ Addresses []economy.CryptoAddress }
	if code := call(http.MethodGet, "/v1/workspaces/"+ws+"/crypto-addresses", "", "", &book); code != http.StatusOK || len(book.Addresses) != 1 {
		t.Fatalf("the saved addresses = %d %+v", code, book)
	}

	// 5. A saved address sends by its name, signed, to the address it was saved as.
	if code := call(http.MethodPost, base+"/crypto-sends/quote", "", send("cold storage", 2_000_000), &q); code != http.StatusOK ||
		q.To != smartaccount.Checksum(cold) || q.SavedName != "Cold storage" {
		t.Fatalf("quote to cold storage = %d %+v", code, q)
	}
	var toCold economy.CryptoSend
	if code := call(http.MethodPost, base+"/crypto-sends", "cold-1", b4849Signed(t, r, owner, sign, base, "cold-1", send("cold storage", 2_000_000)),
		&toCold); code != http.StatusOK || toCold.Status != "landed" || toCold.ToLabel != "Cold storage" {
		t.Fatalf("the owner's signed send to cold storage = %d %+v", code, toCold)
	}
	if p := chain.payments(); len(p) != 2 || !strings.EqualFold(p[1].To, cold) || p[1].Amount.Int64() != 2_000_000 {
		t.Fatalf("the bundler took %+v, want 2000000 of USDC to the saved address next", p)
	}
	var landed, sum int64
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount_minor), 0) FROM agent_crypto_sends WHERE agent_id = $1 AND status = 'landed'
		AND requested_by = 'user:owner'`, agent.ID).Scan(&landed, &sum); err != nil || landed != 2 || sum != 12_000_000 {
		t.Fatalf("landed owner's sends = %d of %d, %v; want 2 of 12000000", landed, sum, err)
	}

	// 6. The agent asks to pay a new address: the owner's unsigned yes, or test amount, sends nothing; signed, each goes once.
	const stranger = "0x0000000000000000000000000000000000005a5a"
	waiting, err := store.SendAgentCrypto(ctx, ws, agent.ID, "agent:"+agent.ID, false, "pay-stranger", economy.CryptoSendRequest{To: stranger,
		Network: "base-sepolia", Token: "USDC", AmountMinor: 3_000_000})
	if err != nil || waiting.Status != "awaiting_owner" {
		t.Fatalf("the agent's send to a new address = %+v %v", waiting, err)
	}
	approve := base + "/crypto-sends/" + waiting.ID + "/approve"
	for _, body := range []string{"", `{"test_amount_minor":100}`} {
		if code := call(http.MethodPost, approve, "", body, nil); code != http.StatusForbidden || len(chain.payments()) != 2 {
			t.Fatalf("the owner's unsigned answer %q = %d with %d operations taken, want 403 and nothing sent", body, code, len(chain.payments()))
		}
	}
	// A yes signed for the test amount does not approve the whole send.
	b, _ = json.Marshal(map[string]any{"assertion": json.RawMessage(mustField(t, b4849SignedApproval(t, r, owner, sign, approve, `{"test_amount_minor":100}`), "assertion"))})
	if code := call(http.MethodPost, approve, "", string(b), nil); code != http.StatusForbidden || len(chain.payments()) != 2 {
		t.Fatalf("a yes signed for the test amount = %d with %d operations taken, want 403 and nothing sent", code, len(chain.payments()))
	}
	var test, approved economy.CryptoSend
	if code := call(http.MethodPost, approve, "", b4849SignedApproval(t, r, owner, sign, approve, `{"test_amount_minor":100}`), &test); code != http.StatusOK ||
		test.Status != "landed" || test.AmountMinor != 100 {
		t.Fatalf("a signed test amount = %d %+v", code, test)
	}
	if code := call(http.MethodPost, approve, "", b4849SignedApproval(t, r, owner, sign, approve, ""), &approved); code != http.StatusOK ||
		approved.Status != "landed" || approved.ApprovedBy != "user:owner" {
		t.Fatalf("a signed yes = %d %+v", code, approved)
	}
	if p := chain.payments(); len(p) != 4 || !strings.EqualFold(p[2].To, stranger) || p[2].Amount.Int64() != 100 ||
		!strings.EqualFold(p[3].To, stranger) || p[3].Amount.Int64() != 3_000_000 {
		t.Fatalf("the bundler took %+v, want 100 then 3000000 of USDC to the new address", p)
	}

	// 7. Removed, its name sends to nobody.
	if code := call(http.MethodDelete, "/v1/workspaces/"+ws+"/crypto-addresses/"+saved.ID, "", "", nil); code != http.StatusNoContent {
		t.Fatalf("remove = %d", code)
	}
	if code := call(http.MethodPost, base+"/crypto-sends/quote", "", send("cold storage", 2_000_000), nil); code != http.StatusNotFound {
		t.Fatalf("quote to a removed name = %d, want 404", code)
	}
}

// mustField is the raw JSON of field in body.
func mustField(t *testing.T, body, field string) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil || m[field] == nil {
		t.Fatalf("no %s in %s: %v", field, body, err)
	}
	return string(m[field])
}
