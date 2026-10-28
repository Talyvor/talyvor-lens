package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/ap2"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/tenant"
)

// B50.1 — Lens's money paths under attack, on the migrated schema. Every test ends by proving each ledger entry still
// sums to zero per currency and funding (b501ZeroSum).

// b501ZeroSum fails t when any entry of the money ledger, or of the agents' LXC ledger, does not sum to zero.
func b501ZeroSum(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM (SELECT 1 FROM money_postings GROUP BY entry_id, currency, funding HAVING sum(amount_minor) <> 0) m) +
		(SELECT count(*) FROM (SELECT 1 FROM agent_postings GROUP BY entry_id HAVING sum(amount_ulxc) <> 0) a)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d ledger entries do not sum to zero per currency and funding", n)
	}
}

// b501Payer is newB468Payer on the sandbox money layer: an agent whose GBP account holds 500.00 of test money.
func b501Payer(t *testing.T, pool *pgxpool.Pool, ws string) b468Payer {
	t.Helper()
	return newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			return sandboxMoneyLayer(t, pool, store, reg)
		})
}

// b501Agents is a workspace with three verified agents of one owner, the first holding 100 LXC.
func b501Agents(t *testing.T, pool *pgxpool.Pool, ws string) (*economy.DualTokenStore, [3]economy.Agent) {
	t.Helper()
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetOwnerVerifier(earnverify.New(false))
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified) VALUES ($1, $1, $1, true)`, ws); err != nil {
		t.Fatal(err)
	}
	var agents [3]economy.Agent
	for i, name := range []string{"payer", "payee", "other"} {
		a, err := store.CreateAgent(ctx, ws, name, "user-owner")
		if err != nil {
			t.Fatal(err)
		}
		agents[i] = a
	}
	if _, err := store.CreditLXC(ctx, ws, 100_000_000, "stripe top-up", map[string]interface{}{"funding": economy.FundingTest}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, agents[0].ID, 100_000_000); err != nil {
		t.Fatal(err)
	}
	return store, agents
}

func b501Postings(t *testing.T, pool *pgxpool.Pool, account string) (n int, sum int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT count(*), COALESCE(sum(amount_ulxc), 0) FROM agent_postings WHERE account = $1 AND kind IN ('pay', 'transfer')`,
		account).Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	return n, sum
}

