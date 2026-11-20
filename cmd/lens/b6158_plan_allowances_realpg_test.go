package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/fees"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B61.58 — fees follow the plan: what is free each calendar month (UTC), then the fee (Nick's 15 Nov fee table). Each
// fee is read off the ledger: what the move's entry posted to Talyvor's revenue account.

// usedTheMonthsFreeConversions spends ws's free conversions this month in every currency, so a test of the margin
// itself pays it from the first penny.
func usedTheMonthsFreeConversions(t *testing.T, pool *pgxpool.Pool, ws string) {
	t.Helper()
	for _, c := range []string{"GBP", "EUR", "USD"} {
		if _, err := pool.Exec(context.Background(), `INSERT INTO plan_allowance_uses (workspace_id, allowance, ref, currency, amount, month)
			VALUES ($1, 'fx', 'test:spent:' || $2, $2, 1000000000000, date_trunc('month', now() AT TIME ZONE 'UTC')::date)`, ws, c); err != nil {
			t.Fatal(err)
		}
	}
}

// allowanceStore is a store on the Test partners, its FX margin by plan as fees' defaults set it.
func allowanceStore(pool *pgxpool.Pool) *economy.DualTokenStore {
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	store.SetFXMargin(func(ctx context.Context, q economy.FeeQuerier, workspaceID string) (int64, string, error) {
		return billing.FXMarginBPS(ctx, q, workspaceID, fees.Defaults())
	})
	return store
}

// allowanceAccount opens ws's account in currency — the agent's when agentID is set, under the company's opened first —
// holding minor of test money; it answers the account.
func allowanceAccount(t *testing.T, pool *pgxpool.Pool, store *economy.DualTokenStore, ws, agentID, currency string, minor int64) string {
	t.Helper()
	ctx := context.Background()
	a, err := store.OpenCurrencyAccount(ctx, ws, agentID, currency, "test")
	if err != nil {
		t.Fatal(err)
	}
	if minor > 0 {
		var partner string
		if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = $2`, ws, currency).Scan(&partner); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
			IdempotencyKey: "top-up:" + a.ID, Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
				{AccountID: partner, AmountMinor: -minor}, {AccountID: a.ID, AmountMinor: minor}}}); err != nil {
			t.Fatal(err)
		}
	}
	return a.ID
}

func allowanceWorkspace(t *testing.T, pool *pgxpool.Pool, ws, accountType, plan string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO workspaces (id, name, cache_prefix, account_type, plan) VALUES ($1, $1, $1, $2, $3)`,
		ws, accountType, plan); err != nil {
		t.Fatal(err)
	}
}

