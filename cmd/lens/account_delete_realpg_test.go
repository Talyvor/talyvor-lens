package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/audit"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money/holdings"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/internal/workspace"
)

// B52.37 — a person deletes their account: refused while it holds money, then their workspace goes, and every
// key and session token of theirs is refused, also by a Lens that started afterwards.

const accountDeleteSecret = "account-delete-test-secret"

// accountDeleteLens is one Lens process: the provision and deletion routes, and a route behind the real auth.
func accountDeleteLens(t *testing.T, pool *pgxpool.Pool, jwtKey *ecdsa.PrivateKey, d accountDeleteDeps) (http.Handler, *auth.KeyStore) {
	t.Helper()
	// As it runs: Lens as lens_app, which may not delete token_events, and that one table as lens_retention (B39.6).
	app := accountDeleteAs(t, pool, dbmigrate.AppRole)
	wsm := workspace.New(app).WithUsageEventsPool(accountDeleteAs(t, pool, dbmigrate.RetentionRole))
	if err := wsm.LoadAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	keys := auth.New(app)
	am := auth.NewManager("", jwtKey, keys, tenant.NewStore(app)).WithAccountDeletions(wsm)
	r := chi.NewRouter()
	mountProvisionRoute(r, accountDeleteSecret, wsm, nil, func(ws, user string, scopes []string, ttl time.Duration) (string, error) {
		return auth.GenerateToken(ws, user, scopes, am.PrivateKey(), ttl)
	})
	d.accounts, d.keys = wsm, keys.EvictWorkspace
	mountAccountDeleteRoute(r, accountDeleteSecret, d)
	r.With(auth.AuthMiddleware(keys, am)).Get("/whoami", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.With(auth.AuthMiddleware(keys, am)).Post("/v1/workspaces/{ws}/agents/{agent}/pay", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return r, keys
}

// accountDeleteAs is pool's database as role, with that role's grants applied as a migration applies them.
func accountDeleteAs(t *testing.T, pool *pgxpool.Pool, role string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	c, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = dbmigrate.ApplyRoles(ctx, c.Conn(), "", "")
	c.Release()
	if err != nil {
		t.Fatal(err)
	}
	cfg := pool.Config()
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+role)
		return err
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// accountDeletePay is a Connect token's one route: an agent's pay.
func accountDeletePay(h http.Handler, ws, agent, credential string) int {
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+ws+"/agents/"+agent+"/pay", nil)
	req.Header.Set("Authorization", "Bearer "+credential)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func accountDeletePost(t *testing.T, h http.Handler, path, identity string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"identity": identity})
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set(provisionSecretHeader, accountDeleteSecret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func accountDeleteAuthed(h http.Handler, credential string) int {
	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+credential)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestB5237_PersonDeletesAccount_RealPG(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	jwtKey, err := auth.GenerateECKey()
	if err != nil {
		t.Fatal(err)
	}
	answers := &accountDeleteAnswers{fail: true}
	billed := 0
	lens, keys := accountDeleteLens(t, pool, jwtKey, accountDeleteDeps{answers: answers,
		billing: func(context.Context, string) error { billed++; return nil }})
	const identity = "https://issuer.example|person-5237"

	rec := accountDeletePost(t, lens, "/v1/provision", identity)
	if rec.Code != http.StatusOK {
		t.Fatalf("provision: %d %s", rec.Code, rec.Body)
	}
	var prov provisionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &prov)
	ws := prov.WorkspaceID
	wsKey, _, err := tenant.NewStore(pool).CreateAPIKey(ctx, ws, "laptop", []string{auth.ScopeProxy}, nil)
	if err != nil {
		t.Fatal(err)
	}
	apiKey, _, err := keys.GenerateKey(ctx, ws, "", "agent", nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, cred := range map[string]string{"session token": prov.Token, "workspace key": wsKey, "api key": apiKey} {
		if code := accountDeleteAuthed(lens, cred); code != http.StatusOK {
			t.Fatalf("before deletion the %s is refused: %d", name, code)
		}
	}
	connect, err := auth.GenerateConnectToken(ws, "u", "client-5237", "ag-pay", auth.ConnectScopes, jwtKey, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if code := accountDeletePay(lens, ws, "ag-pay", connect); code != http.StatusOK {
		t.Fatalf("before deletion the Connect token is refused: %d", code)
	}

	// A room of someone else's: the person's message in it goes; another's message they removed stays.
	if _, err := pool.Exec(ctx, `INSERT INTO rooms (id, owner_workspace_id, title) VALUES ('room-5237', 'u-other', 'r');
		INSERT INTO room_messages (id, room_id, author_workspace_id, kind, body) VALUES ('m-mine', 'room-5237', '`+ws+`', 'text', 'hi');
		INSERT INTO room_messages (id, room_id, author_workspace_id, kind, deleted_at, deleted_by_workspace_id)
			VALUES ('m-theirs', 'room-5237', 'u-other', 'text', now(), '`+ws+`');
		INSERT INTO agent_money_requests (id, from_workspace_id, from_agent_id, to_workspace_id, to_agent_id, amount_ulxc)
			VALUES ('req-5237', 'u-other', 'a-other', '`+ws+`', 'a-mine', 7);
		INSERT INTO token_events (provider, model, input_tokens, output_tokens, workspace_id, prompt_text)
			VALUES ('openai', 'gpt', 1, 1, '`+ws+`', 'my words')`); err != nil {
		t.Fatal(err)
	}

	// It holds credits: refused, said why, and nothing is deleted.
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance) VALUES ($1, 5)`, ws); err != nil {
		t.Fatal(err)
	}
	rec = accountDeletePost(t, lens, "/v1/account/delete", identity)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "credits") {
		t.Fatalf("an account holding credits: %d %s, want 409 naming the credits", rec.Code, rec.Body)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM workspaces WHERE id = $1`, ws).Scan(&n)
	if n != 1 {
		t.Fatalf("a refused deletion deleted the workspace")
	}

	if _, err := pool.Exec(ctx, `UPDATE lxc_balances SET balance = 0 WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	// A payment in held for a compliance check, or a crypto transfer not yet final, is money it holds too.
	for _, held := range []struct{ what, insert, settle string }{
		{"compliance check", `INSERT INTO money_quarantined_deposits (workspace_id, partner_ref, case_id, partner, partner_account_ref, payer,
			reference, amount_minor, currency, funding) VALUES ($1, 'p-5237', 'c-5237', 'test', 'pa', 'payer', 'ref', 5, 'EUR', 'test')`,
			`UPDATE money_quarantined_deposits SET posted_at = now() WHERE workspace_id = $1`},
		{"compliance check", `INSERT INTO chain_movements (id, workspace_id, agent_id, network, tx_hash, log_position, direction, wallet,
			counterparty, token, asset, amount_minor, funding, source, status) VALUES ('cm-5237', $1, 'a', 'base', '0x01', 0, 'in', 'w', 'c',
			't', 'USDC', 5, 'test', 'poll', 'quarantined')`, `UPDATE chain_movements SET status = 'seen' WHERE id = 'cm-5237'`},
		{"crypto transfer not yet final", ``, `UPDATE chain_movements SET status = 'final' WHERE id = 'cm-5237'`},
	} {
		if held.insert != "" {
			if _, err := pool.Exec(ctx, held.insert, ws); err != nil {
				t.Fatal(err)
			}
		}
		if rec = accountDeletePost(t, lens, "/v1/account/delete", identity); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), held.what) {
			t.Fatalf("an account with a %s open: %d %s, want 409 naming it", held.what, rec.Code, rec.Body)
		}
		args := []any{ws}
		if !strings.Contains(held.settle, "$1") {
			args = nil
		}
		if _, err := pool.Exec(ctx, held.settle, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Its cached answers cannot be removed: refused, said plainly, nothing deleted, and the retry does it all.
	rec = accountDeletePost(t, lens, "/v1/account/delete", identity)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "saved answers") || strings.Contains(rec.Body.String(), "redis") {
		t.Fatalf("answers not removable: %d %s, want 503 saying so, without the cause", rec.Code, rec.Body)
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM workspaces WHERE id = $1`, ws).Scan(&n)
	if n != 1 || billed != 0 {
		t.Fatalf("a deletion whose answers stayed deleted the workspace (%d left) or cancelled its subscription (%d)", n, billed)
	}
	answers.fail = false
	rec = accountDeletePost(t, lens, "/v1/account/delete", identity)
	if rec.Code != http.StatusOK || fmt.Sprint(answers.removed) != "["+ws+"]" || billed != 1 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	for table, q := range map[string]string{
		"workspaces":         `SELECT count(*) FROM workspaces WHERE id = $1`,
		"workspace_api_keys": `SELECT count(*) FROM workspace_api_keys WHERE workspace_id = $1`,
		"api_keys":           `SELECT count(*) FROM api_keys WHERE workspace_id = $1`,
		"lxc_balances":       `SELECT count(*) FROM lxc_balances WHERE workspace_id = $1`,
		"token_events":       `SELECT count(*) FROM token_events WHERE workspace_id = $1`,
	} {
		if err := pool.QueryRow(ctx, q, ws).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s still holds %d rows of the deleted account (%v)", table, n, err)
		}
	}
	var left []string
	_ = pool.QueryRow(ctx, `SELECT COALESCE(array_agg(id ORDER BY id), '{}') FROM room_messages`).Scan(&left)
	if strings.Join(left, ",") != "m-theirs" {
		t.Fatalf("room messages left: %v, want only another workspace's, m-theirs", left)
	}
	// Another workspace's request for its money does not stop the deletion: it is declined, and kept for them.
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM agent_money_requests WHERE id = 'req-5237'`).Scan(&status); err != nil || status != "declined" {
		t.Fatalf("the other workspace's pending request: %q (%v), want declined", status, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM account_deletions WHERE workspace_id = $1`, ws).Scan(&n); err != nil || n != 1 {
		t.Fatalf("account_deletions rows: %d (%v), want 1", n, err)
	}
	// A request still running when it committed writes a usage row after: the hourly job, as lens_retention, deletes it.
	if _, err := pool.Exec(ctx, `INSERT INTO token_events (provider, model, input_tokens, output_tokens, workspace_id, prompt_text)
		VALUES ('openai', 'gpt', 1, 1, $1, 'late words')`, ws); err != nil {
		t.Fatal(err)
	}
	if gone, err := audit.DeleteDeletedAccountsEvents(ctx, accountDeleteAs(t, pool, dbmigrate.RetentionRole)); err != nil || gone != 1 {
		t.Fatalf("the hourly job deleted %d usage rows of the deleted account (%v), want 1", gone, err)
	}

	// Every key and token is refused, by this Lens and by one started afterwards.
	restarted, _ := accountDeleteLens(t, pool, jwtKey, accountDeleteDeps{})
	for _, l := range []struct {
		name string
		h    http.Handler
	}{{"the same Lens", lens}, {"a restarted Lens", restarted}} {
		for name, cred := range map[string]string{"session token": prov.Token, "workspace key": wsKey, "api key": apiKey} {
			if code := accountDeleteAuthed(l.h, cred); code != http.StatusUnauthorized {
				t.Errorf("%s: the deleted account's %s gets %d, want 401", l.name, name, code)
			}
		}
	}
	// A Connect token is refused too, and so is a token a sign-in racing the deletion minted after it.
	late, err := auth.GenerateToken(ws, "u", nil, jwtKey, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if code := accountDeletePay(lens, ws, "ag-pay", connect); code != http.StatusUnauthorized {
		t.Errorf("the deleted account's Connect token gets %d, want 401", code)
	}
	if code := accountDeleteAuthed(lens, late); code != http.StatusUnauthorized {
		t.Errorf("a token minted for the deleted account after its deletion gets %d, want 401", code)
	}
	if rec = accountDeletePost(t, lens, "/v1/account/delete", identity); rec.Code != http.StatusNotFound {
		t.Errorf("deleting again: %d, want 404", rec.Code)
	}

	// Signing up again later is a new account, and its new token works.
	time.Sleep(1100 * time.Millisecond) // a token's issue time is in whole seconds
	rec = accountDeletePost(t, restarted, "/v1/provision", identity)
	_ = json.Unmarshal(rec.Body.Bytes(), &prov)
	if rec.Code != http.StatusOK || !prov.Created || prov.WorkspaceID == ws || accountDeleteAuthed(restarted, prov.Token) != http.StatusOK {
		t.Fatalf("signing up again: %d created=%v %s, want a new workspace, not %s", rec.Code, prov.Created, rec.Body, ws)
	}
}

