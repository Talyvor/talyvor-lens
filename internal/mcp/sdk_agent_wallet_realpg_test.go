package mcp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
)

// walletChain is one chain's node and ERC-4337 bundler as an agent's send sees them (cmd/lens's fakeChain for B48.9,
// less its failures): the account's code and nonce, the blocks and the EntryPoint's logs, the USDC contract's name and version, a gas price, and a bundler
// that takes each operation once per nonce and answers its receipt.
type walletChain struct {
	chainID int64
	mu      sync.Mutex
	paid    []smartaccount.Payment
	hashes  map[string]string
	logs    []map[string]any
}

func (f *walletChain) serve(t *testing.T) string {
	f.hashes = map[string]string{}
	abi := func(s string) string { // an ABI string: offset, length, the bytes padded to 32
		return fmt.Sprintf("0x%064x%064x%s", 32, len(s), hex.EncodeToString([]byte(s))+strings.Repeat("00", 32-len(s)%32))
	}
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
		case "eth_call": // the token's name() and version(), else the account's getNonce
			var call struct{ Data string }
			_ = json.Unmarshal(req.Params[0], &call)
			if out, ok := map[string]string{"0x06fdde03": abi("USDC"), "0x54fd4d50": abi("2")}[call.Data]; ok {
				answer(out, nil)
				return
			}
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
			}
			h, _ := op.Hash(f.chainID)
			hash := fmt.Sprintf("0x%x", h)
			f.paid = append(f.paid, pay)
			f.hashes[hash] = fmt.Sprintf("0x%064x", len(f.paid))
			f.logs = append(f.logs, map[string]any{"address": smartaccount.EntryPoint, "transactionHash": f.hashes[hash],
				"topics": []string{"", hash, "0x000000000000000000000000" + strings.ToLower(strings.TrimPrefix(op.Sender, "0x")), "0x" + strings.Repeat("0", 64)},
				"data":   fmt.Sprintf("0x%064x%064x%064x%064x", op.Nonce, 1, 1, 1)})
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
	t.Cleanup(srv.Close)
	return srv.URL
}

func (f *walletChain) payments() []smartaccount.Payment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]smartaccount.Payment(nil), f.paid...)
}

// walletPasskey registers a passkey for ws and returns its signer: this test is the owner's authenticator.
func walletPasskey(t *testing.T, store *economy.DualTokenStore, ws, rpID, origin string) (credentialID string, sign func(challenge string) *passkey.Assertion) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	credID := make([]byte, 16)
	_, _ = rand.Read(credID)
	b64 := base64.RawURLEncoding.EncodeToString
	rpHash := sha256.Sum256([]byte(rpID))
	clientData := func(typ, challenge string) []byte {
		return []byte(`{"type":"` + typ + `","challenge":"` + challenge + `","origin":"` + origin + `"}`)
	}
	regChallenge, _, err := store.PasskeyRegistrationChallenge(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	attested := binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x45), 0)
	attested = append(append(binary.BigEndian.AppendUint16(append(attested, make([]byte, 16)...), uint16(len(credID))), credID...), 0xa0)
	if _, err := store.RegisterPasskey(context.Background(), ws, "Ada's phone", passkey.Registration{CredentialID: b64(credID), PublicKey: spki,
		ClientDataJSON: clientData("webauthn.create", regChallenge), AuthenticatorData: attested}, nil); err != nil {
		t.Fatal(err)
	}
	counter := uint32(0)
	return b64(credID), func(challenge string) *passkey.Assertion {
		counter++
		cdj, ad := clientData("webauthn.get", challenge), binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x05), counter)
		h := sha256.Sum256(cdj)
		digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
		return &passkey.Assertion{CredentialID: b64(credID), ClientDataJSON: b64(cdj), AuthenticatorData: b64(ad), Signature: b64(sig)}
	}
}