// revenueOf is what entryID posted to Talyvor's revenue account.
func revenueOf(t *testing.T, pool *pgxpool.Pool, entryID string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE(sum(p.amount_minor), 0)::bigint FROM money_postings p
		JOIN money_accounts a ON a.id = p.account_id WHERE p.entry_id = $1 AND a.purpose = 'revenue'`, entryID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// paymentRevenue is what the payment out under key posted to Talyvor's revenue account when it completed.
func paymentRevenue(t *testing.T, pool *pgxpool.Pool, ws, key string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE(sum(p.amount_minor), 0)::bigint FROM money_entries e
		JOIN money_postings p ON p.entry_id = e.id JOIN money_accounts a ON a.id = p.account_id
		WHERE e.workspace_id = $1 AND e.kind = 'payment_out' AND e.idempotency_key LIKE 'payment_out%:' || $2 AND a.purpose = 'revenue'`, ws, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// planAllowances is GET /v1/workspaces/{ws}/plan's allowances, by allowance and currency ("fx GBP", "local").
func planAllowances(t *testing.T, pool *pgxpool.Pool, ws string) map[string]economy.PlanAllowance {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/v1/workspaces/{wsID}/plan", newWorkspacePlanHandler(pool))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/plan", nil))
	var out struct {
		Allowances []economy.PlanAllowance `json:"allowances"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); w.Code != http.StatusOK || err != nil {
		t.Fatalf("GET plan = %d %s (%v)", w.Code, w.Body, err)
	}
	got := map[string]economy.PlanAllowance{}
	for _, a := range out.Allowances {
		k := a.Allowance
		if a.Currency != "" {
			k += " " + a.Currency
		}
		got[k] = a
	}
	return got
}

// The exchange that takes a Solo account from 990 to 1,010 pays 1% of 10 and no more; after the line it pays 1% of
// all of it; a plan change mid-month takes the new plan's line at once; a new month starts at zero; the fee rounds up.
func TestB6158_SoloConvertsItsFirst1000FreeThenPaysOnThePartAbove(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b6158-solo"
	store := allowanceStore(pool)
	confirmedAccounts(t, pool, ws)
	allowanceWorkspace(t, pool, ws, "personal", "personal_solo")
	agent, err := store.CreateAgent(ctx, ws, "Converter", "owner")
	if err != nil {
		t.Fatal(err)
	}
	allowanceAccount(t, pool, store, ws, "", "GBP", 0)
	allowanceAccount(t, pool, store, ws, "", "USD", 0)
	allowanceAccount(t, pool, store, ws, agent.ID, "GBP", 2_000_00)
	allowanceAccount(t, pool, store, ws, agent.ID, "USD", 0)
	convert := func(key string, minor int64) economy.Conversion {
		t.Helper()
		q, err := store.QuoteConversion(ctx, ws, agent.ID, economy.Conversion{SellMinor: minor, SellCurrency: "GBP", BuyCurrency: "USD"})
		if err != nil {
			t.Fatalf("quote %s: %v", key, err)
		}
		c, err := store.Convert(ctx, ws, agent.ID, q.ID, key)
		if err != nil {
			t.Fatalf("convert %s: %v", key, err)
		}
		if got := revenueOf(t, pool, c.EntryID); got != c.MarginMinor {
			t.Fatalf("%s: the entry posted %d to revenue, the conversion says a margin of %d", key, got, c.MarginMinor)
		}
		return c
	}

	if c := convert("first-990", 990_00); c.MarginMinor != 0 || c.FreeMinor != 990_00 {
		t.Fatalf("£990.00 on Solo: margin %d, free %d; want it all free", c.MarginMinor, c.FreeMinor)
	}
	if c := convert("cross-the-line", 20_00); c.FreeMinor != 10_00 || c.MarginMinor != 10 {
		t.Fatalf("£990 → £1,010 on Solo: free %d, margin %d; want £10.00 free and 1%% of the other £10.00, 10p", c.FreeMinor, c.MarginMinor)
	}
	if c := convert("above", 100_00); c.FreeMinor != 0 || c.MarginMinor != 100 {
		t.Fatalf("£100.00 above the line: free %d, margin %d; want 1%%, £1.00", c.FreeMinor, c.MarginMinor)
	}
	if a := planAllowances(t, pool, ws)["fx GBP"]; a.Free != 1_000_00 || a.Used != 1_000_00 || a.Left != 0 || a.FeeBPS == nil || *a.FeeBPS != 100 {
		t.Fatalf("Solo's GBP conversions this month = %+v; want 1,000 free, all used, none left, 1%% above", a)
	}

	// A plan change takes the new plan's line at once: Crew's 3,000 less the 1,000 used.
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET plan = 'personal_crew' WHERE id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if c := convert("on-crew", 100_00); c.FreeMinor != 100_00 || c.MarginMinor != 0 {
		t.Fatalf("£100.00 on Crew after 1,000 used: free %d, margin %d; want it free", c.FreeMinor, c.MarginMinor)
	}
	// Rounded up: with 1p left, 3p pays Crew's 0.5% of 2p, a hundredth of a penny, as a whole penny.
	if _, err := pool.Exec(ctx, `INSERT INTO plan_allowance_uses (workspace_id, allowance, ref, currency, amount, month)
		VALUES ($1, 'fx', 'test:near', 'GBP', 189999, date_trunc('month', now() AT TIME ZONE 'UTC')::date)`, ws); err != nil {
		t.Fatal(err)
	}
	if c := convert("rounds-up", 3); c.FreeMinor != 1 || c.MarginMinor != 1 {
		t.Fatalf("3p with 1p left on Crew: free %d, margin %d; want 1p free and the 0.01p fee rounded up to 1p", c.FreeMinor, c.MarginMinor)
	}

	// A new month starts at zero.
	if _, err := pool.Exec(ctx, `UPDATE plan_allowance_uses SET month = (month - interval '1 month')::date WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if a := planAllowances(t, pool, ws)["fx GBP"]; a.Used != 0 || a.Left != 3_000_00 {
		t.Fatalf("a new month's GBP conversions = %+v; want none used and Crew's 3,000 left", a)
	}
	if c := convert("new-month", 50_00); c.MarginMinor != 0 {
		t.Fatalf("£50.00 in a new month: margin %d; want it free", c.MarginMinor)
	}
}