// accountDeleteAnswers is the cached answers' store, failing while fail is set.
type accountDeleteAnswers struct {
	fail    bool
	removed []string
}

func (a *accountDeleteAnswers) DeleteAllOf(_ context.Context, ids []string) (storedanswers.Counts, error) {
	if a.fail {
		return storedanswers.Counts{}, errors.New("redis: connection refused")
	}
	a.removed = append(a.removed, ids...)
	return storedanswers.Counts{}, nil
}

// accountDeleteNode is a chain node: every account holds *coin of the network's coin and *token of every token; it
// answers 500 to every call while *down is "all", and to that method's while it names one. It refuses eth_getLogs from
// the first block, as a live node does.
func accountDeleteNode(t *testing.T, coin, token *atomic.Int64, down *atomic.Value) string {
	t.Helper()
	down.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if d := down.Load().(string); d == "all" || d == in.Method {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var result any = "0x10"
		switch in.Method {
		case "eth_getBalance":
			result = fmt.Sprintf("0x%x", coin.Load())
		case "eth_getLogs":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": in.ID,
				"error": map[string]any{"code": -32005, "message": "query exceeds max block range 10000"}})
			return
		case "eth_call":
			result = fmt.Sprintf("0x%064x", token.Load())
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": in.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// accountDeleteSolana is a Solana node whose every wallet holds *amount of USDC, and an airdrop nobody can value.
type accountDeleteSolana struct{ amount *atomic.Int64 }

func (s accountDeleteSolana) Holdings(context.Context, string) ([]holdings.Held, error) {
	return []holdings.Held{{Address: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", Amount: big.NewInt(s.amount.Load())},
		{Address: "AirdropMint1111111111111111111111111111111", Amount: big.NewInt(1000)}}, nil
}

// A person with an agent, its wallet, a mandate, an invoice, a card, a smart account, rooms and listings: what money
// keeps is kept, what others share stays theirs, and what still holds or owes money stops the deletion first.
func TestB5237_DeletionKeepsMoneyRecordsAndOthersRooms_RealPG(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	jwtKey, err := auth.GenerateECKey()
	if err != nil {
		t.Fatal(err)
	}
	var coin, token, onSolana atomic.Int64
	var down atomic.Value
	node := accountDeleteNode(t, &coin, &token, &down)
	store := economy.NewDualTokenStore(nil, pool, nil)
	// The factory's account has one address on every EVM network: each is read. Lens reading only some is not enough.
	var nets []economy.ChainNetwork
	for _, n := range holdings.Networks {
		if n.ChainID != 0 {
			nets = append(nets, economy.ChainNetwork{Name: n.ID, USDC: "0x00000000000000000000000000000000000000aa",
				Confirmations: 1, Funding: economy.FundingTest, Chain: smartaccount.Chain{RPCURL: node}})
		}
	}
	x, err := store.NewChainIndexer(nets)
	if err != nil {
		t.Fatal(err)
	}
	xBaseOnly, err := store.NewChainIndexer(nets[:1])
	if err != nil {
		t.Fatal(err)
	}
	reading := x
	var solana holdings.Reader = accountDeleteSolana{&onSolana}
	billingErr := errors.New("stripe: card_error")
	var cancelled []string
	lens, _ := accountDeleteLens(t, pool, jwtKey, accountDeleteDeps{
		chain: func(ctx context.Context, ws string) ([]string, error) {
			return store.SmartAccountsHolding(ctx, reading, solana, ws)
		},
		billing: func(_ context.Context, ws string) error {
			if billingErr != nil {
				return billingErr
			}
			cancelled = append(cancelled, ws)
			return nil
		}})
	const identity = "https://issuer.example|person-5237-money"
	rec := accountDeletePost(t, lens, "/v1/provision", identity)
	var prov provisionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &prov)
	ws := prov.WorkspaceID
	if rec.Code != http.StatusOK || ws == "" {
		t.Fatalf("provision: %d %s", rec.Code, rec.Body)
	}
	for _, q := range []string{
		`INSERT INTO agent_accounts (id, workspace_id, name, handle) VALUES ('ag-5237', $1, 'Ada', 'ada-5237')`,
		`INSERT INTO agent_account_keys (scoped_key_id, agent_id) VALUES ('k-5237', 'ag-5237')`,
		`INSERT INTO money_accounts (id, workspace_id, agent_id, currency, purpose) VALUES ('ma-5237', $1, 'ag-5237', 'EUR', 'agent')`,
		`INSERT INTO money_mandates (id, workspace_id, agent_id, account_id, currency, max_per_pull_minor, max_per_month_minor, expires_at,
			payee_workspace_id) VALUES ('mm-5237', $1, 'ag-5237', 'ma-5237', 'EUR', 10, 100, now() + interval '1 day', 'u-other')`,
		`INSERT INTO money_invoices (id, workspace_id, agent_id, account_id, currency, number, reference, pay_token, customer_name, lines,
			subtotal_minor, vat_minor, total_minor, due_date, status)
			VALUES ('mi-5237', $1, 'ag-5237', 'ma-5237', 'EUR', 1, 'INV-1', 'tok-5237', 'C', '[]', 100, 0, 100, current_date, 'paid')`,
		`INSERT INTO money_reusable_cards (id, workspace_id, idempotency_key, currency, funding, account_id, wallet_ref, status,
			per_payment_limit_minor, daily_limit_minor) VALUES ('rcard-5237', $1, 'i', 'EUR', 'test', 'ma-5237', 'w', 'active', 10, 100)`,
		`INSERT INTO agent_smart_accounts (agent_id, workspace_id, address, factory, implementation, owner_credential_id, owner_public_key,
			agent_signer, provider, solana_address) VALUES ('ag-5237', $1, '0x1111111111111111111111111111111111111111', 'f', 'i', 'cred',
			decode(repeat('00', 64), 'hex'), 's', 'test', 'sol')`,
		`INSERT INTO subscriptions (workspace_id, stripe_subscription_id, stripe_customer_id, price_id, status, livemode, last_event_at)
			VALUES ($1, 'sub_5237', 'cus_5237', 'price_5237', 'active', false, now())`,
		`INSERT INTO money_payout_details (workspace_id, asset, holder, iban, partner, kyc_ref, status)
			VALUES ($1, 'EUR', 'Ada', 'DE00', 'test', 'kyc', 'verified')`,
		`INSERT INTO money_earnings_payouts (id, workspace_id, kind, source, recipient_workspace_id, amount_minor, currency, funding,
			account_id, hold_account_id, status) VALUES ('mpo-5237', 'u-other', 'sale', 's', $1, 100, 'EUR', 'test', 'a', 'h', 'pending')`,
		`INSERT INTO market_listings (id, workspace_id, kind, title) VALUES ('ml-mine', $1, 'prompt', 'P'), ('ml-theirs', 'u-other', 'prompt', 'T'),
			('ml-alone', $1, 'prompt', 'A')`,
		`INSERT INTO market_lineage (id, child_listing_id, child_version, parent_listing_id, parent_version, share_bps, source)
			VALUES ('lin-5237', 'ml-theirs', 1, 'ml-mine', 1, 1000, 'declared')`,
		// A room someone else is in passes to its earliest member that is not a viewer; one with only a viewer is closed,
		// and kept for them.
		`INSERT INTO rooms (id, owner_workspace_id, title, wallet_agent_id) VALUES ('room-held', $1, 'r', 'ag-5237'), ('room-alone', $1, 'r', NULL)`,
		`INSERT INTO room_members (room_id, workspace_id, role, may_spend, terms_version, joined_at) VALUES
			('room-held', $1, 'owner', true, 1, now() - interval '3 days'),
			('room-held', 'u-viewer', 'viewer', false, 1, now() - interval '2 days'),
			('room-held', 'u-heir', 'member', false, 1, now() - interval '1 day'),
			('room-alone', $1, 'owner', true, 1, now()),
			('room-alone', 'u-viewer', 'viewer', false, 1, now())`,
	} {
		args := []any{ws}
		if !strings.Contains(q, "$1") {
			args = nil
		}
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// Anything in a smart account — the network's coin, a token sent to it, its Solana wallet — read on the chain:
	// refused, and told how to move it out.
	for _, held := range []struct {
		what string
		on   *atomic.Int64
		at   string
	}{{"its coin", &coin, "on optimism"}, {"a token", &token, "on base"}, {"a Solana token", &onSolana, "sol on solana"}} {
		held.on.Store(5)
		rec = accountDeletePost(t, lens, "/v1/account/delete", identity)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), held.at) || !strings.Contains(rec.Body.String(), "Sweep") {
			t.Fatalf("a smart account holding %s: %d %s, want 409 naming it and saying to sweep it", held.what, rec.Code, rec.Body)
		}
		held.on.Store(0)
	}
	// What cannot be read is not empty: a node that fails, a token's balance it fails to read, a network Lens does not
	// read, or a Solana wallet with no reader, refuses, and the node's URL (its key) never reaches the caller.
	for _, d := range []string{"all", "eth_call"} {
		down.Store(d)
		rec = accountDeletePost(t, lens, "/v1/account/delete", identity)
		if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "127.0.0.1") {
			t.Fatalf("a node failing %s: %d %s, want 503 without its URL", d, rec.Code, rec.Body)
		}
	}
	down.Store("")
	reading = xBaseOnly
	if rec = accountDeletePost(t, lens, "/v1/account/delete", identity); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("smart accounts on networks Lens does not read: %d %s, want 503", rec.Code, rec.Body)
	}
	reading = x
	solana = nil
	if rec = accountDeletePost(t, lens, "/v1/account/delete", identity); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a Solana wallet with nothing to read it: %d %s, want 503", rec.Code, rec.Body)
	}
	solana = accountDeleteSolana{&onSolana}

	// Earnings still being paid out refuse.
	rec = accountDeletePost(t, lens, "/v1/account/delete", identity)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "earnings") {
		t.Fatalf("delete with earnings open: %d %s, want 409 naming them", rec.Code, rec.Body)
	}
	if _, err := pool.Exec(ctx, `UPDATE money_earnings_payouts SET status = 'completed' WHERE id = 'mpo-5237'`); err != nil {
		t.Fatal(err)
	}
	// Stripe does not confirm the subscription's cancellation: refused, plainly, and nothing is deleted.
	rec = accountDeletePost(t, lens, "/v1/account/delete", identity)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "subscription") || strings.Contains(rec.Body.String(), "card_error") {
		t.Fatalf("Stripe refusing the cancellation: %d %s, want 503 saying so, without Stripe's words", rec.Code, rec.Body)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM workspaces WHERE id = $1`, ws).Scan(&n)
	if n != 1 {
		t.Fatal("a deletion whose subscription was not cancelled deleted the workspace")
	}
	billingErr = nil

	rec = accountDeletePost(t, lens, "/v1/account/delete", identity)
	if rec.Code != http.StatusOK || fmt.Sprint(cancelled) != "["+ws+"]" {
		t.Fatalf("delete: %d %s, subscriptions cancelled for %v", rec.Code, rec.Body, cancelled)
	}
	one := func(what, q string, want any) {
		t.Helper()
		var got any
		args := []any{ws}
		if !strings.Contains(q, "$1") {
			args = nil
		}
		if err := pool.QueryRow(ctx, q, args...).Scan(&got); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: %v, want %v", what, got, want)
		}
	}
	// The agent a kept money record names stays, without its name or keys; the records stay as they were.
	one("the kept agent", `SELECT name || '|' || COALESCE(handle, '') || '|' || (archived_at IS NOT NULL)::text FROM agent_accounts WHERE id = 'ag-5237'`, "Deleted agent||true")
	one("the agent's keys", `SELECT count(*) FROM agent_account_keys WHERE agent_id = 'ag-5237'`, 0)
	one("its wallet, mandate and invoice", `SELECT (SELECT count(*) FROM money_accounts WHERE id = 'ma-5237') + (SELECT count(*) FROM money_mandates
		WHERE id = 'mm-5237' AND revoked_at IS NOT NULL) + (SELECT count(*) FROM money_invoices WHERE id = 'mi-5237')`, 3)
	one("the reusable card", `SELECT status FROM money_reusable_cards WHERE id = 'rcard-5237'`, "frozen")
	one("smart account, payout details, subscription", `SELECT (SELECT count(*) FROM agent_smart_accounts WHERE workspace_id = $1) +
		(SELECT count(*) FROM money_payout_details WHERE workspace_id = $1) + (SELECT count(*) FROM subscriptions WHERE workspace_id = $1)`, 0)
	// A listing another's is built on stays, for the royalty record (append-only), and is taken down.
	one("the listings", `SELECT string_agg(id || ':' || review_status, ',' ORDER BY id) FROM market_listings WHERE id LIKE 'ml-%'`,
		"ml-mine:taken_down,ml-theirs:approved")
	one("the room someone else is in", `SELECT owner_workspace_id || '|' || status || '|' || (wallet_agent_id IS NULL)::text FROM rooms WHERE id = 'room-held'`, "u-heir|open|true")
	one("its members", `SELECT string_agg(workspace_id || ':' || role, ',' ORDER BY workspace_id) FROM room_members WHERE room_id = 'room-held'`, "u-heir:owner,u-viewer:viewer")
	one("the room with only a viewer", `SELECT owner_workspace_id || '|' || status FROM rooms WHERE id = 'room-alone'`, "|closed")
	one("its viewer", `SELECT count(*) FROM room_members WHERE room_id = 'room-alone' AND workspace_id = 'u-viewer'`, 1)

	// Signing up again is a new workspace: none of what the deleted one kept is its, so its frozen card stays frozen.
	rec = accountDeletePost(t, lens, "/v1/provision", identity)
	var again provisionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &again)
	if rec.Code != http.StatusOK || again.WorkspaceID == "" || again.WorkspaceID == ws {
		t.Fatalf("signing up again: %d %s, want a new workspace, not %s", rec.Code, rec.Body, ws)
	}
	ws = again.WorkspaceID
	one("what the new workspace inherits", `SELECT (SELECT count(*) FROM agent_accounts WHERE workspace_id = $1) +
		(SELECT count(*) FROM money_accounts WHERE workspace_id = $1) + (SELECT count(*) FROM money_reusable_cards WHERE workspace_id = $1)`, 0)
}

// A deletion waits for money already moving in the workspace (its money lock) and then sees it: a balance written
// while it waited is not orphaned.
func TestB5237_DeletionWaitsForMoneyInFlight_RealPG(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	jwtKey, err := auth.GenerateECKey()
	if err != nil {
		t.Fatal(err)
	}
	lens, _ := accountDeleteLens(t, pool, jwtKey, accountDeleteDeps{})
	const identity = "https://issuer.example|person-5237-race"
	rec := accountDeletePost(t, lens, "/v1/provision", identity)
	var prov provisionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &prov)

	moving, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = moving.Rollback(ctx) }()
	if _, err := moving.Exec(ctx, economy.FreezeLockSQL, prov.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- accountDeletePost(t, lens, "/v1/account/delete", identity) }()
	select {
	case rec := <-done:
		t.Fatalf("the deletion did not wait for the money moving: %d %s", rec.Code, rec.Body)
	case <-time.After(500 * time.Millisecond):
	}
	if _, err := moving.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance) VALUES ($1, 5)`, prov.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if err := moving.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if rec := <-done; rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "credits") {
		t.Fatalf("after the money landed: %d %s, want 409 naming the credits", rec.Code, rec.Body)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM lxc_balances WHERE workspace_id = $1 AND balance = 5`, prov.WorkspaceID).Scan(&n)
	if n != 1 {
		t.Fatalf("the balance that landed: %d rows, want 1", n)
	}
}
