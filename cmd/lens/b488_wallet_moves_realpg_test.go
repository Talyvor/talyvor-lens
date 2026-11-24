package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/tenant"
)

// b488Ramp is the Test ramp on a clock the test moves on (late), which answers its next execution pending once.
type b488Ramp struct {
	partners.TestRampPartner
	late     time.Duration
	pendNext bool
	downNext bool                 // the next execution gets no answer, never made
	expires  map[string]time.Time // by quote
	executed map[string]bool      // by execution
}

func (r *b488Ramp) Quote(ctx context.Context, req partners.RampQuoteRequest) (partners.RampQuote, error) {
	q, err := r.TestRampPartner.Quote(ctx, req)
	q.ExpiresAt = q.ExpiresAt.Add(r.late)
	r.expires[q.Ref] = q.ExpiresAt
	return q, err
}

func (r *b488Ramp) Execute(ctx context.Context, req partners.RampExecution) (partners.Result, error) {
	if r.downNext {
		r.downNext = false
		return partners.Result{}, errors.New("the ramp did not answer")
	}
	if !r.executed[req.ID] && time.Now().Add(r.late).After(r.expires[req.QuoteRef]) {
		return partners.Result{}, fmt.Errorf("%w: quote %s has expired", partners.ErrInvalid, req.QuoteRef)
	}
	res, err := r.TestRampPartner.Execute(ctx, req)
	r.executed[req.ID] = err == nil
	if err == nil && r.pendNext {
		r.pendNext, res.Status = false, partners.StatusPending
	}
	return res, err
}

// b488Transfers is the sandbox's transfers, whose next pay-in answers pending once, having moved the money. It notes,
// as each pay-in starts, the stage another connection reads for its move and whether the call's context is alive.
type b488Transfers struct {
	money.Transfers
	pool     *pgxpool.Pool
	pendNext bool
	stage    string
	alive    bool
	// refuseCredit refuses money arriving, as Circle does a credit whose amount does not match; credits notes each
	// credit made, by key.
	refuseCredit bool
	credits      map[string]money.Amount
	// lost answers the next pay-in with no answer, never having made it.
	lost bool
}

func (t *b488Transfers) Credit(ctx context.Context, req money.Credit) (money.Result, error) {
	if t.refuseCredit {
		return money.Result{}, fmt.Errorf("%w: the amount does not match what arrived", money.ErrRefused)
	}
	res, err := t.Transfers.Credit(ctx, req)
	if err == nil {
		t.credits[req.Key] = req.Amount
	}
	return res, err
}

func (t *b488Transfers) Outbound(ctx context.Context, req money.Outbound) (money.Result, error) {
	t.stage, t.alive = "", ctx.Err() == nil
	if t.lost {
		t.lost = false
		return money.Result{}, errors.New("no answer")
	}
	_ = t.pool.QueryRow(context.Background(), `SELECT stage FROM wallet_moves WHERE id = $1`, req.Key).Scan(&t.stage)
	res, err := t.Transfers.Outbound(ctx, req)
	if err == nil && t.pendNext {
		t.pendNext, res.Status = false, money.StatusPending
	}
	return res, err
}

// b488TravelRuleDown is a Travel Rule provider that names the same providers and sends nothing.
type b488TravelRuleDown struct {
	*partners.TestTravelRuleProvider
}

func (b488TravelRuleDown) Send(context.Context, partners.TravelRuleTransfer) (string, error) {
	return "", errors.New("the Travel Rule network is down")
}