// An idempotency key sent again with a changed body is refused and moves nothing: a payment out, a ledger entry, an
// agent's payment to another agent and its transfer to another wallet.
func TestB501_AReplayedKeyWithAChangedBodyIsRefusedAndMovesNothing(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	b := b501Payer(t, pool, "ws-b501-replay")
	b.pay(t, "inv-1", 1000)
	for name, p := range map[string]economy.OutsidePayment{
		"another amount":    {AccountID: b.acct.ID, PayeeID: b.payee.ID, Currency: "GBP", AmountMinor: 9000, Reference: "Invoice inv-1", IdempotencyKey: "inv-1"},
		"another reference": {AccountID: b.acct.ID, PayeeID: b.payee.ID, Currency: "GBP", AmountMinor: 1000, Reference: "Invoice inv-2", IdempotencyKey: "inv-1"},
	} {
		if _, err := b.store.PayOutside(ctx, b.acct.WorkspaceID, p); !errors.Is(err, economy.ErrIdempotencyKeyReused) {
			t.Errorf("a payment out under a used key with %s: %v, want ErrIdempotencyKeyReused", name, err)
		}
	}
	b.balance(t, 49000)
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, b.acct.WorkspaceID).Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: b.acct.WorkspaceID, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -90000}, {AccountID: b.acct.ID, AmountMinor: 90000}}}); !errors.Is(err, economy.ErrIdempotencyKeyReused) {
		t.Errorf("a ledger entry under a used key for more money: %v, want ErrIdempotencyKeyReused", err)
	}
	b.balance(t, 49000)

	store, ag := b501Agents(t, pool, "ws-b501-replay-agents")
	keyed := economy.WithMoveKey(ctx, "pay-1")
	first, err := store.PayAgent(keyed, "ws-b501-replay-agents", ag[0].ID, ag[1].ID, 5_000_000, "design")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := store.PayAgent(keyed, "ws-b501-replay-agents", ag[0].ID, ag[1].ID, 5_000_000, "design"); err != nil || again.EntryID != first.EntryID {
		t.Fatalf("the same payment again = %+v, %v; want the first one's entry %s", again, err, first.EntryID)
	}
	for name, try := range map[string]func() error{
		"another amount": func() error {
			_, err := store.PayAgent(keyed, "ws-b501-replay-agents", ag[0].ID, ag[1].ID, 9_000_000, "design")
			return err
		},
		"another payee": func() error {
			_, err := store.PayAgent(keyed, "ws-b501-replay-agents", ag[0].ID, ag[2].ID, 5_000_000, "design")
			return err
		},
		"another memo": func() error {
			_, err := store.PayAgent(keyed, "ws-b501-replay-agents", ag[0].ID, ag[1].ID, 5_000_000, "lunch")
			return err
		},
	} {
		if err := try(); !errors.Is(err, economy.ErrIdempotencyKeyReused) {
			t.Errorf("an agent payment under a used key with %s: %v, want ErrIdempotencyKeyReused", name, err)
		}
	}
	keyed = economy.WithMoveKey(ctx, "send-1")
	sent, err := store.SendCredits(keyed, "ws-b501-replay-agents", ag[0].ID, ag[2].ID, 2_000_000, "fee")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := store.SendCredits(keyed, "ws-b501-replay-agents", ag[0].ID, ag[2].ID, 2_000_000, "fee"); err != nil || again.ID != sent.ID || again.EntryID != sent.EntryID {
		t.Fatalf("the same transfer again = %+v, %v; want the first one %s", again, err, sent.ID)
	}
	if _, err := store.SendCredits(keyed, "ws-b501-replay-agents", ag[0].ID, ag[1].ID, 2_000_000, "fee"); !errors.Is(err, economy.ErrIdempotencyKeyReused) {
		t.Errorf("a transfer under a used key to another wallet: %v, want ErrIdempotencyKeyReused", err)
	}
	if n, sum := b501Postings(t, pool, "agent:"+ag[0].ID); n != 2 || sum != -7_000_000 {
		t.Errorf("the payer's payment and transfer postings = %d summing %d, want 2 summing -7 LXC", n, sum)
	}
	b501ZeroSum(t, pool)
}

// The same agent-to-agent payment, and the same transfer, sent 20 times at once under one Idempotency-Key, through the
// routes, move money once. (Payments out, cards, payouts, standing orders, purchases and earnings:
// TestB501_TwentyIdenticalRequestsAtOnceMoveMoneyOnce.)
func TestB501_TwentyAgentToAgentPaymentsAtOnceMoveMoneyOnce(t *testing.T) {
	pool := agentRoutesDB(t)
	const ws = "ws-b501-twenty"
	store, ag := b501Agents(t, pool, ws)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountAgentTransferRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-owner", Scopes: []string{auth.ScopeKeys}}
	for _, c := range []struct{ path, body, payee string }{
		{"/v1/workspaces/" + ws + "/agents/" + ag[0].ID + "/pay", fmt.Sprintf(`{"to_agent_id":%q,"amount_ulxc":1000000}`, ag[1].ID), ag[1].ID},
		{"/v1/workspaces/" + ws + "/agents/" + ag[0].ID + "/send", fmt.Sprintf(`{"to":%q,"amount_ulxc":1000000}`, ag[2].ID), ag[2].ID},
	} {
		var wg sync.WaitGroup
		start := make(chan struct{})
		codes := make(chan int, 20)
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
				req.Header.Set("Idempotency-Key", "twenty-"+c.payee)
				req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				codes <- w.Code
			}()
		}
		close(start)
		wg.Wait()
		close(codes)
		for code := range codes {
			if code != http.StatusOK {
				t.Errorf("POST %s = %d, want every retry answered 200", c.path, code)
			}
		}
		if n, sum := b501Postings(t, pool, "agent:"+c.payee); n != 1 || sum != 1_000_000 {
			t.Errorf("%s: the payee's postings = %d summing %d, want one of 1 LXC", c.path, n, sum)
		}
	}
	b501ZeroSum(t, pool)
}

