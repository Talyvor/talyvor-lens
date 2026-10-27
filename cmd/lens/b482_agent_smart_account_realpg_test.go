package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/coinbase/cdpfake"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/tenant"
)

// B48.2 — the owner makes a crypto agent's wallet its own smart account, owned by one of their passkeys: the agent's
// key and its Solana wallet are made inside the provider (a fake CDP) and only their addresses come back; the account's address is the one the
// factory gives those owners on every chain; a fiat agent, a passkey of another workspace and other owners for an
// account already made are refused; and the agent's private key is in no column, audit entry or log line of Lens's.
// The account itself is deployed, signed for and refused on a local devnet in internal/money/smartaccount.
func TestB482_AnAgentsCryptoWalletIsItsOwnSmartAccountOwnedByThePerson(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b482"
	var logs bytes.Buffer
	prevSlog, prevLog := slog.Default(), log.Writer()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	log.SetOutput(&logs)
	t.Cleanup(func() { slog.SetDefault(prevSlog); log.SetOutput(prevLog) })

	store := economy.NewDualTokenStore(nil, pool, nil)
	reg := partners.NewRegistry(nil)
	store.SetAccountPartners(reg)
	store.SetScreener(screening.NewScreener(pool, reg))
	layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b482"), Clearances: store, Limits: store.MoneyLimits(),
		Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool)})
	if err != nil {
		t.Fatal(err)
	}
	store.SetMoneyLayer(layer)
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
	base := "/v1/workspaces/" + ws + "/agents"
	create := func(body string) economy.Agent {
		t.Helper()
		var a economy.Agent
		if code := call(http.MethodPost, base, body, &a); code != http.StatusCreated {
			t.Fatalf("create %s = %d", body, code)
		}
		return a
	}
	trader, payroll, ops := create(`{"name":"Trader","wallet_kind":"crypto"}`), create(`{"name":"Payroll","wallet_kind":"fiat"}`), create(`{"name":"Ops","wallet_kind":"both"}`)

	// The person's passkey, a phone's device key, and a passkey of another workspace's.
	passkey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	device, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	for _, k := range []struct {
		sql, ws string
		key     *ecdsa.PublicKey
	}{
		{`INSERT INTO workspace_passkeys (credential_id, workspace_id, public_key) VALUES ('cred-b482', $1, $2)`, ws, &passkey.PublicKey},
		{`INSERT INTO workspace_passkeys (credential_id, workspace_id, public_key) VALUES ('cred-other', $1, $2)`, "ws-other", &passkey.PublicKey},
		{`INSERT INTO workspace_device_keys (id, workspace_id, platform, public_key, attestation) VALUES ('dev-b482', $1, 'test', $2, '')`, ws, &device.PublicKey},
	} {
		spki, err := x509.MarshalPKIXPublicKey(k.key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, k.sql, k.ws, spki); err != nil {
			t.Fatal(err)
		}
	}
	const recovery = "0x00000000000000000000000000000000000Ec0de"
	path := base + "/" + trader.ID + "/smart-account"
	body := `{"credential_id":"cred-b482","recovery_address":"` + recovery + `"}`

	if code := call(http.MethodPost, path, body, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("with no provider holding agents' keys = %d, want 503", code)
	}
	cdp := cdpfake.New(t)
	store.SetSmartAccounts(cdp.Signers(t), "coinbase", smartaccount.CoinbaseV11)

	for _, c := range []struct{ agent, body string }{
		{payroll.ID, body}, // a fiat agent has no crypto wallet
		{trader.ID, `{}`},
		{trader.ID, `{"credential_id":"cred-b482","device_id":"dev-b482"}`},
		{trader.ID, `{"credential_id":"cred-other"}`},
		{trader.ID, `{"credential_id":"cred-b482","recovery_address":"0xnot-an-address"}`},
	} {
		if code := call(http.MethodPost, base+"/"+c.agent+"/smart-account", c.body, nil); code != http.StatusBadRequest {
			t.Fatalf("POST %s smart-account %s = %d, want 400", c.agent, c.body, code)
		}
	}
	if code := call(http.MethodGet, path, "", nil); code != http.StatusNotFound {
		t.Fatalf("GET before it is made = %d, want 404", code)
	}

	var made economy.AgentSmartAccount
	if code := call(http.MethodPost, path, body, &made); code != http.StatusOK {
		t.Fatalf("POST smart-account = %d", code)
	}
	if len(made.Owners) != 3 || made.Owners[0].Role != "person" || made.Owners[0].CredentialID != "cred-b482" ||
		made.Owners[1].Role != "agent" || made.Owners[1].Provider != "coinbase" || made.Owners[2].Role != "recovery" ||
		!strings.EqualFold(made.Owners[2].Address, recovery) {
		t.Fatalf("owners = %+v", made.Owners)
	}
	agentKey := made.Owners[1].Address
	private, solanaPrivate := cdp.PrivateKey(agentKey), cdp.PrivateKey(made.SolanaAddress)
	if private == "" || solanaPrivate == "" {
		t.Fatalf("the agent's key %s or Solana wallet %q is not one the provider holds", agentKey, made.SolanaAddress)
	}
	want, err := smartaccount.New(smartaccount.CoinbaseV11, &passkey.PublicKey, agentKey, recovery)
	if err != nil {
		t.Fatal(err)
	}
	if made.Address != want.Address || made.Factory != smartaccount.CoinbaseV11.Address || made.EntryPoint != smartaccount.EntryPoint ||
		made.Owners[0].PublicKey != fmt.Sprintf("0x%x", want.Owners[0]) {
		t.Fatalf("account = %+v, want the factory's address for those owners, %s", made, want.Address)
	}

	// Asked again it is the same account and the same key; other owners for it are refused; anyone reads it.
	var again economy.AgentSmartAccount
	if code := call(http.MethodPost, path, body, &again); code != http.StatusOK || again.Address != made.Address || again.Owners[1].Address != agentKey ||
		again.SolanaAddress != made.SolanaAddress {
		t.Fatalf("POST again = %d %+v", code, again)
	}
	if code := call(http.MethodPost, path, `{"device_id":"dev-b482"}`, nil); code != http.StatusConflict {
		t.Fatalf("other owners for a made account = %d, want 409", code)
	}
	var read economy.AgentSmartAccount
	if code := call(http.MethodGet, path, "", &read); code != http.StatusOK || read.Address != made.Address {
		t.Fatalf("GET = %d %+v", code, read)
	}

	// A "both" agent's account, owned by the phone's device key, is its own: another agent key, another address.
	var opsAcct economy.AgentSmartAccount
	if code := call(http.MethodPost, base+"/"+ops.ID+"/smart-account", `{"device_id":"dev-b482"}`, &opsAcct); code != http.StatusOK {
		t.Fatalf("POST ops smart-account = %d", code)
	}
	if opsAcct.Address == made.Address || opsAcct.Owners[1].Address == agentKey || opsAcct.SolanaAddress == made.SolanaAddress || len(opsAcct.Owners) != 2 || opsAcct.Owners[0].DeviceID != "dev-b482" {
		t.Fatalf("ops's account = %+v", opsAcct)
	}

	// Lens holds addresses and public keys only: no column of the table can hold a key's secret, and neither agent's
	// private key is in a row, an audit entry or a log line.
	var columns []string
	rows, err := pool.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name = 'agent_smart_accounts' AND table_schema = current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, c)
	}
	if len(columns) == 0 {
		t.Fatal("agent_smart_accounts has no columns in the migrated schema")
	}
	secretish := regexp.MustCompile(`(?i)private|secret|seed|mnemonic|share|priv_`)
	for _, c := range columns {
		if secretish.MatchString(c) {
			t.Fatalf("agent_smart_accounts has a column %q", c)
		}
	}
	mig, err := os.ReadFile("../../migrations/0276_agent_smart_accounts.sql")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(mig), "CREATE TABLE") != 1 {
		t.Fatal("the migration makes another table this test does not read")
	}
	var stored bytes.Buffer
	for _, q := range []string{`SELECT row_to_json(a)::text FROM agent_smart_accounts a WHERE workspace_id = $1`,
		`SELECT body::text FROM audit_chain WHERE workspace_id = $1`} {
		rows, err := pool.Query(ctx, q, ws)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			stored.WriteString(s)
		}
	}
	if !strings.Contains(stored.String(), made.Address) || !strings.Contains(stored.String(), agentKey) || !strings.Contains(stored.String(), made.SolanaAddress) {
		t.Fatalf("the rows read do not hold the account: %s", stored.String())
	}
	for _, k := range []string{private, solanaPrivate, cdp.PrivateKey(opsAcct.Owners[1].Address), cdp.PrivateKey(opsAcct.SolanaAddress)} {
		if k == "" || strings.Contains(strings.ToLower(stored.String()), k) || strings.Contains(strings.ToLower(logs.String()), k) {
			t.Fatal("an agent's private key is in Lens's rows or logs")
		}
	}
}