// B48.8 — through the sandbox adapter and the Test ramp: an owner tops an agent's wallet up by paying into it, from
// their own wallet or another of their agents' (no approval), with a card or a bank transfer, or by converting a fiat
// wallet; and cashes it out to their own checked bank account. Each is quoted first and posts exactly once, at the
// quoted rate and fee, in both wallets' ledger.
func TestB488_TopUpAndCashOutAnAgentsWalletPostOnceAtTheQuote(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, secret = "ws-b488", "whsec_b488"
	store := economy.NewDualTokenStore(nil, pool, nil)
	reg := partners.NewRegistry(nil)
	store.SetAccountPartners(reg)
	store.SetScreener(screening.NewScreener(pool, reg))
	ramp := &b488Ramp{expires: map[string]time.Time{}, executed: map[string]bool{}}
	store.SetRampPartner(ramp)
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
	call := func(method, path, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code >= 300 {
			t.Logf("%s %s = %d %s", method, path, w.Code, w.Body.String())
		}
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
	ops := create(`{"name":"Ops","wallet_kind":"both"}`)
	trader := create(`{"name":"Trader","wallet_kind":"crypto"}`)
	stranger := create(`{"name":"Someone else's","wallet_kind":"crypto"}`)
	if _, err := pool.Exec(ctx, `UPDATE agent_accounts SET owner_user_id = 'someone-else' WHERE id = $1`, stranger.ID); err != nil {
		t.Fatal(err)
	}
	// Any payment of Trader's above 0.000001 USDC would wait for its owner.
	if code := call(http.MethodPut, base+"/"+trader.ID+"/rules", `{"approval_above_ulxc":1}`, nil); code != http.StatusOK {
		t.Fatalf("Trader's rules = %d", code)
	}
	var ownerWallets economy.AgentWallets
	if code := call(http.MethodGet, base+"/owner-wallets", "", &ownerWallets); code != http.StatusOK || len(ownerWallets.Wallets) != 2 {
		t.Fatalf("owner wallets = %d %+v", code, ownerWallets)
	}
	account := func(agentID, asset string) string {
		t.Helper()
		var id string
		q := `SELECT id FROM money_accounts WHERE workspace_id = $1 AND currency = $2 AND status = 'open' AND `
		args := []any{ws, asset}
		if agentID == "" {
			q += `purpose = 'company'`
		} else {
			q, args = q+`purpose = 'agent' AND agent_id = $3`, append(args, agentID)
		}
		if err := pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("%s's %s account: %v", agentID, asset, err)
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
	// (a) Money paid into a wallet's details posts once, however often the partner says so.
	var partnerRef string
	notify := func(accountID string, minor int64, asset, ref string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT partner_account_ref FROM money_accounts WHERE id = $1`, accountID).Scan(&partnerRef); err != nil {
			t.Fatal(err)
		}
		for i := range 2 { // the same payment in two notifications
			body := []byte(fmt.Sprintf(`{"id":"evt_%s_%d","to":%q,"amount":{"amount_minor":%d,"asset":%q},"partner_ref":%q,"payer":"Acme"}`,
				ref, i, partnerRef, minor, asset, ref))
			req := httptest.NewRequest(http.MethodPost, "/v1/money/webhooks/sandbox", bytes.NewReader(body))
			req.Header.Set(money.SandboxSignatureHeader, money.SignSandbox(secret, body, time.Now()))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("deposit = %d %s", w.Code, w.Body.String())
			}
		}
	}
	deposit := func(accountID string, minor int64, asset, ref string) {
		t.Helper()
		notify(accountID, minor, asset, ref)
		// The partner holds it too: the sandbox spends only what it was credited.
		c := economy.CapabilityStablecoins
		if asset != money.USDC {
			c = economy.CapabilityCurrencyAccounts
		}
		tr, err := layer.Transfers(ctx, money.Route{Workspace: ws, Capability: c})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tr.Credit(ctx, money.Credit{Key: ref, To: partnerRef, Amount: money.Amount{Minor: minor, Asset: asset}, PartnerRef: ref}); err != nil {
			t.Fatal(err)
		}
	}
	ownerUSDC, ownerEUR := account("", money.USDC), account("", money.EUR)
	opsUSDC, traderUSDC := account(ops.ID, money.USDC), account(trader.ID, money.USDC)
	deposit(ownerUSDC, 100_000_000, money.USDC, "in_owner_usdc")
	deposit(ownerEUR, 20_000, money.EUR, "in_owner_eur")
	deposit(traderUSDC, 50_000_000, money.USDC, "in_trader")
	if held(ownerUSDC) != 100_000_000 || held(ownerEUR) != 20_000 || held(traderUSDC) != 50_000_000 {
		t.Fatalf("deposits told twice hold %d, %d, %d; want each once", held(ownerUSDC), held(ownerEUR), held(traderUSDC))
	}

	quote := func(agentID, body string, want int) economy.WalletMove {
		t.Helper()
		var m economy.WalletMove
		if code := call(http.MethodPost, base+"/"+agentID+"/wallet-moves/quote", body, &m); code != want {
			t.Fatalf("quote %s = %d, want %d", body, code, want)
		}
		return m
	}
	// moveEntry finds the entry move $2 posted: under its own key, or a top-up's under the deposit that delivered it.
	const moveEntry = `(e.idempotency_key = 'wallet_move:' || $2 OR e.id = (SELECT NULLIF(entry_id, '') FROM wallet_moves WHERE id = $2))`
	// confirm makes m twice — a retry — and checks it posted one entry, whose postings are want, at the quote.
	confirm := func(m economy.WalletMove, want map[string]int64) {
		t.Helper()
		for i := range 2 {
			var got economy.WalletMove
			path := base + "/" + m.AgentID + "/wallet-moves/" + m.ID + "/confirm"
			if code := call(http.MethodPost, path, "", &got); code != http.StatusOK || got.Status != "posted" || got.EntryID == "" ||
				got.BuyMinor != m.BuyMinor || got.FeeMinor != m.FeeMinor || got.Rate != m.Rate {
				t.Fatalf("confirm %s (%d) = %d %+v; want it posted at its quote %+v", m.Route, i, code, got, m)
			}
		}
		rows, err := pool.Query(ctx, `SELECT p.account_id, p.amount_minor FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
			WHERE e.workspace_id = $1 AND `+moveEntry, ws, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int64{}
		for rows.Next() {
			var id string
			var n int64
			if err := rows.Scan(&id, &n); err != nil {
				t.Fatal(err)
			}
			got[id] += n
		}
		for id, n := range want {
			if got[id] != n {
				t.Fatalf("%s posted %+v; want %s to move %d once", m.Route, got, id, n)
			}
		}
		var entries int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_entries e WHERE workspace_id = $1 AND `+moveEntry, ws, m.ID).
			Scan(&entries); err != nil || entries != 1 {
			t.Fatalf("%s posted %d entries (%v), want 1", m.Route, entries, err)
		}
	}

	// (b) From the owner's wallet, and from another of their agents — whose rules would hold any payment for approval —
	// in one tap, at par and no fee; never from an agent another person owns.
	m := quote(ops.ID, `{"route":"wallet","amount_minor":20000000,"currency":"USDC"}`, http.StatusOK)
	if m.Rate != "1" || m.FeeMinor != 0 || m.BuyMinor != 20_000_000 || m.Status != "quoted" {
		t.Fatalf("owner-to-agent quote = %+v", m)
	}
	confirm(m, map[string]int64{ownerUSDC: -20_000_000, opsUSDC: 20_000_000})
	m = quote(ops.ID, fmt.Sprintf(`{"route":"wallet","amount_minor":5000000,"currency":"USDC","from_agent_id":%q}`, trader.ID), http.StatusOK)
	confirm(m, map[string]int64{traderUSDC: -5_000_000, opsUSDC: 5_000_000})
	var approvals int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_approvals WHERE workspace_id = $1`, ws).Scan(&approvals); err != nil || approvals != 0 {
		t.Fatalf("approvals filed for moves between the owner's own wallets = %d (%v), want 0", approvals, err)
	}
	quote(ops.ID, fmt.Sprintf(`{"route":"wallet","amount_minor":1000000,"currency":"USDC","from_agent_id":%q}`, stranger.ID), http.StatusBadRequest)
	// An admin moves no money out of the owner's agents, nor out of the workspace's own wallets into an agent the admin
	// owns: that would let them cash the workspace's money out.
	admins := stranger
	if _, err := pool.Exec(ctx, `UPDATE agent_accounts SET owner_user_id = 'an-admin' WHERE id = $1`, admins.ID); err != nil {
		t.Fatal(err)
	}
	fromOwners := quote(ops.ID, `{"route":"wallet","amount_minor":1000000,"currency":"USDC"}`, http.StatusOK)
	owner.UserID, owner.AuthMethod, owner.Scopes = "an-admin", auth.MethodWorkspaceKey, []string{auth.ScopeAdmin}
	// #1001 review 7: nor confirms or returns one the owner quoted.
	for _, step := range []string{"confirm", "return"} {
		if code := call(http.MethodPost, base+"/"+ops.ID+"/wallet-moves/"+fromOwners.ID+"/"+step, "", nil); code != http.StatusBadRequest ||
			held(ownerUSDC) != 80_000_000 {
			t.Fatalf("an admin's %s of the owner's move = %d, owner wallet %d; want 400, nothing moved", step, code, held(ownerUSDC))
		}
	}
	quote(ops.ID, fmt.Sprintf(`{"route":"wallet","amount_minor":1000000,"currency":"USDC","from_agent_id":%q}`, trader.ID), http.StatusBadRequest)
	quote(admins.ID, `{"route":"wallet","amount_minor":1000000,"currency":"USDC"}`, http.StatusBadRequest)
	quote(admins.ID, `{"route":"convert","amount_minor":1000,"currency":"EUR"}`, http.StatusBadRequest)
	owner.UserID, owner.AuthMethod, owner.Scopes = "owner", auth.MethodJWT, []string{auth.ScopeKeys}

	// (c) Bought with a card (Test fee 3%) and with a bank transfer (1%), at the Test rates: EUR 1.10, GBP 1.25 a dollar.
	m = quote(ops.ID, `{"route":"card","amount_minor":10000,"currency":"EUR"}`, http.StatusOK)
	if m.FeeMinor != 300 || m.BuyMinor != 106_700_000 || m.BuyCurrency != money.USDC || m.ExpiresAt == nil {
		t.Fatalf("card quote = %+v; want a 3.00 EUR fee buying 106.70 USDC", m)
	}
	confirm(m, map[string]int64{opsUSDC: 106_700_000})
	// #1001 review 2: the ramp's delivery, reported again by the money layer's deposit webhook, posts nothing more.
	var deliveredRef string
	if err := pool.QueryRow(ctx, `SELECT e.partner_ref FROM money_entries e JOIN wallet_moves w ON w.entry_id = e.id WHERE w.id = $1`, m.ID).
		Scan(&deliveredRef); err != nil {
		t.Fatal(err)
	}
	before := held(opsUSDC)
	notify(opsUSDC, m.BuyMinor, money.USDC, deliveredRef)
	if held(opsUSDC) != before {
		t.Fatalf("the deposit webhook for a delivered card top-up moved Ops %d → %d; want it posted once", before, held(opsUSDC))
	}
	m = quote(trader.ID, `{"route":"bank_transfer","amount_minor":5000,"currency":"GBP"}`, http.StatusOK)
	if m.FeeMinor != 50 || m.BuyMinor != 61_875_000 {
		t.Fatalf("bank transfer quote = %+v; want a 0.50 GBP fee buying 61.875 USDC", m)
	}
	confirm(m, map[string]int64{traderUSDC: 61_875_000})

	// (d) Converted from the owner's euro wallet (Test fee 0.5%).
	m = quote(ops.ID, `{"route":"convert","amount_minor":10000,"currency":"EUR"}`, http.StatusOK)
	if m.FeeMinor != 50 || m.BuyMinor != 109_450_000 {
		t.Fatalf("convert quote = %+v; want a 0.50 EUR fee buying 109.45 USDC", m)
	}
	confirm(m, map[string]int64{ownerEUR: -10_000, opsUSDC: 109_450_000})

	// Cash out: only once the owner is verified, only to their own bank account — in the verified person's name, which
	// its bank confirmed exactly.
	for id, p := range map[string][2]string{"payee_b488_exact": {"The Owner", "exact_match"}, "payee_b488_close": {"The Owner", "close_match"},
		"payee_b488_other": {"Someone Else", "exact_match"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO money_payees (id, workspace_id, name, country, iban, check_result, checked_by)
			VALUES ($1, $2, $3, 'GB', 'GB82WEST12345698765432', $4, 'test')`, id, ws, p[0], p[1]); err != nil {
			t.Fatal(err)
		}
	}
	cashOut := func(payee string) string {
		return fmt.Sprintf(`{"route":"cash_out","amount_minor":10000000,"currency":"USDC","to_currency":"GBP","payee_id":%q}`, payee)
	}
	quote(ops.ID, cashOut("payee_b488_exact"), http.StatusBadRequest)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref, verified_name)
		VALUES ($1, 2, 'person', 'test', 'completed', 'test_kyc_b488', 'The Owner')`, ws); err != nil {
		t.Fatal(err)
	}
	quote(ops.ID, cashOut("payee_b488_close"), http.StatusBadRequest)
	quote(ops.ID, cashOut("payee_b488_other"), http.StatusBadRequest)
	m = quote(ops.ID, cashOut("payee_b488_exact"), http.StatusOK)
	if m.FeeMinor != 50_000 || m.BuyMinor != 796 || m.BuyCurrency != "GBP" {
		t.Fatalf("cash-out quote = %+v; want a 0.05 USDC fee paying 7.96 GBP", m)
	}
	confirm(m, map[string]int64{opsUSDC: -10_000_000})
	// Money leaving Talyvor is judged by the agent's rules: Trader's cash-out waits for its owner's approval.
	m = quote(trader.ID, cashOut("payee_b488_exact"), http.StatusOK)
	var waiting economy.WalletMove
	if code := call(http.MethodPost, base+"/"+trader.ID+"/wallet-moves/"+m.ID+"/confirm", "", &waiting); code != http.StatusOK ||
		waiting.Status != "quoted" || !strings.Contains(waiting.Detail, "approval") {
		t.Fatalf("Trader's cash-out = %d %+v; want it waiting for approval", code, waiting)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_approvals WHERE workspace_id = $1 AND agent_id = $2`, ws, trader.ID).
		Scan(&approvals); err != nil || approvals != 1 {
		t.Fatalf("approvals for Trader's cash-out = %d (%v), want 1", approvals, err)
	}

	// Each wallet's history shows what moved it.
	routes := func(path string) string {
		t.Helper()
		var ms []economy.WalletMove
		if code := call(http.MethodGet, path, "", &ms); code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, code)
		}
		var s []string
		for _, m := range ms {
			s = append(s, m.Route)
		}
		return strings.Join(s, ",")
	}
	if got := routes(base + "/" + ops.ID + "/wallet-moves"); got != "cash_out,convert,card,wallet,wallet" {
		t.Fatalf("Ops's history = %s", got)
	}
	if got := routes(base + "/" + trader.ID + "/wallet-moves"); got != "bank_transfer,wallet" {
		t.Fatalf("Trader's history = %s", got)
	}
	if got := routes(base + "/owner-wallets/moves"); got != "convert,wallet" {
		t.Fatalf("the owner's history = %s", got)
	}
	if held(ownerUSDC) != 80_000_000 || held(ownerEUR) != 10_000 || held(traderUSDC) != 106_875_000 ||
		held(opsUSDC) != 20_000_000+5_000_000+106_700_000+109_450_000-10_000_000 {
		t.Fatalf("balances after: owner %d USDC %d EUR, Trader %d, Ops %d", held(ownerUSDC), held(ownerEUR), held(traderUSDC), held(opsUSDC))
	}

	// A move a partner began is replayed to its end however late it is confirmed: the euros reached the ramp, which
	// answered pending, and the quote ran out five minutes ago.
	late := func(m economy.WalletMove) {
		t.Helper()
		ramp.late = 5 * time.Minute
		if _, err := pool.Exec(ctx, `UPDATE wallet_moves SET expires_at = now() - interval '5 minutes' WHERE id = $1`, m.ID); err != nil {
			t.Fatal(err)
		}
	}
	tradersCashOut := waiting
	m = quote(ops.ID, `{"route":"convert","amount_minor":5000,"currency":"EUR"}`, http.StatusOK)
	ramp.pendNext = true
	var pending economy.WalletMove
	if code := call(http.MethodPost, base+"/"+ops.ID+"/wallet-moves/"+m.ID+"/confirm", "", &pending); code != http.StatusOK ||
		pending.Status != "quoted" || !strings.Contains(pending.Detail, "pending") {
		t.Fatalf("convert at a ramp answering pending = %d %+v; want it pending", code, pending)
	}
	late(m)
	confirm(m, map[string]int64{ownerEUR: -5_000, opsUSDC: m.BuyMinor})
	// The owner approves Trader's cash-out five minutes on: it is priced again, and goes.
	var approvalID string
	if err := pool.QueryRow(ctx, `SELECT id FROM agent_approvals WHERE workspace_id = $1 AND agent_id = $2`, ws, trader.ID).Scan(&approvalID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DecideAgentApproval(ctx, ws, approvalID, true); err != nil {
		t.Fatal(err)
	}
	late(tradersCashOut)
	confirm(tradersCashOut, map[string]int64{traderUSDC: -10_000_000}) // the Test ramp's price is the same five minutes on

	// A convert is marked started, where another connection sees it, before its euros are paid in to the ramp; and it
	// goes on though the owner hung up. The pay-in answers pending; confirmed after the ramp's price ran out, the move
	// is priced again and waits for the owner — never failed with the euros at the ramp — and at the new price posts once.
	m = quote(ops.ID, `{"route":"convert","amount_minor":4000,"currency":"EUR"}`, http.StatusOK)
	transfers.pendNext = true
	gone, hangUp := context.WithCancel(ctx)
	hangUp()
	if got, err := store.ConfirmWalletMove(gone, ws, ops.ID, m.ID, "owner", true); err != nil || got.Status != "quoted" || !strings.Contains(got.Detail, "pending") ||
		transfers.stage != "started" || !transfers.alive {
		t.Fatalf("convert confirmed by an owner who hung up = %+v %v (stage at the pay-in %q, alive %v); want it pending, started first",
			got, err, transfers.stage, transfers.alive)
	}
	late(m)
	ramp.late = 10 * time.Minute // the ramp refuses the quote, too
	var repriced economy.WalletMove
	if code := call(http.MethodPost, base+"/"+ops.ID+"/wallet-moves/"+m.ID+"/confirm", "", &repriced); code != http.StatusOK ||
		repriced.Status != "quoted" || repriced.ExpiresAt == nil || !repriced.ExpiresAt.After(time.Now()) || !strings.Contains(repriced.Detail, "priced again") {
		t.Fatalf("convert paid in, confirmed after its quote = %d %+v; want it priced again, waiting for the owner", code, repriced)
	}
	confirm(repriced, map[string]int64{ownerEUR: -4_000, opsUSDC: repriced.BuyMinor})

	// A bank account edited while a cash-out waits for approval is not paid until its 24-hour hold has passed.
	m = quote(trader.ID, cashOut("payee_b488_exact"), http.StatusOK)
	if code := call(http.MethodPost, base+"/"+trader.ID+"/wallet-moves/"+m.ID+"/confirm", "", &waiting); code != http.StatusOK ||
		!strings.Contains(waiting.Detail, "approval") {
		t.Fatalf("Trader's second cash-out = %d %+v; want it waiting for approval", code, waiting)
	}
	if _, err := pool.Exec(ctx, `UPDATE money_payees SET iban = 'GB33BUKB20201555555555', details_changed_at = now() WHERE id = 'payee_b488_exact'`); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM agent_approvals WHERE workspace_id = $1 AND agent_id = $2 AND status = 'pending'`, ws, trader.ID).
		Scan(&approvalID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DecideAgentApproval(ctx, ws, approvalID, true); err != nil {
		t.Fatal(err)
	}
	before = held(traderUSDC)
	if code := call(http.MethodPost, base+"/"+trader.ID+"/wallet-moves/"+m.ID+"/confirm", "", nil); code != http.StatusBadRequest || held(traderUSDC) != before {
		t.Fatalf("cash-out to a bank account edited while it waited = %d, Trader %d → %d; want it refused, nothing moved", code, before, held(traderUSDC))
	}

	// Once money has left, no refusal fails the move: it is held, where the owner and the operators see it, until it is
	// completed or the money goes back where it came from — posted exactly once, or not at all.
	confirmOnce := func(m economy.WalletMove) economy.WalletMove {
		t.Helper()
		var got economy.WalletMove
		if code := call(http.MethodPost, base+"/"+m.AgentID+"/wallet-moves/"+m.ID+"/confirm", "", &got); code != http.StatusOK {
			t.Fatalf("confirm %s %s = %d", m.Route, m.ID, code)
		}
		return got
	}
	entries := func(m economy.WalletMove) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_entries e WHERE workspace_id = $1 AND `+moveEntry, ws, m.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	isHeld := func(m economy.WalletMove) {
		t.Helper()
		ops, err := store.HeldWalletMoves(ctx)
		if err != nil || !slices.ContainsFunc(ops, func(o economy.WalletMove) bool { return o.ID == m.ID }) {
			t.Fatalf("the operators' held moves = %+v (%v); want %s among them", ops, err, m.ID)
		}
	}

	// (2) The card was charged and the ramp bought the USDC, but the money layer does not know the ramp's reference as a
	// deposit (Circle knows its own transaction id): the move waits, shown in the agent's history, never failed; the
	// deposit webhook then posts it once, as the move's delivery; confirmed again, nothing more.
	m = quote(ops.ID, `{"route":"card","amount_minor":2000,"currency":"EUR"}`, http.StatusOK)
	transfers.refuseCredit = true
	if got := confirmOnce(m); got.Status != "quoted" || !strings.Contains(got.Detail, "arrives") || entries(m) != 0 {
		t.Fatalf("card top-up whose delivery the money layer has not seen = %+v, %d entries; want it waiting, nothing posted", got, entries(m))
	}
	if got := routes(base + "/" + ops.ID + "/wallet-moves"); !strings.HasPrefix(got, "card,") {
		t.Fatalf("Ops's history = %s; want the card top-up on its way listed", got)
	}
	if code := call(http.MethodPost, base+"/"+ops.ID+"/wallet-moves/"+m.ID+"/return", "", nil); code != http.StatusConflict {
		t.Fatalf("return of a top-up the ramp bought = %d, want 409", code)
	}
	before = held(opsUSDC)
	notify(opsUSDC, m.BuyMinor, money.USDC, "circle_tx_b488_card")
	if held(opsUSDC) != before+m.BuyMinor || entries(m) != 1 {
		t.Fatalf("the ramp's delivery reported by the deposit webhook moved Ops %d → %d, %d move entries; want %d once, as the move's",
			before, held(opsUSDC), entries(m), m.BuyMinor)
	}
	confirm(m, map[string]int64{opsUSDC: m.BuyMinor})
	transfers.refuseCredit = false
	if held(opsUSDC) != before+m.BuyMinor {
		t.Fatalf("Ops %d after confirming a delivered top-up; want %d", held(opsUSDC), before+m.BuyMinor)
	}

	// #1001 review 3: a confirm that read the move before another confirm priced it again posts the price it executed.
	m = quote(ops.ID, `{"route":"convert","amount_minor":1000,"currency":"EUR"}`, http.StatusOK)
	q, err := ramp.Quote(ctx, partners.RampQuoteRequest{ID: m.ID + ".other", Sell: partners.Money{Minor: 1000, Currency: money.EUR},
		Buy: money.USDC, Method: partners.RampCard})
	if err != nil || q.Buy.Minor == m.BuyMinor {
		t.Fatalf("a second price = %+v %v", q, err)
	}
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('wallet_moves:' || $1, 0))`, m.ID); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		m   economy.WalletMove
		err error
	}
	done := make(chan answer, 1)
	go func() {
		got, err := store.ConfirmWalletMove(ctx, ws, ops.ID, m.ID, "owner", true)
		done <- answer{got, err}
	}()
	for waiting := false; !waiting; {
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND NOT granted)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := lock.Exec(ctx, `UPDATE wallet_moves SET quote_ref = $2, fee_minor = $3, buy_minor = $4, rate = $5 WHERE id = $1`,
		m.ID, q.Ref, q.Fee.Minor, q.Buy.Minor, q.Rate); err != nil {
		t.Fatal(err)
	}
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if a := <-done; a.err != nil || a.m.Status != "posted" || a.m.BuyMinor != q.Buy.Minor {
		t.Fatalf("convert confirmed while priced again = %+v %v; want it posted at %d", a.m, a.err, q.Buy.Minor)
	}
	m.BuyMinor, m.FeeMinor, m.Rate = q.Buy.Minor, q.Fee.Minor, q.Rate
	confirm(m, map[string]int64{ownerEUR: -1_000, opsUSDC: q.Buy.Minor})

	// #1001 review 4: the ramp delivered but the wallet was closed meanwhile: held, never a bare error; #1001 review 5: the
	// ramp, asked for the money back, says it executed whatever executed_at says; reopened, it posts once.
	m = quote(ops.ID, `{"route":"card","amount_minor":1500,"currency":"EUR"}`, http.StatusOK)
	if _, err := pool.Exec(ctx, `UPDATE money_accounts SET status = 'closed' WHERE id = $1`, opsUSDC); err != nil {
		t.Fatal(err)
	}
	if got := confirmOnce(m); got.Status != "held" || entries(m) != 0 {
		t.Fatalf("card top-up delivered into a closed wallet = %+v, %d entries; want it held, nothing posted", got, entries(m))
	}
	isHeld(m)
	if _, err := pool.Exec(ctx, `UPDATE wallet_moves SET executed_at = NULL WHERE id = $1`, m.ID); err != nil {
		t.Fatal(err)
	}
	if code := call(http.MethodPost, base+"/"+ops.ID+"/wallet-moves/"+m.ID+"/return", "", nil); code != http.StatusConflict {
		t.Fatalf("return of a top-up the ramp executed = %d, want 409", code)
	}
	if _, err := pool.Exec(ctx, `UPDATE money_accounts SET status = 'open' WHERE id = $1`, opsUSDC); err != nil {
		t.Fatal(err)
	}
	confirm(m, map[string]int64{opsUSDC: m.BuyMinor})

	// (3) A bank account edited after a started cash-out was quoted: held with the USDC at the ramp — never failed,
	// never paid to the new details within 24 hours. The owner confirms again once the hold has passed, and it is paid;
	// or asks for the money back, and it is back, nothing posted.
	if _, err := pool.Exec(ctx, `INSERT INTO money_payees (id, workspace_id, name, country, iban, check_result, checked_by)
		VALUES ('payee_b488_mine', $1, 'The Owner', 'GB', 'GB82WEST12345698765432', 'exact_match', 'test')`, ws); err != nil {
		t.Fatal(err)
	}
	edited := func() economy.WalletMove {
		t.Helper()
		m := quote(ops.ID, cashOut("payee_b488_mine"), http.StatusOK)
		ramp.downNext = true // the USDC reached the ramp, which has not answered
		if code := call(http.MethodPost, base+"/"+ops.ID+"/wallet-moves/"+m.ID+"/confirm", "", nil); code != http.StatusBadGateway {
			t.Fatalf("cash-out whose ramp did not answer = %d, want 502", code)
		}
		if _, err := pool.Exec(ctx, `UPDATE money_payees SET iban = 'GB33BUKB20201555555555', details_changed_at = now() WHERE id = 'payee_b488_mine'`); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if got := confirmOnce(m); got.Status != "held" || entries(m) != 0 {
				t.Fatalf("started cash-out to a bank account edited since = %+v, %d entries; want it held, nothing posted", got, entries(m))
			}
		}
		isHeld(m)
		return m
	}
	m = edited()
	// A day on: the edit came after the quote, and its hold has passed.
	if _, err := pool.Exec(ctx, `UPDATE money_payees SET details_changed_at = now() - interval '25 hours' WHERE id = 'payee_b488_mine'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE wallet_moves SET created_at = now() - interval '26 hours' WHERE id = $1`, m.ID); err != nil {
		t.Fatal(err)
	}
	confirm(m, map[string]int64{opsUSDC: -10_000_000})
	m = edited()
	before = held(opsUSDC)
	var back economy.WalletMove
	for range 2 {
		if code := call(http.MethodPost, base+"/"+ops.ID+"/wallet-moves/"+m.ID+"/return", "", &back); code != http.StatusOK || back.Status != "returned" {
			t.Fatalf("return of a held cash-out = %d %+v; want it returned", code, back)
		}
	}
	if got := transfers.credits[m.ID+".return"]; got.Minor != 10_000_000 || got.Asset != money.USDC || entries(m) != 0 || held(opsUSDC) != before {
		t.Fatalf("returned cash-out credited %+v back, %d entries, Ops %d → %d; want 10 USDC back once, nothing posted", got, entries(m), before, held(opsUSDC))
	}
	if code := call(http.MethodPost, base+"/"+ops.ID+"/wallet-moves/"+m.ID+"/confirm", "", &back); code != http.StatusOK || back.Status != "returned" || entries(m) != 0 {
		t.Fatalf("confirm of a returned cash-out = %d %+v; want it left returned", code, back)
	}

	// (1) The owner pauses the agent after its cash-out's USDC reached the ramp: the pay-in, replayed under its key, is
	// the same payment the agent's rules allowed — not judged again — and the cash-out completes, posted once.
	if _, err := pool.Exec(ctx, `UPDATE money_payees SET details_changed_at = now() - interval '25 hours' WHERE id = 'payee_b488_exact'`); err != nil {
		t.Fatal(err)
	}
	m = quote(ops.ID, cashOut("payee_b488_exact"), http.StatusOK)
	transfers.pendNext = true
	if got := confirmOnce(m); got.Status != "quoted" || !strings.Contains(got.Detail, "pending") {
		t.Fatalf("cash-out whose pay-in answers pending = %+v", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_accounts SET paused_at = now(), paused_reason = 'b488' WHERE id = $1`, ops.ID); err != nil {
		t.Fatal(err)
	}
	confirm(m, map[string]int64{opsUSDC: -10_000_000})
	if _, err := pool.Exec(ctx, `UPDATE agent_accounts SET paused_at = NULL WHERE id = $1`, ops.ID); err != nil {
		t.Fatal(err)
	}

	// #1001 review 6: the cash-out's USDC goes to the ramp's address through B48.7's Travel Rule step.
	travel := &partners.TestTravelRuleProvider{VASPs: map[string]string{}}
	store.SetTravelRule(travel)
	m = quote(ops.ID, cashOut("payee_b488_exact"), http.StatusOK)
	var payIn string
	if err := pool.QueryRow(ctx, `SELECT pay_in_address FROM wallet_moves WHERE id = $1`, m.ID).Scan(&payIn); err != nil || payIn == "" {
		t.Fatalf("pay-in address %q %v", payIn, err)
	}
	travel.VASPs[strings.ToLower(payIn)] = "Test Ramp Exchange"
	// The identity data cannot be sent: the USDC is not paid in.
	store.SetTravelRule(b488TravelRuleDown{travel})
	var paidIn bool
	if code := call(http.MethodPost, base+"/"+ops.ID+"/wallet-moves/"+m.ID+"/confirm", "", nil); code < 500 ||
		pool.QueryRow(ctx, `SELECT paid_in_at IS NOT NULL FROM wallet_moves WHERE id = $1`, m.ID).Scan(&paidIn) != nil || paidIn {
		t.Fatalf("cash-out whose Travel Rule data could not be sent = %d, paid in %v; want a 5xx, nothing paid in", code, paidIn)
	}
	store.SetTravelRule(travel)
	confirm(m, map[string]int64{opsUSDC: -10_000_000})
	var sent int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM travel_rule_transfers WHERE payment_id = $1 AND vasp = 'Test Ramp Exchange'`, m.ID).
		Scan(&sent); err != nil || sent != 1 {
		t.Fatalf("Travel Rule records for the cash-out's pay-in = %d (%v), want 1", sent, err)
	}
	store.SetTravelRule(nil)

	// #1001 review 4: screening refuses the bank account's holder once the USDC reached the ramp: held, never a bare
	// refusal; the owner has the USDC back, nothing posted.
	m = quote(ops.ID, cashOut("payee_b488_exact"), http.StatusOK)
	ramp.downNext = true
	if code := call(http.MethodPost, base+"/"+ops.ID+"/wallet-moves/"+m.ID+"/confirm", "", nil); code != http.StatusBadGateway {
		t.Fatalf("cash-out whose ramp did not answer = %d, want 502", code)
	}
	if _, err := pool.Exec(ctx, `UPDATE money_payees SET name = 'TESTSANCTION The Owner' WHERE id = 'payee_b488_exact'`); err != nil {
		t.Fatal(err)
	}
	if got := confirmOnce(m); got.Status != "held" || entries(m) != 0 {
		t.Fatalf("started cash-out whose payee now screens as a hit = %+v, %d entries; want it held", got, entries(m))
	}
	before = held(opsUSDC)
	if code := call(http.MethodPost, base+"/"+ops.ID+"/wallet-moves/"+m.ID+"/return", "", &back); code != http.StatusOK || back.Status != "returned" ||
		transfers.credits[m.ID+".return"].Minor != 10_000_000 || entries(m) != 0 || held(opsUSDC) != before {
		t.Fatalf("return of the held cash-out = %d %+v; want 10 USDC back, nothing posted", code, back)
	}

	// #1001 review 5: a pay-in that got no answer, or answered only that it is on its way, then held, is not given back:
	// it was never seen arriving at the ramp, which is never asked to give back what it may not have. It stays held, an
	// operator's case.
	if _, err := pool.Exec(ctx, `UPDATE money_payees SET name = 'The Owner' WHERE id = 'payee_b488_exact'`); err != nil {
		t.Fatal(err)
	}
	m = quote(ops.ID, cashOut("payee_b488_exact"), http.StatusOK)
	transfers.lost = true
	if code := call(http.MethodPost, base+"/"+ops.ID+"/wallet-moves/"+m.ID+"/confirm", "", nil); code < 500 {
		t.Fatalf("cash-out whose pay-in got no answer = %d, want a 5xx", code)
	}
	transfers.pendNext = true
	if got := confirmOnce(m); got.Status != "quoted" || !strings.Contains(got.Detail, "pending") {
		t.Fatalf("cash-out whose pay-in now answers pending = %+v", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE money_payees SET name = 'TESTSANCTION The Owner' WHERE id = 'payee_b488_exact'`); err != nil {
		t.Fatal(err)
	}
	if got := confirmOnce(m); got.Status != "held" {
		t.Fatalf("started cash-out, payee now a hit = %+v; want it held", got)
	}
	if code := call(http.MethodPost, base+"/"+ops.ID+"/wallet-moves/"+m.ID+"/return", "", nil); code != http.StatusConflict ||
		transfers.credits[m.ID+".return"].Minor != 0 {
		t.Fatalf("return before the pay-in was seen arriving = %d, credited %+v; want 409, nothing back", code, transfers.credits[m.ID+".return"])
	}
}