// Zero, negative, overflowing, not-a-number and wrong-currency amounts are refused wherever money enters, and post
// nothing.
func TestB501_BadAmountsAreRefusedWhereverMoneyEnters(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	b := b501Payer(t, pool, "ws-b501-amounts")
	ws := b.acct.WorkspaceID
	var company, partner string
	if err := pool.QueryRow(ctx, `SELECT c.partner_account_ref, p.id FROM money_accounts c JOIN money_accounts p ON p.workspace_id = c.workspace_id
		AND p.purpose = 'partner' WHERE c.workspace_id = $1 AND c.purpose = 'company'`, ws).Scan(&company, &partner); err != nil {
		t.Fatal(err)
	}
	var entries int
	count := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_entries WHERE workspace_id = $1`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	entries = count()
	for i, c := range []struct {
		amount   int64
		currency string
	}{{0, "GBP"}, {-1, "GBP"}, {math.MinInt64, "GBP"}, {math.MaxInt64, "GBP"}, {100, "EUR"}, {100, "XXX"}} {
		key := fmt.Sprintf("bad-%d", i)
		if _, err := b.store.PayOutside(ctx, ws, economy.OutsidePayment{AccountID: b.acct.ID, PayeeID: b.payee.ID, Currency: c.currency,
			AmountMinor: c.amount, Reference: "x", IdempotencyKey: key}); err == nil {
			t.Errorf("a payment out of %d %s was made", c.amount, c.currency)
		}
		if _, err := b.store.ReceivePayment(ctx, economy.InboundPayment{PartnerAccountRef: company, PartnerRef: "in-" + key, Payer: "x",
			AmountMinor: c.amount, Currency: c.currency}); err == nil && c.amount != math.MaxInt64 {
			t.Errorf("a payment in of %d %s was posted", c.amount, c.currency)
		}
		if _, err := b.store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
			IdempotencyKey: "entry-" + key, Funding: economy.FundingTest, Postings: []economy.MoneyPosting{
				{AccountID: partner, AmountMinor: -c.amount, Currency: c.currency}, {AccountID: b.acct.ID, AmountMinor: c.amount, Currency: c.currency}}}); err == nil &&
			c.amount > 0 && c.currency != "GBP" {
			t.Errorf("a ledger entry of %d %s into a GBP account was posted", c.amount, c.currency)
		}
	}
	// MaxInt64 GBP into the company account is one legal entry; the rest posted nothing.
	if got := count(); got > entries+1 {
		t.Errorf("bad amounts posted %d entries", got-entries)
	}
	// The webhook's own check: a credit of nothing, or of less than nothing, is incomplete.
	layer := sandboxMoneyLayer(t, pool, b.store, partners.NewRegistry(nil))
	r := chi.NewRouter()
	r.Post("/v1/money/webhooks/{partner}", layer.ServeWebhook)
	for _, amount := range []string{"0", "-4200", "1.5", "9223372036854775808"} {
		body := []byte(fmt.Sprintf(`{"id":"evt_%s","to":%q,"amount":{"amount_minor":%s,"asset":"GBP"},"partner_ref":"ref_%s"}`, amount, company, amount, amount))
		req := httptest.NewRequest(http.MethodPost, "/v1/money/webhooks/sandbox", bytes.NewReader(body))
		req.Header.Set(money.SandboxSignatureHeader, money.SignSandbox("whsec_b468", body, time.Now()))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == http.StatusOK {
			t.Errorf("a webhook credit of %s was taken", amount)
		}
	}

	store, ag := b501Agents(t, pool, "ws-b501-amounts-agents")
	for _, amount := range []int64{0, -1, math.MinInt64} {
		if _, err := store.PayAgent(ctx, "ws-b501-amounts-agents", ag[0].ID, ag[1].ID, amount, ""); err == nil {
			t.Errorf("an agent payment of %d was made", amount)
		}
		if _, err := store.SendCredits(ctx, "ws-b501-amounts-agents", ag[0].ID, ag[1].ID, amount, ""); err == nil {
			t.Errorf("an agent transfer of %d was made", amount)
		}
	}
	if _, err := store.PayAgent(ctx, "ws-b501-amounts-agents", ag[0].ID, ag[1].ID, math.MaxInt64, ""); !errors.Is(err, economy.ErrAgentFunds) {
		t.Errorf("an agent payment of MaxInt64: %v, want ErrAgentFunds", err)
	}
	for _, usd := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := economy.NewMarketplaceStore(nil, pool).ExecuteTrade(ctx, "lst_x", "buyer", usd); err == nil || !strings.Contains(err.Error(), "positive") {
			t.Errorf("a marketplace trade of %v USD: %v, want refused as not positive", usd, err)
		}
	}
	b.balance(t, 50000)
	if n, _ := b501Postings(t, pool, "agent:"+ag[1].ID); n != 0 {
		t.Errorf("bad agent amounts posted %d postings", n)
	}
	b501ZeroSum(t, pool)
}

// An entry that does not sum to zero in each currency is refused whole, however it is unbalanced.
func TestB501_EveryLedgerEntrySumsToZeroPerCurrencyAndFunding(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	b := b501Payer(t, pool, "ws-b501-zero")
	ws := b.acct.WorkspaceID
	eur, err := b.store.OpenCurrencyAccount(ctx, ws, "", "EUR", "test")
	if err != nil {
		t.Fatal(err)
	}
	var partner string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = 'GBP'`, ws).Scan(&partner); err != nil {
		t.Fatal(err)
	}
	for name, postings := range map[string][]economy.MoneyPosting{
		"one side short":           {{AccountID: partner, AmountMinor: -100}, {AccountID: b.acct.ID, AmountMinor: 99}},
		"one leg":                  {{AccountID: b.acct.ID, AmountMinor: 100}},
		"GBP out, EUR in":          {{AccountID: partner, AmountMinor: -100, Currency: "GBP"}, {AccountID: eur.ID, AmountMinor: 100, Currency: "EUR"}},
		"balanced but overflowing": {{AccountID: partner, AmountMinor: math.MinInt64}, {AccountID: b.acct.ID, AmountMinor: math.MaxInt64}, {AccountID: b.acct.ID, AmountMinor: 1}},
	} {
		if _, err := b.store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
			IdempotencyKey: "zero-" + name, Funding: economy.FundingTest, Postings: postings}); err == nil {
			t.Errorf("an entry with %s was posted", name)
		}
	}
	b.balance(t, 50000)
	b501ZeroSum(t, pool)
}

