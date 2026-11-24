package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/tenant"
)

// b4842Ramp is the Test ramp under another name: a ramp whose deliveries are told only by the reference it reports.
type b4842Ramp struct{ partners.TestRampPartner }

func (*b4842Ramp) Name() string { return "b4842-ramp" }

// B48.42 — once a workspace has the phone's key, an owner session alone neither cashes an agent's wallet out nor
// converts into it: unsigned, or signed over another move's challenge, the confirm is refused 403 and moves nothing.
// Signed over the move's own challenge it moves once, and confirming it again needs no new signature. A card top-up is
// confirmed unsigned, as before; and the ramp's delivery of it is the deposit whose transaction the ramp reported, not
// another deposit of the same amount.
func TestB4842_WalletCashOutAndConvertAreSignedAndADeliveryIsMatchedByItsTransaction(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, secret = "ws-b4842", "whsec_b4842"
	store := economy.NewDualTokenStore(nil, pool, nil)
	reg := partners.NewRegistry(nil)
	store.SetAccountPartners(reg)
	store.SetScreener(screening.NewScreener(pool, reg))
	store.SetRampPartner(&b4842Ramp{})
	sandbox := money.NewSandbox(reg, secret)
	transfers := &b488Transfers{Transfers: sandbox.Transfers, pool: pool, credits: map[string]money.Amount{}}
	sandbox.Transfers = transfers
	layer, err := money.NewRegistry(money.Config{Sandbox: sandbox, Clearances: store, Limits: store.MoneyLimits(),
		Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool)})
	if err != nil {
		t.Fatal(err)
	}
	store.SetMoneyLayer(layer)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	r.Post("/v1/money/webhooks/{partner}", layer.ServeWebhook)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(path, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil && w.Code < 300 {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("POST %s: %v in %s", path, err, w.Body.String())
			}
		}
		return w.Code
	}
	base := "/v1/workspaces/" + ws + "/agents"
	var ops economy.Agent
	if code := call(base, `{"name":"Ops","wallet_kind":"both"}`, &ops); code != http.StatusCreated {
		t.Fatalf("create Ops = %d", code)
	}
	account := func(agentID, asset string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND currency = $2 AND status = 'open'
			AND (($3 = '' AND purpose = 'company') OR (purpose = 'agent' AND agent_id = $3))`, ws, asset, agentID).Scan(&id); err != nil {
			t.Fatalf("%q's %s account: %v", agentID, asset, err)
		}
		return id
	}
	held := func(accountID string) int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_minor), 0) FROM money_postings WHERE account_id = $1`, accountID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// notify is the money layer's deposit webhook: minor of asset arrived in accountID, in transaction ref.
	notify := func(accountID string, minor int64, asset, ref string) {
		t.Helper()
		var to string
		if err := pool.QueryRow(ctx, `SELECT partner_account_ref FROM money_accounts WHERE id = $1`, accountID).Scan(&to); err != nil {
			t.Fatal(err)
		}
		body := []byte(fmt.Sprintf(`{"id":"evt_%s","to":%q,"amount":{"amount_minor":%d,"asset":%q},"partner_ref":%q,"payer":"Acme"}`,
			ref, to, minor, asset, ref))
		req := httptest.NewRequest(http.MethodPost, "/v1/money/webhooks/sandbox", bytes.NewReader(body))
		req.Header.Set(money.SandboxSignatureHeader, money.SignSandbox(secret, body, time.Now()))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("deposit = %d %s", w.Code, w.Body.String())
		}
	}
	deposit := func(accountID string, minor int64, asset, ref, c string) {
		t.Helper()
		notify(accountID, minor, asset, ref)
		tr, err := layer.Transfers(ctx, money.Route{Workspace: ws, Capability: c})
		if err != nil {
			t.Fatal(err)
		}
		var to string
		if err := pool.QueryRow(ctx, `SELECT partner_account_ref FROM money_accounts WHERE id = $1`, accountID).Scan(&to); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.Credit(ctx, money.Credit{Key: ref, To: to, Amount: money.Amount{Minor: minor, Asset: asset}, PartnerRef: ref}); err != nil {
			t.Fatal(err)
		}
	}
	opened := httptest.NewRequest(http.MethodGet, base+"/owner-wallets", nil) // opens the owner's own wallets
	opened = opened.WithContext(auth.WithAuthContext(opened.Context(), owner))
	opening := httptest.NewRecorder()
	if r.ServeHTTP(opening, opened); opening.Code != http.StatusOK {
		t.Fatalf("owner wallets = %d %s", opening.Code, opening.Body.String())
	}
	ownerEUR, opsUSDC := account("", money.EUR), account(ops.ID, money.USDC)
	deposit(ownerEUR, 20_000, money.EUR, "in_b4842_eur", economy.CapabilityCurrencyAccounts)
	deposit(opsUSDC, 50_000_000, money.USDC, "in_b4842_usdc", economy.CapabilityStablecoins)
	if _, err := pool.Exec(ctx, `INSERT INTO money_payees (id, workspace_id, name, country, iban, check_result, checked_by)
		VALUES ('payee_b4842', $1, 'The Owner', 'GB', 'GB82WEST12345698765432', 'exact_match', 'test')`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref, verified_name)
		VALUES ($1, 2, 'person', 'test', 'completed', 'test_kyc_b4842', 'The Owner')`, ws); err != nil {
		t.Fatal(err)
	}

	// The owner's phone holds the workspace's key.
	phone, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&phone.PublicKey)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, platform, public_key, attestation) VALUES ('dev-b4842', $1, 'test', $2, '')`,
		ws, spki); err != nil {
		t.Fatal(err)
	}
	quote := func(body string) economy.WalletMove {
		t.Helper()
		var m economy.WalletMove
		if code := call(base+"/"+ops.ID+"/wallet-moves/quote", body, &m); code != http.StatusOK {
			t.Fatalf("quote %s = %d", body, code)
		}
		return m
	}
	signed := func(m economy.WalletMove) string {
		t.Helper()
		var c struct {
			Challenge string `json:"challenge"`
		}
		if code := call(base+"/"+ops.ID+"/wallet-moves/"+m.ID+"/challenge", "", &c); code != http.StatusOK || c.Challenge == "" {
			t.Fatalf("challenge for %s = %d %+v", m.ID, code, c)
		}
		h := sha256.Sum256(devicekey.Message(c.Challenge))
		sig, _ := ecdsa.SignASN1(rand.Reader, phone, h[:])
		return `{"assertion":` + jsonOf(&passkey.Assertion{DeviceID: "dev-b4842", Challenge: c.Challenge,
			Signature: base64.RawURLEncoding.EncodeToString(sig)}) + `}`
	}
	confirm := func(m economy.WalletMove, body string) (int, economy.WalletMove) {
		t.Helper()
		var got economy.WalletMove
		code := call(base+"/"+ops.ID+"/wallet-moves/"+m.ID+"/confirm", body, &got)
		return code, got
	}
	entries := func(m economy.WalletMove) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_entries e WHERE e.workspace_id = $1 AND (e.idempotency_key = 'wallet_move:' || $2
			OR e.id = (SELECT NULLIF(entry_id, '') FROM wallet_moves WHERE id = $2))`, ws, m.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	cashOut := quote(`{"route":"cash_out","amount_minor":10000000,"currency":"USDC","to_currency":"GBP","payee_id":"payee_b4842"}`)
	convert := quote(`{"route":"convert","amount_minor":10000,"currency":"EUR"}`)
	smaller := quote(`{"route":"cash_out","amount_minor":1000000,"currency":"USDC","to_currency":"GBP","payee_id":"payee_b4842"}`)
	refused := func(why string, m economy.WalletMove, body string) {
		t.Helper()
		if code, _ := confirm(m, body); code != http.StatusForbidden || entries(m) != 0 || held(opsUSDC) != 50_000_000 || held(ownerEUR) != 20_000 {
			t.Fatalf("%s = %d, %d entries, Ops %d USDC, owner %d EUR; want 403 and nothing moved", why, code, entries(m), held(opsUSDC), held(ownerEUR))
		}
	}
	refused("a cash-out confirmed by the owner's session alone", cashOut, "")
	refused("a convert confirmed by the owner's session alone", convert, "")
	refused("a 10 USDC cash-out signed over a 1 USDC one's challenge", cashOut, signed(smaller))

	if code, got := confirm(cashOut, signed(cashOut)); code != http.StatusOK || got.Status != "posted" || entries(cashOut) != 1 ||
		held(opsUSDC) != 40_000_000 {
		t.Fatalf("signed cash-out = %d %+v, %d entries, Ops %d; want it posted once, 10 USDC out", code, got, entries(cashOut), held(opsUSDC))
	}
	if code, got := confirm(convert, signed(convert)); code != http.StatusOK || got.Status != "posted" || entries(convert) != 1 ||
		held(ownerEUR) != 10_000 {
		t.Fatalf("signed convert = %d %+v, %d entries, owner %d EUR; want it posted once, 100 EUR out", code, got, entries(convert), held(ownerEUR))
	}
	afterConvert := held(opsUSDC)
	for _, m := range []economy.WalletMove{cashOut, convert} {
		if code, got := confirm(m, ""); code != http.StatusOK || got.Status != "posted" || entries(m) != 1 {
			t.Fatalf("%s confirmed again unsigned = %d %+v, %d entries; want it as it ended, posted once", m.Route, code, got, entries(m))
		}
	}
	if held(opsUSDC) != afterConvert || held(ownerEUR) != 10_000 {
		t.Fatalf("replays moved Ops %d → %d, owner %d EUR", afterConvert, held(opsUSDC), held(ownerEUR))
	}

	// A card top-up is confirmed unsigned. The ramp bought it, and the money layer has not seen it arrive.
	card := quote(`{"route":"card","amount_minor":2000,"currency":"EUR"}`)
	transfers.refuseCredit = true
	if code, got := confirm(card, ""); code != http.StatusOK || got.Status != "quoted" || !strings.Contains(got.Detail, "arrives") {
		t.Fatalf("card top-up = %d %+v; want it bought, waiting to arrive", code, got)
	}
	var rampRef string
	if err := pool.QueryRow(ctx, `SELECT ramp_ref FROM wallet_moves WHERE id = $1`, card.ID).Scan(&rampRef); err != nil || rampRef == "" {
		t.Fatalf("the ramp's reference for the card top-up = %q (%v)", rampRef, err)
	}
	// Someone else pays the agent the same amount: an ordinary payment in, not the top-up.
	before := held(opsUSDC)
	notify(opsUSDC, card.BuyMinor, money.USDC, "0xb4842someoneelse:0")
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM wallet_moves WHERE id = $1`, card.ID).Scan(&status); err != nil || status != "quoted" || entries(card) != 0 {
		t.Fatalf("a deposit of the same amount in another transaction left the card top-up %q (%v), %d entries; want it still waiting, none",
			status, err, entries(card))
	}
	// The ramp's own transaction arrives: it is the top-up, posted once.
	notify(opsUSDC, card.BuyMinor, money.USDC, rampRef+":0")
	if err := pool.QueryRow(ctx, `SELECT status FROM wallet_moves WHERE id = $1`, card.ID).Scan(&status); err != nil || status != "posted" || entries(card) != 1 ||
		held(opsUSDC) != before+2*card.BuyMinor {
		t.Fatalf("the ramp's delivery left the card top-up %q (%v), %d entries, Ops %d → %d; want it posted once", status, err, entries(card),
			before, held(opsUSDC))
	}
}