// B48.16 — an assistant uses an agent's own wallet through the official TypeScript MCP SDK client, with the agent's
// key: it lists the agent's wallets, a pounds one and a USDC one, with their balances as the ledger keeps them, and
// reads where each is paid into — the pounds wallet's details and the USDC wallet's smart account address. A send
// inside the owner's permission, to an address the workspace has paid, lands once however often it is retried under
// its key; a send to an address the workspace never paid prepares and waits for the owner, and nothing is signed. It
// asks to be paid, and gets a link and the URI a QR code encodes; the history shows every send. B48.39: it asks its
// owner for a top-up, and gets the link to B48.8's quote, which the owner confirming credits on the ledger. Every call
// is in the agent's tool log. Needs node and `npm ci` in testdata/sdk-client, as TestOfficialTypeScriptSDKClient does.
func TestOfficialTypeScriptSDKClient_AgentWallet(t *testing.T) {
	dir, _ := filepath.Abs("testdata/sdk-client")
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err != nil {
		if os.Getenv("LENS_MCP_SDK_E2E") != "" {
			t.Fatalf("LENS_MCP_SDK_E2E is set but %s has no node_modules: run npm ci there", dir)
		}
		t.Skip("needs the MCP SDK: npm ci in testdata/sdk-client, or LENS_MCP_SDK_E2E=1 (the mcp-sdk CI job)")
	}
	pool := savingsTestPool(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b4816-wallet", "app.talyvor.com", "https://app.talyvor.com"
	const usdc = "0x036CbD53842c5426634e7929541eC2318f3dCF7e"
	const shop, stranger = "0x000000000000000000000000000000000000b0b0", "0x000000000000000000000000000000000000F00D"
	store := economy.NewDualTokenStore(nil, pool, nil)
	reg := partners.NewRegistry(nil)
	store.SetAccountPartners(reg)
	store.SetScreener(screening.NewScreener(pool, reg))
	layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b4816"), Clearances: store, Limits: store.MoneyLimits(),
		Audit: store, Replays: money.NewReplayStore(pool)})
	if err != nil {
		t.Fatal(err)
	}
	store.SetMoneyLayer(layer)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	store.SetSmartAccounts(cdpfake.New(t).Signers(t), "coinbase", smartaccount.CoinbaseV11)
	store.SetRampPartner(&partners.TestRampPartner{}) // B48.39: a card top-up buys the agent's USDC through it
	chain := &walletChain{chainID: 84532}
	node := chain.serve(t)
	store.SetCryptoSendNetworks([]economy.CryptoSendNetwork{{Name: "base-sepolia", USDC: usdc, Funding: economy.FundingTest,
		Explorer: "https://sepolia.basescan.org", ArrivalSeconds: 6, Chain: smartaccount.Chain{ID: chain.chainID, RPCURL: node, BundlerURL: node}}})
	indexer, err := store.NewChainIndexer([]economy.ChainNetwork{{Name: "base-sepolia", Chain: smartaccount.Chain{ID: chain.chainID, RPCURL: node},
		USDC: usdc, Confirmations: 1, Funding: economy.FundingTest}})
	if err != nil {
		t.Fatal(err)
	}

	// The owner makes a pounds-and-USDC agent, its smart account, and signs it a permission to pay up to 25 USDC at a time.
	agent, err := store.CreateAgent(ctx, ws, "Buyer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentWalletKind(ctx, ws, agent.ID, economy.WalletKindBoth, "GBP"); err != nil {
		t.Fatal(err)
	}
	cred, sign := walletPasskey(t, store, ws, rpID, origin)
	challenge, _, err := store.SmartAccountChallenge(ctx, ws, agent.ID, economy.SmartAccountOwnerRequest{CredentialID: cred})
	if err != nil {
		t.Fatal(err)
	}
	acct, err := store.CreateAgentSmartAccount(ctx, ws, agent.ID, economy.SmartAccountOwnerRequest{CredentialID: cred, Assertion: sign(challenge)})
	if err != nil {
		t.Fatal(err)
	}
	canonical, challenge, err := store.PrepareSpendPermission(ctx, ws, agent.ID, smartaccount.Permission{Tokens: []smartaccount.TokenLimit{{
		Token: usdc, Allowance: 100_000_000, Period: "day", PerPayment: 25_000_000}}, Expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetSpendPermission(ctx, ws, agent.ID, "user:owner", canonical, sign(challenge)); err != nil {
		t.Fatal(err)
	}
	// The owner has paid the shop before, so the agent's send there needs no new say.
	if c, err := store.SendAgentCrypto(ctx, ws, agent.ID, "user:owner", true, "owner-pays-shop", economy.CryptoSendRequest{To: shop,
		Network: "base-sepolia", Token: "USDC", AmountMinor: 1_000_000}); err != nil || c.Status != "landed" {
		t.Fatalf("the owner's send to the shop = %+v %v", c, err)
	}
	if err := store.AttachAgentKey(ctx, ws, agent.ID, "key-b4816"); err != nil {
		t.Fatal(err)
	}
	wallets, err := store.AgentWallets(ctx, ws, agent.ID)
	if err != nil || len(wallets.Wallets) != 2 || wallets.Wallets[0].Asset != "GBP" || wallets.Wallets[1].Asset != money.USDC {
		t.Fatalf("the agent's wallets = %+v %v, want GBP then USDC", wallets, err)
	}
	pounds := wallets.Wallets[0]

	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetAreas(everyArea{})
	srv.SetAgentBank(store)
	srv.SetCryptoRequests(indexer)
	r := chi.NewRouter()
	r.With(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer agent-key" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithAuthContext(r.Context(), &auth.AuthContext{APIKeyID: "key-b4816", WorkspaceID: ws})))
		})
	}).Post("/mcp", srv.HandleRPC)
	lens := httptest.NewServer(r)
	t.Cleanup(lens.Close)

	type result struct {
		Name    string          `json:"name"`
		IsError bool            `json:"isError"`
		Value   json.RawMessage `json:"value"`
	}
	assistant := func(calls ...map[string]any) (map[string]map[string]any, []result) {
		t.Helper()
		raw, _ := json.Marshal(calls)
		cmd := exec.Command("node", filepath.Join(dir, "wallet.mjs"), lens.URL, "agent-key", string(raw))
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			stderr := ""
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = string(ee.Stderr)
			}
			t.Fatalf("the SDK client failed: %v\n%s%s", err, out, stderr)
		}
		var got struct {
			Tools []struct {
				Name        string         `json:"name"`
				Annotations map[string]any `json:"annotations"`
			} `json:"tools"`
			Results []result `json:"results"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
			t.Fatalf("the SDK client printed %q: %v", out, err)
		}
		tools := map[string]map[string]any{}
		for _, tl := range got.Tools {
			tools[tl.Name] = tl.Annotations
		}
		for _, res := range got.Results {
			if res.IsError {
				t.Fatalf("%s was refused: %s", res.Name, res.Value)
			}
		}
		return tools, got.Results
	}
	call := func(name string, args map[string]any) map[string]any {
		return map[string]any{"name": name, "arguments": args}
	}
	send := func(to string, amount int64, key string) map[string]any {
		return call("wallet_send", map[string]any{"to": to, "network": "base-sepolia", "token": "USDC", "amount_minor": amount, "idempotency_key": key})
	}
	tools, results := assistant(
		call("wallet_list", map[string]any{}),
		call("wallet_balance", map[string]any{}),
		call("wallet_receive_details", map[string]any{}),
		send(shop, 2_000_000, "pay-shop"),
		send(shop, 2_000_000, "pay-shop"),
		send(stranger, 3_000_000, "pay-stranger"),
		call("wallet_request", map[string]any{"network": "base-sepolia", "token": "USDC", "amount_minor": 5_000_000, "memo": "the March report",
			"expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339), "idempotency_key": "ask-1"}),
		call("wallet_history", map[string]any{}),
	)
	for name, readOnly := range map[string]bool{"wallet_list": true, "wallet_balance": true, "wallet_receive_details": true, "wallet_history": true,
		"wallet_send": false, "wallet_request": false} {
		if a := tools[name]; a == nil || a["readOnlyHint"] != readOnly || a["destructiveHint"] == readOnly {
			t.Errorf("%s's annotations = %v, want readOnlyHint %v", name, a, readOnly)
		}
	}
	value := func(i int, into any) {
		t.Helper()
		if err := json.Unmarshal(results[i].Value, into); err != nil {
			t.Fatalf("%s answered %s: %v", results[i].Name, results[i].Value, err)
		}
	}

	// The wallets and their balances, as the ledger keeps them.
	var list struct {
		WalletKind string `json:"wallet_kind"`
		Wallets    []struct {
			AccountID    string `json:"account_id"`
			Asset        string `json:"asset"`
			BalanceMinor int64  `json:"balance_minor"`
		} `json:"wallets"`
	}
	value(0, &list)
	if list.WalletKind != economy.WalletKindBoth || len(list.Wallets) != 2 || list.Wallets[0].AccountID != pounds.AccountID ||
		list.Wallets[1].AccountID != wallets.Wallets[1].AccountID {
		t.Fatalf("wallet_list = %s, want the agent's two wallets", results[0].Value)
	}
	var balance struct {
		Balances []struct {
			Asset        string `json:"asset"`
			BalanceMinor int64  `json:"balance_minor"`
		} `json:"balances"`
	}
	value(1, &balance)
	if len(balance.Balances) != 2 || balance.Balances[0].Asset != "GBP" || balance.Balances[0].BalanceMinor != pounds.BalanceMinor ||
		balance.Balances[1].Asset != money.USDC || balance.Balances[1].BalanceMinor != wallets.Wallets[1].BalanceMinor {
		t.Fatalf("wallet_balance = %s, want the ledger's %d GBP and %d USDC", results[1].Value, pounds.BalanceMinor, wallets.Wallets[1].BalanceMinor)
	}

	// Where each is paid into: the pounds wallet at its details, the USDC wallet at its smart account.
	var receive struct {
		Wallets []struct {
			Asset            string        `json:"asset"`
			Receive          money.Details `json:"receive"`
			PaymentReference string        `json:"payment_reference"`
			Address          string        `json:"address"`
			Networks         []string      `json:"networks"`
		} `json:"wallets"`
	}
	value(2, &receive)
	if len(receive.Wallets) != 2 || receive.Wallets[0].Receive != pounds.Receive || receive.Wallets[0].PaymentReference != pounds.PaymentReference ||
		receive.Wallets[1].Address != acct.Address || acct.Address == "" || len(receive.Wallets[1].Networks) != 1 || receive.Wallets[1].Networks[0] != "base-sepolia" {
		t.Fatalf("wallet_receive_details = %s, want the pounds wallet's details %+v and the smart account %s on base-sepolia",
			results[2].Value, pounds.Receive, acct.Address)
	}

	// Inside the permission: it lands, once, however often it is asked under its key.
	var paid, again, waiting economy.CryptoSend
	value(3, &paid)
	value(4, &again)
	if paid.Status != "landed" || paid.TxHash == "" || again.ID != paid.ID || paid.RequestedBy != "agent:"+agent.ID {
		t.Fatalf("the agent's send to the shop = %+v, asked again %+v; want landed once, asked by the agent", paid, again)
	}
	// Outside it: prepared for the owner, nothing signed.
	value(5, &waiting)
	if waiting.Status != "awaiting_owner" || waiting.TxHash != "" {
		t.Fatalf("the agent's send to a new address = %s, want awaiting_owner", results[5].Value)
	}
	if p := chain.payments(); len(p) != 2 || !strings.EqualFold(p[1].To, shop) || p[1].Amount.Int64() != 2_000_000 || !strings.EqualFold(p[1].Token, usdc) {
		t.Fatalf("the bundler took %+v, want the owner's send and one of 2 USDC to the shop", p)
	}
	var uses, used int64
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(amount), 0) FROM agent_spend_permission_uses WHERE agent_id = $1`,
		agent.ID).Scan(&uses, &used); err != nil || uses != 2 || used != 3_000_000 {
		t.Fatalf("the allowance counts %d payments of %d (%v), want 2 of 3000000", uses, used, err)
	}
	var status, by string
	if err := pool.QueryRow(ctx, `SELECT status, requested_by FROM agent_crypto_sends WHERE id = $1`, waiting.ID).Scan(&status, &by); err != nil ||
		status != "awaiting_owner" || by != "agent:"+agent.ID {
		t.Fatalf("the waiting send's row = %s by %s (%v)", status, by, err)
	}

	// Asked to be paid: a link and what the QR code encodes, into the agent's smart account.
	var ask economy.CryptoRequest
	value(6, &ask)
	if ask.ID == "" || ask.PayTo != acct.Address || ask.AmountMinor != 5_000_000 || ask.Status != "open" || ask.Link == "" ||
		!strings.HasPrefix(ask.URI, "ethereum:") || ask.AgentID != agent.ID {
		t.Fatalf("wallet_request = %s, want an open request for 5 USDC into %s", results[6].Value, acct.Address)
	}

	// The history: every send, newest first.
	var history struct {
		Sends []economy.CryptoSend `json:"sends"`
		Lines []economy.WalletLine `json:"lines"`
	}
	value(7, &history)
	if len(history.Sends) != 3 || history.Sends[0].ID != waiting.ID || history.Sends[1].ID != paid.ID {
		t.Fatalf("wallet_history's sends = %+v, want the waiting one, the shop's, the owner's", history.Sends)
	}

	// B48.39 — the agent asks its owner to top its USDC wallet up by card: wallet_topup_link quotes it through B48.8,
	// at the Test ramp's rate and 3% fee, and answers the link the owner opens and B48.8's route that confirms it. Only
	// the owner's confirm moves money: it credits the agent's USDC wallet on the ledger, once, at the quote.
	tools, topUps := assistant(call("wallet_topup_link", map[string]any{"amount_minor": 10_000, "currency": "EUR"}))
	if a := tools["wallet_topup_link"]; a == nil || a["readOnlyHint"] != false || a["destructiveHint"] != false {
		t.Errorf("wallet_topup_link's annotations = %v, want it to write a quote and move nothing", a)
	}
	var topUp struct {
		Move    economy.WalletMove `json:"move"`
		Link    string             `json:"link"`
		Confirm string             `json:"confirm"`
	}
	if err := json.Unmarshal(topUps[0].Value, &topUp); err != nil {
		t.Fatalf("wallet_topup_link answered %s: %v", topUps[0].Value, err)
	}
	var q struct {
		id, route, status, sellCurrency, buyCurrency, rate, by string
		sell, fee, buy                                         int64
	}
	if err := pool.QueryRow(ctx, `SELECT id, route, status, sell_currency, buy_currency, rate, created_by, sell_minor, fee_minor, buy_minor
		FROM wallet_moves WHERE workspace_id = $1 AND agent_id = $2`, ws, agent.ID).Scan(&q.id, &q.route, &q.status, &q.sellCurrency,
		&q.buyCurrency, &q.rate, &q.by, &q.sell, &q.fee, &q.buy); err != nil || q.id != topUp.Move.ID || q.route != economy.WalletMoveCard ||
		q.status != "quoted" || q.sell != 10_000 || q.sellCurrency != "EUR" || q.fee != 300 || q.buy != 106_700_000 || q.buyCurrency != money.USDC ||
		q.by != "agent:"+agent.ID || topUp.Move.BuyMinor != q.buy || topUp.Move.Rate != q.rate {
		t.Fatalf("wallet_topup_link = %s; B48.8 keeps %+v (%v), want the one card quote of 100 EUR buying 106.70 USDC, asked by the agent",
			topUps[0].Value, q, err)
	}
	if want := "/v1/workspaces/" + ws + "/agents/" + agent.ID + "/wallet-moves/" + topUp.Move.ID + "/confirm"; topUp.Confirm != want {
		t.Fatalf("the top-up's confirm = %q, want B48.8's route %q", topUp.Confirm, want)
	}
	if want := "https://app.talyvor.com/agents/" + agent.ID + "?top_up=" + topUp.Move.ID; topUp.Link != want {
		t.Fatalf("the top-up's link = %q, want %q", topUp.Link, want)
	}
	usdcHeld := func() int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_minor), 0) FROM money_postings WHERE account_id = $1`,
			wallets.Wallets[1].AccountID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := usdcHeld()
	for i := range 2 { // the owner taps it twice: it posts once
		posted, err := store.ConfirmWalletMove(ctx, ws, agent.ID, topUp.Move.ID, "owner", false, nil)
		if err != nil || posted.Status != "posted" || posted.EntryID == "" {
			t.Fatalf("the owner's confirm (%d) = %+v %v, want it posted", i, posted, err)
		}
	}
	if got := usdcHeld() - before; got != 106_700_000 {
		t.Fatalf("the owner's top-up credited the agent's USDC wallet %d on the ledger, want 106700000 once", got)
	}

	// Every call is in the agent's tool log.
	var logged int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_tool_calls WHERE agent_id = $1 AND tool LIKE 'wallet_%' AND outcome = 'ok'`,
		agent.ID).Scan(&logged); err != nil || logged != len(results)+len(topUps) {
		t.Fatalf("the tool log has %d calls (%v), want %d", logged, err, len(results)+len(topUps))
	}
}