// An agent's approval lets through only the request it was given for, once: not another amount, payee or memo, and
// not twice. A payment request's signed statement is refused once expired, for another request, or altered.
func TestB501_AnApprovalCoversOnlyItsOwnRequestOnceAndExpires(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b501-approval"
	store, ag := b501Agents(t, pool, ws)
	if _, err := store.SetAgentRules(ctx, ws, ag[0].ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PayAgent(ctx, ws, ag[0].ID, ag[1].ID, 5_000_000, "invoice 7"); !errors.Is(err, economy.ErrApprovalRequired) {
		t.Fatalf("a payment above the approval amount: %v, want ErrApprovalRequired", err)
	}
	var approval string
	if err := pool.QueryRow(ctx, `SELECT id FROM agent_approvals WHERE agent_id = $1 AND status = 'pending'`, ag[0].ID).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DecideAgentApproval(ctx, ws, approval, true); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]struct {
		to     string
		amount int64
		memo   string
	}{"another amount": {ag[1].ID, 5_000_001, "invoice 7"}, "another payee": {ag[2].ID, 5_000_000, "invoice 7"}, "another request": {ag[1].ID, 5_000_000, "invoice 8"}} {
		if _, err := store.PayAgent(ctx, ws, ag[0].ID, p.to, p.amount, p.memo); !errors.Is(err, economy.ErrApprovalRequired) {
			t.Errorf("%s under the approval: %v, want ErrApprovalRequired", name, err)
		}
	}
	if _, err := store.PayAgent(ctx, ws, ag[0].ID, ag[1].ID, 5_000_000, "invoice 7"); err != nil {
		t.Fatalf("the approved payment: %v", err)
	}
	if _, err := store.PayAgent(ctx, ws, ag[0].ID, ag[1].ID, 5_000_000, "invoice 7"); !errors.Is(err, economy.ErrApprovalRequired) {
		t.Errorf("the approved payment a second time: %v, want ErrApprovalRequired", err)
	}
	if n, sum := b501Postings(t, pool, "agent:"+ag[1].ID); n != 1 || sum != 5_000_000 {
		t.Errorf("the payee's postings = %d summing %d, want the one approved payment", n, sum)
	}

	b := b501Payer(t, pool, "ws-b501-approval-request")
	prepare := func(key string, minor int64) (economy.PaymentRequest, economy.ApprovalStatement, string) {
		t.Helper()
		r, _, err := b.store.PreparePaymentRequest(ctx, b.acct.WorkspaceID, "ada", key, economy.PaymentRequestIn{Client: "chat",
			Items: []economy.PaymentRequestItemIn{{PayeeID: b.payee.ID, Currency: "GBP", AmountMinor: minor, Reference: "Invoice " + key}}})
		if err != nil {
			t.Fatal(err)
		}
		statement, _, _, err := b.store.PrepareRequestApproval(ctx, b.acct.WorkspaceID, r.ID, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		var st economy.ApprovalStatement
		if err := json.Unmarshal([]byte(statement), &st); err != nil {
			t.Fatal(err)
		}
		return r, st, statement
	}
	r1, st, _ := prepare("req-1", 2000)
	r2, _, _ := prepare("req-2", 3000)
	expired := st
	expired.ExpiresAt = time.Now().Add(-time.Second).UTC().Truncate(time.Second)
	altered := st
	altered.Items = append([]economy.ApprovalStatementItem(nil), st.Items...)
	altered.Items[0].AmountMinor = 200000
	for name, c := range map[string]struct {
		id string
		st economy.ApprovalStatement
	}{"expired": {r1.ID, expired}, "another request's": {r2.ID, st}, "altered": {r1.ID, altered}} {
		body, _ := json.Marshal(c.st)
		// Unsigned: each is refused before its signature is read, so no passkey is needed to prove it.
		if _, err := b.store.ApprovePaymentRequest(ctx, b.acct.WorkspaceID, c.id, "ada", string(body), nil); !errors.Is(err, economy.ErrPaymentRequest) {
			t.Errorf("an %s statement: %v, want ErrPaymentRequest", name, err)
		}
	}
	b.balance(t, 50000)
	b501ZeroSum(t, pool)
}

// An agent cannot approve its own request, and its own key cannot raise its limits, boost them or decide its approvals.
func TestB501_AnAgentCannotApproveItselfOrRaiseItsOwnLimits(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b501-self"
	store, ag := b501Agents(t, pool, ws)
	store.SetMandateSigner(ap2.NewSigner([]byte("b50.1 test seed, thirty-two by.."), "https://lens.test"))
	if _, err := store.SetAgentRules(ctx, ws, ag[0].ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PayAgent(ctx, ws, ag[0].ID, ag[1].ID, 5_000_000, "self"); !errors.Is(err, economy.ErrApprovalRequired) {
		t.Fatalf("a payment above the approval amount: %v", err)
	}
	var approval string
	if err := pool.QueryRow(ctx, `SELECT id FROM agent_approvals WHERE agent_id = $1 AND status = 'pending'`, ag[0].ID).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApproveAsAgent(ctx, ws, ag[0].ID, approval); !errors.Is(err, economy.ErrNotApprover) {
		t.Errorf("the agent approving its own request: %v, want ErrNotApprover", err)
	}
	if err := store.AttachAgentKey(ctx, ws, ag[0].ID, "key-b501-self"); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	own := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-b501-self", Scopes: []string{auth.ScopeProxy}}
	base := "/v1/workspaces/" + ws + "/agents/"
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPut, base + ag[0].ID + "/rules", `{"approval_above_ulxc":0,"max_per_request_ulxc":999000000}`},
		{http.MethodPost, base + ag[0].ID + "/rules/rollback", `{"version":1}`},
		{http.MethodPost, base + ag[0].ID + "/rules/boosts", `{"rule":"max_per_request_ulxc","value":999000000,"until":"2099-01-01T00:00:00Z"}`},
		{http.MethodPost, base + "approvals/" + approval + "/approve", `{}`},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), own))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("the agent's own key: %s %s = %d, want 403", c.method, c.path, w.Code)
		}
	}
	rules, err := store.GetAgentRules(ctx, ws, ag[0].ID)
	if err != nil || rules.ApprovalAboveULXC != 1_000_000 || rules.MaxPerRequestULXC != 0 {
		t.Errorf("the agent's rules after its own attempts = %+v, %v; want them as the owner set them", rules, err)
	}
	if n, _ := b501Postings(t, pool, "agent:"+ag[1].ID); n != 0 {
		t.Errorf("the self-approved payment posted %d postings", n)
	}
	b501ZeroSum(t, pool)
}