// Two exchanges racing at the line never both go free: eight agents of one Company account, each with a quote for the
// last £10.00 of its 15,000 free, convert at once; one converts and the others are told to ask again.
func TestB6158_ConversionsRacingAtTheLineNeverBothGoFree(t *testing.T) {
	ctx := context.Background()
	// A pool of its own, so the race is at the allowance's lock and not for the pool (FOUND, B61.58; B50.54 screens
	// before the transaction, so eight at once no longer need sixteen).
	cfg := agentRoutesDB(t).Config()
	cfg.MaxConns = 24
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	const ws = "ws-b6158-race"
	store := allowanceStore(pool)
	confirmedAccounts(t, pool, ws)
	allowanceWorkspace(t, pool, ws, "business", "business_company")
	if _, err := pool.Exec(ctx, `INSERT INTO plan_allowance_uses (workspace_id, allowance, ref, currency, amount, month)
		VALUES ($1, 'fx', 'test:14990', 'GBP', 1499000, date_trunc('month', now() AT TIME ZONE 'UTC')::date)`, ws); err != nil {
		t.Fatal(err)
	}
	allowanceAccount(t, pool, store, ws, "", "GBP", 0)
	allowanceAccount(t, pool, store, ws, "", "USD", 0)
	type racer struct{ agent, quote string }
	var racers []racer
	for i := range 8 {
		agent, err := store.CreateAgent(ctx, ws, fmt.Sprintf("Racer %d", i), "owner")
		if err != nil {
			t.Fatal(err)
		}
		allowanceAccount(t, pool, store, ws, agent.ID, "GBP", 10_00)
		allowanceAccount(t, pool, store, ws, agent.ID, "USD", 0)
		q, err := store.QuoteConversion(ctx, ws, agent.ID, economy.Conversion{SellMinor: 10_00, SellCurrency: "GBP", BuyCurrency: "USD"})
		if err != nil || q.FreeMinor != 10_00 || q.MarginMinor != 0 {
			t.Fatalf("racer %d's quote = %+v, %v; want the last £10.00 free", i, q, err)
		}
		racers = append(racers, racer{agent.ID, q.ID})
	}
	var wg sync.WaitGroup
	errs := make([]error, len(racers))
	start := make(chan struct{})
	for i, r := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = store.Convert(ctx, ws, r.agent, r.quote, "race-"+r.quote)
		}()
	}
	close(start)
	wg.Wait()
	converted := 0
	for i, err := range errs {
		switch {
		case err == nil:
			converted++
		case !errors.Is(err, economy.ErrQuote):
			t.Errorf("racer %d: %v; want converted, or told to ask again", i, err)
		}
	}
	var free, margins int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0)::bigint FROM plan_allowance_uses WHERE workspace_id = $1 AND allowance = 'fx'
		AND released_at IS NULL`, ws).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(p.amount_minor), 0)::bigint FROM money_postings p JOIN money_accounts a ON a.id = p.account_id
		JOIN money_entries e ON e.id = p.entry_id WHERE e.workspace_id = $1 AND e.kind = 'fx_conversion' AND a.purpose = 'revenue'`, ws).Scan(&margins); err != nil {
		t.Fatal(err)
	}
	if converted != 1 || free != 15_000_00 || margins != 0 {
		t.Fatalf("%d of 8 converted the last £10.00 free, taking the month's free conversions to %d; want exactly one, and 15,000.00 (errors %v)", converted, free, errs)
	}
}

