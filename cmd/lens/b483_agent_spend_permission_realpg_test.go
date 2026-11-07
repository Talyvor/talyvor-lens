package main

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
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

// B48.3 — the owner signs their crypto agent's spend permission with the passkey that owns its account (one tap), and
// Talyvor's gate allows a payment inside it and refuses one over the cap on one payment, over the day's allowance, to
// a blocked counterparty, in a token not allowed or after expiry, keeping a row for each payment it allows and none
// for one it refuses. Another passkey's signature, an altered permission, an old one replayed and one altered in the
// database are refused. Freeze refuses everything at once and answers the call that takes the agent's key off chain;
// that call, signed by the person's passkey, is refused-after on a local devnet in internal/money/smartaccount.
func TestB483_TheOwnersSpendPermissionAndAnInstantFreeze(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b483", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	reg := partners.NewRegistry(nil)
	store.SetAccountPartners(reg)
	store.SetScreener(screening.NewScreener(pool, reg))
	layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b483"), Clearances: store, Limits: store.MoneyLimits(),
		Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool)})
	if err != nil {
		t.Fatal(err)
	}
	store.SetMoneyLayer(layer)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	store.SetSmartAccounts(cdpfake.New(t).Signers(t), "coinbase", smartaccount.CoinbaseV11)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil && w.Code < 300 {
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
	// Another passkey of the workspace's, which does not own the account.
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_passkeys (credential_id, workspace_id, public_key)
		SELECT 'cred-other', workspace_id, public_key FROM workspace_passkeys WHERE credential_id = $1`, keys[0].CredentialID); err != nil {
		t.Fatal(err)
	}
	signOther := func(challenge string) *passkey.Assertion {
		a := sign(challenge)
		a.CredentialID = "cred-other"
		return a
	}
	var agent economy.Agent
	if code := call(http.MethodPost, "/v1/workspaces/"+ws+"/agents", `{"name":"Trader","wallet_kind":"crypto"}`, &agent); code != http.StatusCreated {
		t.Fatalf("create agent = %d", code)
	}
	base := "/v1/workspaces/" + ws + "/agents/" + agent.ID
	var acct economy.AgentSmartAccount
	owners := `"credential_id":"` + keys[0].CredentialID + `"`
	var ch struct{ Challenge string }
	if code := call(http.MethodPost, base+"/smart-account/challenge", "{"+owners+"}", &ch); code != http.StatusOK {
		t.Fatalf("smart account challenge = %d", code)
	}
	if code := call(http.MethodPost, base+"/smart-account", "{"+owners+`,"assertion":`+jsonOf(sign(ch.Challenge))+"}", &acct); code != http.StatusOK {
		t.Fatalf("smart account = %d", code)
	}
	if code := call(http.MethodGet, base+"/spend-permission", "", nil); code != http.StatusNotFound {
		t.Fatalf("GET before one is signed = %d, want 404", code)
	}

	const usdc, payee, banned = "0x036CbD53842c5426634e7929541eC2318f3dCF7e", "0x000000000000000000000000000000000000b0b0", "0x000000000000000000000000000000000000BAD0"
	prepare := func(expires time.Time) (string, string) {
		t.Helper()
		var out struct{ Canonical, Challenge string }
		body := `{"tokens":[{"token":"` + strings.ToLower(usdc) + `","allowance":100000000,"period":"day","per_payment":25000000}],
			"blocked":["` + banned + `"],"expires":"` + expires.Format(time.RFC3339) + `"}`
		if code := call(http.MethodPost, base+"/spend-permission/prepare", body, &out); code != http.StatusOK {
			t.Fatalf("prepare = %d", code)
		}
		return out.Canonical, out.Challenge
	}
	put := func(canonical string, a *passkey.Assertion) int {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"canonical": canonical, "assertion": a})
		return call(http.MethodPut, base+"/spend-permission", string(b), nil)
	}
	canonical, challenge := prepare(time.Now().Add(time.Hour))
	altered := strings.Replace(canonical, `"allowance":100000000`, `"allowance":900000000`, 1)
	for name, c := range map[string]struct {
		canonical string
		a         *passkey.Assertion
		want      int
	}{
		"unsigned":                      {canonical, nil, http.StatusBadRequest},
		"signed by another passkey":     {canonical, signOther(challenge), http.StatusForbidden},
		"altered after it was prepared": {altered, sign(challenge), http.StatusForbidden},
	} {
		if code := put(c.canonical, c.a); code != c.want {
			t.Fatalf("PUT %s = %d, want %d", name, code, c.want)
		}
	}
	if code := put(canonical, sign(challenge)); code != http.StatusOK {
		t.Fatalf("PUT signed by the account's owner = %d", code)
	}
	var got economy.SpendPermission
	if code := call(http.MethodGet, base+"/spend-permission", "", &got); code != http.StatusOK || got.Number != 1 || got.FrozenAt != nil ||
		got.Permission.Tokens[0].Token != usdc || got.Account != acct.Address {
		t.Fatalf("GET = %d %+v", code, got)
	}
	if code := put(canonical, sign(challenge)); code != http.StatusBadRequest {
		t.Fatalf("the same permission signed again = %d, want 400: it is not the next number", code)
	}

	gate := func(token, to string, amount int64) error {
		_, err := store.AuthoriseAgentCryptoPayment(ctx, ws, agent.ID, smartaccount.Payment{Token: token, To: to, Amount: big.NewInt(amount)})
		return err
	}
	uses := func() (n, sum int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount), 0) FROM agent_spend_permission_uses WHERE agent_id = $1`,
			agent.ID).Scan(&n, &sum); err != nil {
			t.Fatal(err)
		}
		return n, sum
	}
	for _, amount := range []int64{10_000_000, 25_000_000, 25_000_000, 25_000_000} {
		if err := gate(usdc, payee, amount); err != nil {
			t.Fatalf("a payment of %d inside the permission: %v", amount, err)
		}
	}
	for name, err := range map[string]error{
		"over the per-payment cap":  gate(usdc, payee, 25_000_001),
		"over the day's allowance":  gate(usdc, payee, 15_000_001),
		"to a blocked counterparty": gate(usdc, strings.ToLower(banned), 1),
		"in a token not allowed":    gate(smartaccount.Native, payee, 1),
	} {
		if !errors.Is(err, smartaccount.ErrPermission) {
			t.Fatalf("a payment %s: %v, want refused", name, err)
		}
	}
	if n, sum := uses(); n != 4 || sum != 85_000_000 {
		t.Fatalf("the gate kept %d payments of %d in all, want the 4 it allowed, 85000000", n, sum)
	}
	// B48.25: the most an ERC-20 paymaster may take for the network fee counts with the payment, in the check and in
	// the use kept.
	feeGate := func(amount, fee int64) (int64, error) {
		return store.AuthoriseAgentCryptoPayment(ctx, ws, agent.ID, smartaccount.Payment{Token: usdc, To: payee, Amount: big.NewInt(amount),
			Fee: big.NewInt(fee), Paymaster: "0x000000000000000000000000000000000000FEE0"})
	}
	if _, err := feeGate(24_000_000, 1_000_001); !errors.Is(err, smartaccount.ErrPermission) || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("a payment whose fee goes over the cap on one payment: %v, want refused", err)
	}
	use, err := feeGate(4_000_000, 1_000_000)
	if err != nil {
		t.Fatalf("a payment and its fee inside the permission: %v", err)
	}
	if n, sum := uses(); n != 5 || sum != 90_000_000 {
		t.Fatalf("the gate kept %d payments of %d in all, want 5 of 90000000, the fee counted", n, sum)
	}
	// B54.12: once the operation is on the chain, the allowance counts the fee the paymaster took, 300,000 of the
	// 1,000,000 counted, and what is still approved to it, until the next such payment's approval replaces that.
	if err := gate(usdc, payee, 10_000_001); !errors.Is(err, smartaccount.ErrPermission) {
		t.Fatalf("a payment over what the quoted fee left: %v, want refused", err)
	}
	settle := func(use, charged, left int64) error {
		return store.SettleAgentCryptoFee(ctx, ws, agent.ID, use, big.NewInt(charged), big.NewInt(left))
	}
	if err := settle(use, 1_000_001, 0); err == nil {
		t.Fatal("a fee charged above the most it could be was settled")
	}
	if err := store.SettleAgentCryptoFee(ctx, "ws_other", agent.ID, use, big.NewInt(300_000), big.NewInt(0)); err == nil {
		t.Fatal("another workspace settled the agent's fee")
	}
	row := func(id int64) (amount, charged, left int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT amount, fee_charged, fee_left FROM agent_spend_permission_uses WHERE id = $1`, id).Scan(&amount, &charged, &left); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := settle(use, 300_000, 700_000); err != nil {
		t.Fatalf("settle the fee, 700,000 still approved: %v", err)
	}
	if a, c, l := row(use); a != 5_000_000 || c != 300_000 || l != 700_000 {
		t.Fatalf("the use settled counts %d (%d charged, %d left); want 5000000: the approval left still counts", a, c, l)
	}
	if err := settle(use, 200_000, 0); err == nil {
		t.Fatal("a settled fee was settled again at another figure")
	}
	// Each later fee payment's approval replaces the one left, so what is left approved counts once, on the newest
	// settled use, whatever order they are settled in; settling again never lowers what a use counts.
	next, err := feeGate(1_000_000, 500_000)
	if err != nil {
		t.Fatal(err)
	}
	third, err := feeGate(1_000_000, 500_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := settle(third, 100_000, 400_000); err != nil {
		t.Fatal(err)
	}
	if a, _, l := row(use); a != 4_300_000 || l != 0 {
		t.Fatalf("the first use counts %d with %d left, want 4300000 and 0 once its approval was replaced", a, l)
	}
	if err := settle(next, 200_000, 450_000); err != nil {
		t.Fatal(err)
	}
	if err := settle(next, 200_000, 0); err != nil {
		t.Fatal(err)
	}
	if a, _, l := row(next); a != 1_200_000 || l != 0 {
		t.Fatalf("an older use settled after a newer one counts %d with %d left, want 1200000 and 0", a, l)
	}
	if a, _, l := row(third); a != 1_550_000 || l != 450_000 {
		t.Fatalf("the newest use counts %d with %d left, want 1550000 and 450000: what is approved now", a, l)
	}
	// 92,050,000 counted: 7,949,999 fits, where the quoted fees (93,000,000) left 7,000,000.
	if err := gate(usdc, payee, 7_949_999); err != nil {
		t.Fatalf("a payment inside what the fees charged left: %v", err)
	}
	if n, sum := uses(); n != 8 || sum != 99_999_999 {
		t.Fatalf("the gate kept %d payments of %d in all, want 8 of 99999999", n, sum)
	}

	// Changing it is signing the next one; this one ends in a second.
	canonical, challenge = prepare(time.Now().Add(time.Second))
	if code := put(canonical, sign(challenge)); code != http.StatusOK {
		t.Fatalf("PUT the next permission = %d", code)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := gate(usdc, payee, 1); !errors.Is(err, smartaccount.ErrPermission) || !strings.Contains(err.Error(), "ended") {
		t.Fatalf("a payment after expiry: %v, want refused", err)
	}

	// A permission altered in the database allows nothing: the gate checks the owner's signature again.
	canonical, challenge = prepare(time.Now().Add(time.Hour))
	if code := put(canonical, sign(challenge)); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	if err := gate(usdc, payee, 1); err != nil {
		t.Fatalf("a payment under the new permission: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_spend_permissions SET canonical = replace(canonical, '"per_payment":25000000', '"per_payment":99000000')
		WHERE agent_id = $1 AND number = 3`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := gate(usdc, payee, 1); !errors.Is(err, smartaccount.ErrPermission) || !strings.Contains(err.Error(), "signed") {
		t.Fatalf("a payment under a permission altered in the database: %v, want refused", err)
	}

	// Freeze: everything refused at once, and the call that takes the agent's key out of the account on chain.
	var frozen economy.AgentFreeze
	if code := call(http.MethodPost, base+"/freeze", "", &frozen); code != http.StatusOK || frozen.Account != acct.Address ||
		!strings.HasPrefix(frozen.RevokeCall, "0xb61d27f6") { // execute(address,uint256,bytes)
		t.Fatalf("freeze = %d %+v", code, frozen)
	}
	if err := gate(usdc, payee, 1); !errors.Is(err, smartaccount.ErrFrozen) {
		t.Fatalf("a frozen agent's payment: %v, want refused", err)
	}
	if code := call(http.MethodPost, base+"/spend-permission/prepare", `{"tokens":[],"expires":"`+time.Now().Add(time.Hour).Format(time.RFC3339)+`"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("a new permission for a frozen agent = %d, want 400", code)
	}
	if n, _ := uses(); n != 9 {
		t.Fatalf("the gate kept %d payments, want 9", n)
	}
}