// A mandate allows a payment only inside every one of its terms: another payee, currency, client, agent, or an amount
// over a limit, outside its dates or once revoked is blocked.
func TestB501_AMandateNeverCoversAPaymentOutsideItsTerms(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	terms := economy.PermissionMandateTerms{Version: economy.PermissionMandateVersion, ID: "mdt_b501", WorkspaceID: "ws-b501", AgentID: "agt_a",
		Clients: []string{"chat"}, Scope: economy.MandateScope{Payees: []string{"pay_ada"}, Currencies: []string{"GBP"}},
		Limits:    economy.MandateLimits{PerPayment: map[string]int64{"GBP": 5000}, PerDay: map[string]int64{"GBP": 8000}, PerMonth: map[string]int64{"GBP": 20000}},
		ValidFrom: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}
	in := economy.PreparedPayment{Client: "chat", AgentID: "agt_a", PayeeID: "pay_ada", Currency: "GBP", AmountMinor: 4000}
	if d := economy.EvaluatePermissionMandate(terms, nil, in, now); d.Decision != economy.MandateAllow {
		t.Fatalf("the payment inside its terms = %+v, want allowed", d)
	}
	revoked := now.Add(-time.Minute)
	for name, c := range map[string]struct {
		p       func(*economy.PreparedPayment)
		at      time.Time
		revoked *time.Time
	}{
		"another payee":     {p: func(p *economy.PreparedPayment) { p.PayeeID = "pay_mallory" }},
		"another currency":  {p: func(p *economy.PreparedPayment) { p.Currency = "EUR" }},
		"another client":    {p: func(p *economy.PreparedPayment) { p.Client = "mcp:other" }},
		"another agent":     {p: func(p *economy.PreparedPayment) { p.AgentID = "agt_b" }},
		"over a payment":    {p: func(p *economy.PreparedPayment) { p.AmountMinor = 5001 }},
		"over the day":      {p: func(p *economy.PreparedPayment) { p.SpentTodayMinor = 4001 }},
		"over the month":    {p: func(p *economy.PreparedPayment) { p.SpentMonthMinor = 16001 }},
		"nothing":           {p: func(p *economy.PreparedPayment) { p.AmountMinor = 0 }},
		"less than nothing": {p: func(p *economy.PreparedPayment) { p.AmountMinor = -4000 }},
		"before its start":  {at: now.Add(-2 * time.Hour)},
		"after its expiry":  {at: now.Add(time.Hour)},
		"once revoked":      {revoked: &revoked},
	} {
		p, at := in, now
		if c.p != nil {
			c.p(&p)
		}
		if !c.at.IsZero() {
			at = c.at
		}
		if d := economy.EvaluatePermissionMandate(terms, c.revoked, p, at); d.Decision == economy.MandateAllow {
			t.Errorf("a payment with %s = %+v, want it not allowed", name, d)
		}
	}
}

