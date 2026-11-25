package main

import (
	"context"
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
	"github.com/talyvor/lens/internal/workspace"
)

// B48.32 — an agent sends USDC to a person on Talyvor by their @name or email, into the agent wallet they chose for
// that network, through the real routes on a migrated schema. The person chooses the wallet through the BFF's
// settings route, and only an agent of their own may be chosen. A quote answers the person's shown name and their
// wallet; the owner's send lands there once however often it is retried under its key, by name and by email alike.
// A name nobody holds, a person who has not said yes to being found that way, and a person who chose no wallet on
// the network all answer the same status and the same words. Every check is on the rows kept and the operations the
// bundler took.
func TestB4832_AnAgentSendsUSDCToAPersonByTheirNameOrEmail(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b4832", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	reg := partners.NewRegistry(nil)
	store.SetAccountPartners(reg)
	store.SetScreener(screening.NewScreener(pool, reg))
	layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b4832"), Clearances: store, Limits: store.MoneyLimits(),
		Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool)})
	if err != nil {
		t.Fatal(err)
	}
	store.SetMoneyLayer(layer)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	store.SetSmartAccounts(cdpfake.New(t).Signers(t), "coinbase", smartaccount.CoinbaseV11)
	store.SetPersonLookupsPerHour(1000)
	const usdc = "0x036CbD53842c5426634e7929541eC2318f3dCF7e"
	chain := &fakeChain{t: t, chainID: 84532}
	url := chain.serve()
	store.SetCryptoSendNetworks([]economy.CryptoSendNetwork{{Name: "base-sepolia", USDC: usdc, Funding: economy.FundingTest, ArrivalSeconds: 6,
		Chain: smartaccount.Chain{ID: chain.chainID, RPCURL: url, BundlerURL: url}}})

	r := chi.NewRouter()
	const secret = "provision-secret"
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountAgentCryptoSendRoutes(r, store)
	mountPeopleFindingRoutes(r, secret, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, key, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if strings.HasPrefix(path, "/v1/people/") {
			req.Header.Set(provisionSecretHeader, secret)
		} else {
			req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	decode := func(body string, out any) {
		t.Helper()
		if err := json.Unmarshal([]byte(body), out); err != nil {
			t.Fatalf("%v in %s", err, body)
		}
	}

	// The sending agent: its smart account and the owner's signed spend permission, as B48.9 sets them.
	sign := b5018Passkey(t, store, ws, rpID, origin)
	keys, err := store.ListPasskeys(ctx, ws)
	if err != nil || len(keys) != 1 {
		t.Fatalf("passkeys = %v %v", keys, err)
	}
	var agent economy.Agent
	if code, body := call(http.MethodPost, "/v1/workspaces/"+ws+"/agents", "", `{"name":"Buyer","wallet_kind":"crypto"}`); code != http.StatusCreated {
		t.Fatalf("create agent = %d %s", code, body)
	} else {
		decode(body, &agent)
	}
	base := "/v1/workspaces/" + ws + "/agents/" + agent.ID
	owners := `"credential_id":"` + keys[0].CredentialID + `"`
	var ch struct{ Challenge string }
	if code, body := call(http.MethodPost, base+"/smart-account/challenge", "", "{"+owners+"}"); code != http.StatusOK {
		t.Fatalf("smart account challenge = %d %s", code, body)
	} else {
		decode(body, &ch)
	}
	if code, body := call(http.MethodPost, base+"/smart-account", "", "{"+owners+`,"assertion":`+jsonOf(sign(ch.Challenge))+"}"); code != http.StatusOK {
		t.Fatalf("smart account = %d %s", code, body)
	}
	var perm struct{ Canonical, Challenge string }
	if code, body := call(http.MethodPost, base+"/spend-permission/prepare", "", `{"tokens":[{"token":"`+usdc+`","allowance":100000000,"period":"day",
		"per_payment":25000000}],"expires":"`+time.Now().Add(time.Hour).Format(time.RFC3339)+`"}`); code != http.StatusOK {
		t.Fatalf("prepare = %d %s", code, body)
	} else {
		decode(body, &perm)
	}
	b, _ := json.Marshal(map[string]any{"canonical": perm.Canonical, "assertion": sign(perm.Challenge)})
	if code, body := call(http.MethodPut, base+"/spend-permission", "", string(b)); code != http.StatusOK {
		t.Fatalf("sign the permission = %d %s", code, body)
	}

	// People (B61.22), each with a first account and an agent wallet on it.
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	person := func(id, phone, fullName, wallet string) (agentID string) {
		t.Helper()
		identity := "test|" + id
		wsID := workspace.FirstAccountID(identity)
		exec(`INSERT INTO people (id, phone, identity, phone_sandbox, phone_confirmed_at) VALUES ($1, $2, $3, true, now())`, id, phone, identity)
		exec(`INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, $2, $1, 'personal', 'hybrid')`, wsID, fullName)
		exec(`INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref, verified_name)
			VALUES ($1, 2, 'person', 'test', 'completed', $2, $3)`, wsID, "ev-"+wsID, fullName)
		a, err := store.CreateAgent(ctx, wsID, "Wallet", "owner")
		if err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO agent_smart_accounts (agent_id, workspace_id, address, factory, implementation, owner_credential_id,
			owner_public_key, agent_signer, provider, solana_address) VALUES ($1, $2, $3, '0x', '0x', 'cred', $4, '0x', 'cdp', '')`,
			a.ID, wsID, smartaccount.Checksum(wallet), make([]byte, 64))
		return a.ID
	}
	const graceWallet, alanWallet, kenWallet = "0x00000000000000000000000000000000000b4832", "0x00000000000000000000000000000000000a1a17",
		"0x00000000000000000000000000000000000ce117"
	grace := person("per_b4832_grace", "+447700900321", "Grace Brewster Hopper", graceWallet)
	person("per_b4832_alan", "+447700900654", "Alan Turing", alanWallet)
	ken := person("per_b4832_ken", "+447700900987", "Ken Thompson", kenWallet)
	change := func(body string) (int, string) {
		t.Helper()
		return call(http.MethodPost, "/v1/people/finding/change", "", body)
	}
	wallet := func(agentID string) string {
		return `"crypto_wallet":{"network":"base-sepolia","token":"USDC","agent_id":"` + agentID + `"}`
	}

	// Choosing the wallet: only an agent of the person's own; Grace says yes by name and email and chooses hers.
	if code, body := change(`{"person_id":"per_b4832_grace",` + wallet(agent.ID) + `}`); code != http.StatusBadRequest {
		t.Fatalf("Grace chooses the sender's agent as her wallet = %d %s, want 400", code, body)
	}
	code, body := change(`{"person_id":"per_b4832_grace","name":"grace.h","by_name":true,"email":"grace@example.com","by_email":true,` + wallet(grace) + `}`)
	var f economy.PersonFinding
	decode(body, &f)
	if code != http.StatusOK || len(f.CryptoWallets) != 1 || f.CryptoWallets[0].AgentID != grace || f.CryptoWallets[0].Network != "base-sepolia" {
		t.Fatalf("Grace chooses her wallet = %d %s", code, body)
	}
	// Alan says yes by name and email, but chooses no wallet; Ken chooses one, but never says yes to being found.
	if code, body := change(`{"person_id":"per_b4832_alan","name":"alan.t","by_name":true,"email":"alan@example.com","by_email":true}`); code != http.StatusOK {
		t.Fatalf("Alan = %d %s", code, body)
	}
	if code, body := change(`{"person_id":"per_b4832_ken","name":"ken.t","email":"ken@example.com",` + wallet(ken) + `}`); code != http.StatusOK {
		t.Fatalf("Ken = %d %s", code, body)
	}

	send := func(to string, amount int64) string {
		return fmt.Sprintf(`{"to":%q,"network":"base-sepolia","token":"USDC","amount_minor":%d}`, to, amount)
	}

	// The quote names her by her shown name only, and her chosen wallet, new to this workspace.
	var q economy.CryptoSendQuote
	code, body = call(http.MethodPost, base+"/crypto-sends/quote", "", send("@grace.h", 10_000_000))
	decode(body, &q)
	if code != http.StatusOK || q.To != smartaccount.Checksum(graceWallet) || q.ToKind != "person" || q.ToLabel != "Grace H." || !q.NewAddress {
		t.Fatalf("quote to @grace.h = %d %s", code, body)
	}

	// Nobody, a person with no wallet on the network, and a person who has not said yes all answer alike.
	var nobody string
	for i, to := range []string{"@nobody.here", "nobody@example.com", "@alan.t", "alan@example.com", "@ken.t", "ken@example.com"} {
		code, body := call(http.MethodPost, base+"/crypto-sends", fmt.Sprintf("nobody-%d", i), send(to, 1_000_000))
		if code != http.StatusNotFound || !strings.Contains(body, economy.PersonNotFoundWords) {
			t.Fatalf("a send to %s = %d %s, want 404 with the one answer for nobody found", to, code, body)
		}
		if i == 0 {
			nobody = body
		} else if body != nobody {
			t.Fatalf("a send to %s answers %s, unlike nobody's %s", to, body, nobody)
		}
	}
	if len(chain.payments()) != 0 {
		t.Fatalf("the bundler took %d operations for sends to nobody", len(chain.payments()))
	}

	// By her name, at the owner's command: it lands in her wallet once, however often it is asked under its key.
	var first economy.CryptoSend
	for range 3 {
		code, body := call(http.MethodPost, base+"/crypto-sends", "pay-grace-1", send("@Grace.H", 10_000_000))
		decode(body, &first)
		if code != http.StatusOK || first.Status != "landed" || first.ToKind != "person" || first.ToLabel != "Grace H." {
			t.Fatalf("the send to @Grace.H = %d %s", code, body)
		}
	}
	// By her email: the same wallet, and it is no longer new.
	var second economy.CryptoSend
	code, body = call(http.MethodPost, base+"/crypto-sends", "pay-grace-2", send("grace@example.com", 4_000_000))
	decode(body, &second)
	if code != http.StatusOK || second.Status != "landed" || second.To != smartaccount.Checksum(graceWallet) {
		t.Fatalf("the send to grace@example.com = %d %s", code, body)
	}
	p := chain.payments()
	if len(p) != 2 || !strings.EqualFold(p[0].To, graceWallet) || p[0].Amount.Int64() != 10_000_000 ||
		!strings.EqualFold(p[1].To, graceWallet) || p[1].Amount.Int64() != 4_000_000 {
		t.Fatalf("the bundler took %+v, want 10000000 then 4000000 of USDC to Grace's wallet", p)
	}
	var rows int
	var sum int64
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount_minor), 0) FROM agent_crypto_sends WHERE agent_id = $1 AND status = 'landed'
		AND to_kind = 'person' AND to_agent_id = $2 AND lower(to_address) = lower($3)`, agent.ID, grace, graceWallet).Scan(&rows, &sum); err != nil ||
		rows != 2 || sum != 14_000_000 {
		t.Fatalf("landed sends to Grace = %d of %d, %v; want 2 of 14000000", rows, sum, err)
	}
	got, err := store.AgentCryptoSends(ctx, workspace.FirstAccountID("test|per_b4832_grace"), grace)
	if err != nil || len(got) != 2 || got[0].Direction != "in" {
		t.Fatalf("Grace's wallet's history = %+v %v", got, err)
	}
}