// A Studio account's 10th local payment goes free and its 11th pays 0.20; a failed payment gives its free one back;
// a new month starts at zero. The fee is held with the payment and posted to revenue when it completes.
func TestB6158_StudioSendsTenLocalPaymentsFreeThenPays20p(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b6158-studio"
	store := allowanceStore(pool)
	allowanceWorkspace(t, pool, ws, "business", "business_studio")
	confirmedAccounts(t, pool, ws)
	acct := allowanceAccount(t, pool, store, ws, "", "GBP", 500_00)
	payee, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
	if err != nil {
		t.Fatal(err)
	}
	pay := func(key string, minor int64) economy.OutsidePayment {
		t.Helper()
		p, err := store.PayOutside(ctx, ws, economy.OutsidePayment{PayeeID: payee.ID, AccountID: acct, Currency: "GBP", AmountMinor: minor,
			Reference: "Invoice " + key, IdempotencyKey: key})
		if err != nil {
			t.Fatalf("pay %s: %v", key, err)
		}
		return p
	}
	for i := 1; i <= 9; i++ {
		if p := pay(fmt.Sprintf("p%d", i), 1_00); p.FeeMinor != 0 || paymentRevenue(t, pool, ws, p.IdempotencyKey) != 0 {
			t.Fatalf("payment %d on Studio: fee %d; want free", i, p.FeeMinor)
		}
	}
	// Amounts ending in 13 fail at the Test partner: the money and the free payment both come back.
	if p := pay("fails", 1_13); p.Status != economy.PaymentFailed || p.FeeMinor != 0 {
		t.Fatalf("the failing payment = %s, fee %d; want failed, free", p.Status, p.FeeMinor)
	}
	if a := planAllowances(t, pool, ws)["local"]; a.Free != 10 || a.Used != 9 || a.Left != 1 || a.FeeMinor["GBP"] != 20 {
		t.Fatalf("Studio's local payments = %+v; want 9 of 10 used after the failed one gave its use back, 0.20 above", a)
	}
	if p := pay("p10", 1_00); p.FeeMinor != 0 || paymentRevenue(t, pool, ws, "p10") != 0 {
		t.Fatalf("the 10th payment: fee %d, revenue %d; want none", p.FeeMinor, paymentRevenue(t, pool, ws, "p10"))
	}
	before, err := store.Balance(ctx, ws, acct)
	if err != nil {
		t.Fatal(err)
	}
	if p := pay("p11", 1_00); p.Status != economy.PaymentCompleted || p.FeeMinor != 20 || paymentRevenue(t, pool, ws, "p11") != 20 {
		t.Fatalf("the 11th payment = %s, fee %d, revenue %d; want completed with 0.20 to revenue", p.Status, p.FeeMinor, paymentRevenue(t, pool, ws, "p11"))
	}
	if after, err := store.Balance(ctx, ws, acct); err != nil || before.TestMinor-after.TestMinor != 1_20 {
		t.Fatalf("the 11th payment took %d from the account (%v); want £1.00 and its 0.20 fee", before.TestMinor-after.TestMinor, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE plan_allowance_uses SET month = (month - interval '1 month')::date WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if p := pay("next-month", 1_00); p.FeeMinor != 0 {
		t.Fatalf("the first payment of a new month: fee %d; want free", p.FeeMinor)
	}
}

// A Company account's 5th international payment goes free and its 6th pays the fee; a quote priced free that another
// payment beat to the last free one is refused, to be asked again; Fleet pays 20% less.
func TestB6158_CompanySendsFiveInternationalPaymentsFree(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b6158-company"
	store := allowanceStore(pool)
	allowanceWorkspace(t, pool, ws, "business", "business_company")
	confirmedAccounts(t, pool, ws)
	acct := allowanceAccount(t, pool, store, ws, "", "GBP", 500_00)
	payee, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Grace Hopper", Country: "DE", IBAN: "DE89370400440532013000", BIC: "COBADEFFXXX"})
	if err != nil {
		t.Fatal(err)
	}
	quote := func() economy.InternationalQuote {
		t.Helper()
		q, err := store.QuoteInternational(ctx, ws, economy.OutsidePayment{PayeeID: payee.ID, AccountID: acct, Currency: "GBP", AmountMinor: 10_00})
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	pay := func(key string, q economy.InternationalQuote) (economy.OutsidePayment, error) {
		return store.PayOutside(ctx, ws, economy.OutsidePayment{PayeeID: payee.ID, AccountID: acct, Currency: "GBP", AmountMinor: 10_00,
			QuoteID: q.ID, Reference: "Rechnung " + key, IdempotencyKey: key})
	}
	for i := 1; i <= 4; i++ {
		q := quote()
		if p, err := pay(fmt.Sprintf("i%d", i), q); err != nil || q.FeeMinor != 0 || p.FeeMinor != 0 || paymentRevenue(t, pool, ws, p.IdempotencyKey) != 0 {
			t.Fatalf("international payment %d on Company: quote fee %d, %+v, %v; want free", i, q.FeeMinor, p, err)
		}
	}
	fifth, stale := quote(), quote()
	if p, err := pay("i5", fifth); err != nil || fifth.FeeMinor != 0 || paymentRevenue(t, pool, ws, "i5") != 0 {
		t.Fatalf("the 5th: quote fee %d, %+v, %v; want free", fifth.FeeMinor, p, err)
	}
	if _, err := pay("stale", stale); stale.FeeMinor != 0 || !errors.Is(err, economy.ErrQuote) {
		t.Fatalf("a quote priced free before the 5th took the last free one: %v; want ErrQuote, asked again", err)
	}
	sixth := quote()
	if p, err := pay("i6", sixth); err != nil || sixth.FeeMinor != 5_00 || p.FeeMinor != 5_00 || paymentRevenue(t, pool, ws, "i6") != 5_00 {
		t.Fatalf("the 6th: quote fee %d, %+v, %v, revenue %d; want £5.00 to revenue", sixth.FeeMinor, p, err, paymentRevenue(t, pool, ws, "i6"))
	}

	const fleet = "ws-b6158-fleet"
	allowanceWorkspace(t, pool, fleet, "personal", "personal_fleet")
	confirmedAccounts(t, pool, fleet)
	fleetAcct := allowanceAccount(t, pool, store, fleet, "", "EUR", 100_00)
	fleetPayee, err := store.CreateOutsidePayee(ctx, fleet, economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"})
	if err != nil {
		t.Fatal(err)
	}
	if q, err := store.QuoteInternational(ctx, fleet, economy.OutsidePayment{PayeeID: fleetPayee.ID, AccountID: fleetAcct, Currency: "EUR", AmountMinor: 10_00}); err != nil || q.FeeMinor != 4_80 {
		t.Fatalf("Fleet's international quote = %+v, %v; want €6.00 less 20%%, €4.80", q, err)
	}
}

// Local payments are counted across currencies, so payments in pounds, euros and dollars share no account lock: the
// allowance's own lock alone keeps a Studio account's last free payment to one of three sent at once, round after
// round.
func TestB6158_PaymentsRacingAtTheLineNeverBothGoFree(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b6158-payment-race"
	store := allowanceStore(pool)
	allowanceWorkspace(t, pool, ws, "business", "business_studio")
	confirmedAccounts(t, pool, ws)
	type racer struct{ account, payee, currency string }
	var racers []racer
	for _, c := range []struct {
		currency string
		payee    economy.OutsidePayee
	}{
		{"GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"}},
		{"EUR", economy.OutsidePayee{Name: "Grace Hopper", Country: "DE", IBAN: "DE89370400440532013000", BIC: "COBADEFFXXX"}},
		{"USD", economy.OutsidePayee{Name: "Katherine Johnson", Country: "US", RoutingNumber: "021000021", AccountNumber: "000123456789"}},
	} {
		acct := allowanceAccount(t, pool, store, ws, "", c.currency, 100_00)
		payee, err := store.CreateOutsidePayee(ctx, ws, c.payee)
		if err != nil {
			t.Fatal(err)
		}
		racers = append(racers, racer{acct, payee.ID, c.currency})
	}
	for round := range 6 {
		// Nine of Studio's ten used: one left.
		if _, err := pool.Exec(ctx, `DELETE FROM plan_allowance_uses WHERE workspace_id = $1`, ws); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO plan_allowance_uses (workspace_id, allowance, ref, currency, amount, month)
			SELECT $1, 'local', 'test:used:' || n, 'GBP', 1, date_trunc('month', now() AT TIME ZONE 'UTC')::date FROM generate_series(1, 9) n`, ws); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		paid := make([]economy.OutsidePayment, len(racers))
		errs := make([]error, len(racers))
		start := make(chan struct{})
		for i, r := range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				key := fmt.Sprintf("race-%d-%s", round, r.currency)
				paid[i], errs[i] = store.PayOutside(ctx, ws, economy.OutsidePayment{PayeeID: r.payee, AccountID: r.account, Currency: r.currency,
					AmountMinor: 1_00, Reference: key, IdempotencyKey: key})
			}()
		}
		close(start)
		wg.Wait()
		free := 0
		for i, p := range paid {
			switch {
			case errs[i] != nil:
				t.Fatalf("round %d, %s: %v", round, racers[i].currency, errs[i])
			case p.FeeMinor == 0:
				free++
			case p.FeeMinor != 20 || paymentRevenue(t, pool, ws, p.IdempotencyKey) != 20:
				t.Fatalf("round %d, %s: fee %d; want 0.20 to revenue", round, racers[i].currency, p.FeeMinor)
			}
		}
		if free != 1 {
			t.Fatalf("round %d: %d of 3 payments sent at once went free with one free payment left; want exactly one", round, free)
		}
	}
}