// A held payment's pass through the money layer's gate covers only its own payee, account, amount, currency, agent and
// workspace; anything else under its key is judged by the agent's rules, and a payout is never let through by it.
func TestB501_AGatePassCoversOnlyItsOwnPayeeAccountAndAmount(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	b := b501Payer(t, pool, "ws-b501-gate")
	ws := b.acct.WorkspaceID
	p := b.pay(t, "held", 2000)
	if _, err := pool.Exec(ctx, `UPDATE money_payments SET status = 'pending' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	var from string
	if err := pool.QueryRow(ctx, `SELECT c.partner_account_ref FROM money_accounts a JOIN money_accounts c ON c.id = COALESCE(a.parent_account_id, a.id)
		WHERE a.id = $1`, b.acct.ID).Scan(&from); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.SetAgentRules(ctx, ws, p.AgentID, economy.AgentRules{MaxPerRequestULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	own := money.Payee{Name: b.payee.Name, SortCode: b.payee.SortCode, AccountNumber: b.payee.AccountNumber}
	op := func(f func(*money.Op)) money.Op {
		o := money.Op{Route: money.Route{Workspace: ws, Agent: p.AgentID, Country: "GB", Capability: economy.CapabilityPaymentsOut},
			Partner: money.SandboxName, Capability: money.CapTransfers, Method: "outbound", Key: p.ID, Ref: from,
			Amount: money.Amount{Minor: 2000, Asset: "GBP"}, Payee: own.Name, To: own}
		if f != nil {
			f(&o)
		}
		return o
	}
	limits := b.store.MoneyLimits()
	if err := limits.Allow(ctx, op(nil)); err != nil {
		t.Fatalf("the held payment itself: %v", err)
	}
	other, err := b.store.CreateAgent(ctx, ws, "Other", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.SetAgentRules(ctx, ws, other.ID, economy.AgentRules{MaxPerRequestULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]money.Op{
		"a larger amount":         op(func(o *money.Op) { o.Amount.Minor = 200000 }),
		"another currency":        op(func(o *money.Op) { o.Amount.Asset = "EUR" }),
		"another payee's account": op(func(o *money.Op) { o.To.AccountNumber = "11223344" }),
		"another paying account":  op(func(o *money.Op) { o.Ref = from + "-other" }),
		"another agent":           op(func(o *money.Op) { o.Route.Agent = other.ID }),
		"a payout":                op(func(o *money.Op) { o.Capability, o.Method = money.CapPayouts, "pay" }),
	} {
		if err := limits.Allow(ctx, o); err == nil {
			t.Errorf("%s under the held payment's key went through the gate", name)
		}
	}
	if err := limits.Allow(ctx, op(func(o *money.Op) { o.Route.Workspace = "ws-b501-gate-other" })); err == nil {
		t.Errorf("another workspace under the held payment's key went through the gate")
	}
	b501ZeroSum(t, pool)
}

// A credit webhook with a bad or missing signature, a replayed id, or naming an account held at another partner posts
// nothing; the partner's own signed credit posts once.
func TestB501_ABadWebhookPostsNothing(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	b := b501Payer(t, pool, "ws-b501-webhook")
	ws := b.acct.WorkspaceID
	var company string
	if err := pool.QueryRow(ctx, `SELECT partner_account_ref FROM money_accounts WHERE workspace_id = $1 AND purpose = 'company'`, ws).Scan(&company); err != nil {
		t.Fatal(err)
	}
	ledger := moneyLedger{b.store}
	credit := money.Event{ID: "evt_other", To: company, Amount: money.Amount{Minor: 4200, Asset: "GBP"}, PartnerRef: "swan_in_1", Payer: "Mallory"}
	for _, partner := range []string{"swan", "circle", "coinbase"} {
		if _, err := ledger.PostCredit(ctx, partner, credit); err == nil {
			t.Errorf("a %s webhook credited an account at the sandbox's test partner", partner)
		}
	}
	// An agent's USDC wallet, as if opened at Circle: the sandbox's webhook cannot pay into it.
	holder, err := b.store.CreateAgent(ctx, ws, "Holder", "owner")
	if err != nil {
		t.Fatal(err)
	}
	var wallet string
	if err := pool.QueryRow(ctx, `SELECT partner_account_ref FROM money_accounts WHERE agent_id = $1 AND currency = 'USDC' AND purpose = 'agent'`,
		holder.ID).Scan(&wallet); err != nil {
		t.Fatalf("the agent's USDC wallet: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE money_accounts SET name = 'circle' WHERE partner_account_ref = $1 AND purpose = 'partner'`, wallet); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.PostCredit(ctx, money.SandboxName, money.Event{ID: "evt_wallet", To: wallet,
		Amount: money.Amount{Minor: 4200, Asset: "USDC"}, PartnerRef: "sandbox_in_1", Payer: "Acme"}); err == nil {
		t.Errorf("a sandbox webhook credited a wallet held at Circle")
	}
	b.balance(t, 50000)

	layer := sandboxMoneyLayer(t, pool, b.store, partners.NewRegistry(nil))
	r := chi.NewRouter()
	r.Post("/v1/money/webhooks/{partner}", layer.ServeWebhook)
	body := []byte(fmt.Sprintf(`{"id":"evt_b501","to":%q,"amount":{"amount_minor":4200,"asset":"GBP"},"partner_ref":"in_b501","payer":"Acme"}`, company))
	post := func(sig string, set bool) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/money/webhooks/sandbox", bytes.NewReader(body))
		if set {
			req.Header.Set(money.SandboxSignatureHeader, sig)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	for name, c := range map[string]struct {
		sig string
		set bool
	}{
		"no signature":       {"", false},
		"an empty one":       {"", true},
		"another secret's":   {money.SignSandbox("whsec_wrong", body, time.Now()), true},
		"an hour-old one":    {money.SignSandbox("whsec_b468", body, time.Now().Add(-time.Hour)), true},
		"another body's one": {money.SignSandbox("whsec_b468", []byte(`{}`), time.Now()), true},
	} {
		if code := post(c.sig, c.set); code != http.StatusUnauthorized && code != http.StatusBadRequest {
			t.Errorf("a credit with %s = %d, want refused", name, code)
		}
	}
	if code := post(money.SignSandbox("whsec_b468", body, time.Now()), true); code != http.StatusOK {
		t.Fatalf("the signed credit = %d", code)
	}
	if code := post(money.SignSandbox("whsec_b468", body, time.Now()), true); code != http.StatusConflict {
		t.Errorf("the same id again = %d, want 409", code)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_entries WHERE workspace_id = $1 AND kind = 'payment_in'`, ws).Scan(&n); err != nil || n != 1 {
		t.Errorf("payments in posted = %d, %v; want the one signed credit", n, err)
	}
	b501ZeroSum(t, pool)
}

// A refund gives back at most what went out, once: a transfer refunded twice pays back once, and a payment out returned
// by the partner twice, or for more than it was, releases only what it took.
func TestB501_ARefundNeverPaysBackMoreThanWentOut(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b501-refund"
	store, ag := b501Agents(t, pool, ws)
	sent, err := store.SendCredits(ctx, ws, ag[0].ID, ag[2].ID, 3_000_000, "deposit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RefundTransfer(ctx, ws, sent.ID); err != nil {
		t.Fatalf("the refund: %v", err)
	}
	if _, err := store.RefundTransfer(ctx, ws, sent.ID); !errors.Is(err, economy.ErrAlreadyRefunded) {
		t.Errorf("a second refund: %v, want ErrAlreadyRefunded", err)
	}
	if n, sum := b501Postings(t, pool, "agent:"+ag[0].ID); sum != 0 || n != 2 {
		t.Errorf("the sender's postings after a refund = %d summing %d, want the transfer and its one refund", n, sum)
	}

	b := b501Payer(t, pool, "ws-b501-refund-out")
	p := b.pay(t, "returned", 2000)
	ledger := moneyLedger{b.store}
	for i, minor := range []int64{2000, 999999} {
		e := money.Event{ID: fmt.Sprintf("evt_ret_%d", i), Kind: money.EventTransfer, Status: money.StatusReturned, PartnerRef: p.PartnerRef,
			Amount: money.Amount{Minor: minor, Asset: "GBP"}}
		_, _, _ = ledger.SettleTransfer(ctx, money.SandboxName, e)
	}
	b.balance(t, 50000)
	b501ZeroSum(t, pool)
}
