package economy

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B30.2 — money in currencies: accounts and a double-entry ledger in pounds, euros, dollars and USDC (0221).

// screenedStore is a money store whose money in and out through a partner is screened (B30.6) — by the Test provider's
// words alone, as these tests load no sanctions list.
func screenedStore(pool *pgxpool.Pool) *DualTokenStore {
	s := NewDualTokenStore(nil, pool, nil)
	s.SetScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	return s
}

func openMoney(t *testing.T, s *DualTokenStore, ws, currency, purpose string) MoneyAccount {
	t.Helper()
	a, err := s.OpenMoneyAccount(context.Background(), MoneyAccount{WorkspaceID: ws, Currency: currency, Purpose: purpose, Name: purpose})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func moneyBalance(t *testing.T, s *DualTokenStore, ws, account string) MoneyBalance {
	t.Helper()
	b, err := s.Balance(context.Background(), ws, account)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func moneyCount(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An entry whose postings do not sum to zero is refused when its transaction commits — not when the posting is
// written, so an entry may be built a posting at a time — and so are an entry with no postings and an entry where
// test money balances live money. PostMoney answers ErrMoneyUnbalanced, and nothing it wrote remains.
func TestMoneyLedger_AnUnbalancedEntryIsRefusedAtCommit(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := screenedStore(pool)
	const ws = "ws-b302-unbalanced"
	sandboxConfirmed(t, pool, ws) // B61.52: they move money, so they have confirmed who they are
	company, partner := openMoney(t, s, ws, CurrencyGBP, MoneyCompany), openMoney(t, s, ws, CurrencyGBP, MoneyPartner)

	refusedAtCommit := func(name string, postings ...[3]any) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `INSERT INTO money_entries (id, workspace_id, capability, kind, idempotency_key)
			VALUES ($1, $2, 'currency_accounts', 'raw', $1)`, "mle_"+name, ws); err != nil {
			t.Fatal(err)
		}
		for i, p := range postings {
			if _, err := tx.Exec(ctx, `INSERT INTO money_postings (entry_id, line, account_id, amount_minor, currency, funding)
				VALUES ($1, $2, $3, $4, 'GBP', $5)`, "mle_"+name, i+1, p[0], p[1], p[2]); err != nil {
				t.Fatalf("%s: posting %d was refused when written (%v); want it refused at commit", name, i+1, err)
			}
		}
		var pgErr *pgconn.PgError
		if err := tx.Commit(ctx); !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("%s committed with %v; want a check violation at commit", name, err)
		}
	}
	refusedAtCommit("one-sided", [3]any{company.ID, int64(1_000), "test"})
	refusedAtCommit("short", [3]any{company.ID, int64(1_000), "test"}, [3]any{partner.ID, int64(-999), "test"})
	refusedAtCommit("test-against-live", [3]any{company.ID, int64(1_000), "test"}, [3]any{partner.ID, int64(-1_000), "live"})
	refusedAtCommit("empty")

	_, err := s.PostMoney(ctx, MoneyEntry{WorkspaceID: ws, Capability: CapabilityCurrencyAccounts, Counterparty: "Acme Ltd", Kind: "payment_in",
		IdempotencyKey: "short", Funding: FundingTest, Postings: []MoneyPosting{
			{AccountID: company.ID, AmountMinor: 10_000}, {AccountID: partner.ID, AmountMinor: -9_999}}})
	if !errors.Is(err, ErrMoneyUnbalanced) {
		t.Fatalf("PostMoney of an entry 1p short = %v; want ErrMoneyUnbalanced", err)
	}
	if n := moneyCount(t, pool, `SELECT count(*) FROM money_entries`) + moneyCount(t, pool, `SELECT count(*) FROM money_postings`) +
		moneyCount(t, pool, `SELECT count(*) FROM money_account_balances`); n != 0 {
		t.Fatalf("the refused entries left %d rows of entries, postings and balances; want none", n)
	}
	if b := moneyBalance(t, s, ws, company.ID); b.AmountMinor != 0 {
		t.Fatalf("after only refused entries the company holds %+v; want 0", b)
	}
}

// A posting, and the entry it belongs to, can be neither changed nor deleted: the balance stays what was posted.
func TestMoneyLedger_APostingCannotBeUpdatedOrDeleted(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := screenedStore(pool)
	const ws = "ws-b302-append-only"
	sandboxConfirmed(t, pool, ws) // B61.52: they move money, so they have confirmed who they are
	company, partner := openMoney(t, s, ws, CurrencyEUR, MoneyCompany), openMoney(t, s, ws, CurrencyEUR, MoneyPartner)
	e, err := s.PostMoney(ctx, MoneyEntry{WorkspaceID: ws, Capability: CapabilityCurrencyAccounts, Counterparty: "Acme Ltd", Kind: "payment_in",
		IdempotencyKey: "in-1", Funding: FundingTest, Postings: []MoneyPosting{
			{AccountID: company.ID, AmountMinor: 25_00}, {AccountID: partner.ID, AmountMinor: -25_00}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`UPDATE money_postings SET amount_minor = 1_000_000 WHERE entry_id = $1 AND line = 1`,
		`UPDATE money_postings SET account_id = account_id WHERE entry_id = $1`,
		`DELETE FROM money_postings WHERE entry_id = $1`,
		`UPDATE money_entries SET memo = 'rewritten' WHERE id = $1`,
		`DELETE FROM money_entries WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, sql, e.ID); err == nil {
			t.Fatalf("%q was allowed; postings and entries are append-only", sql)
		}
	}
	if _, err := pool.Exec(ctx, `TRUNCATE money_postings`); err == nil {
		t.Fatal("TRUNCATE money_postings was allowed")
	}
	if n := moneyCount(t, pool, `SELECT count(*) FROM money_postings WHERE entry_id = $1`, e.ID); n != 2 {
		t.Fatalf("the entry has %d postings left; want its 2", n)
	}
	if b := moneyBalance(t, s, ws, company.ID); b != (MoneyBalance{AccountID: company.ID, Currency: CurrencyEUR, AmountMinor: 25_00, TestMinor: 25_00}) {
		t.Fatalf("the company holds %+v; want €25.00 of test money", b)
	}
}

// After 1,000 random entries across pounds, euros, dollars and USDC, test and live money, two to four postings in one
// or two currencies each, every stored balance row equals the sum of its postings, every account's Balance is what
// was posted to it, and every currency sums to zero across the ledger.
func TestMoneyLedger_AThousandRandomEntriesKeepEveryBalance(t *testing.T) {
	pool := supplyPool(t)
	gb := WithUseCountry(context.Background(), "GB")
	s := screenedStore(pool)
	const ws = "ws-b302-random"
	if _, err := pool.Exec(gb, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	planGatesOnPlan(t, pool, ws, "team", false) // live money needs a plan with live money, and a clearance
	liveVerified(t, pool, ws)                   // and, B30.4, a verification level
	if _, err := s.ClearCapability(gb, CapabilityCurrencyAccounts, "nicolai", ClearanceTerms{Reference: "B30.2 test",
		Licence: "EMI-900123", Partner: "Test Payments Ltd", Countries: []string{"GB"}, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	agent, err := s.CreateAgent(gb, ws, "spender", "user-b302")
	if err != nil {
		t.Fatal(err)
	}

	currencies := []string{CurrencyGBP, CurrencyEUR, CurrencyUSD, CurrencyUSDC}
	accounts := map[string][]string{}
	for _, cur := range currencies {
		for _, purpose := range []string{MoneyCompany, MoneyPartner, MoneyRevenue, MoneyPot, MoneySuspense} {
			accounts[cur] = append(accounts[cur], openMoney(t, s, ws, cur, purpose).ID)
		}
	}
	a, err := s.OpenMoneyAccount(gb, MoneyAccount{WorkspaceID: ws, AgentID: agent.ID, Currency: CurrencyGBP, Purpose: MoneyAgent})
	if err != nil {
		t.Fatal(err)
	}
	accounts[CurrencyGBP] = append(accounts[CurrencyGBP], a.ID)

	rng := rand.New(rand.NewSource(302))
	want := map[string]MoneyBalance{}
	for i := 0; i < 1_000; i++ {
		funding := []string{FundingTest, FundingLive}[rng.Intn(2)]
		var postings []MoneyPosting
		picked := rng.Perm(len(currencies))[:1+rng.Intn(2)]
		for _, c := range picked {
			cur := currencies[c]
			order := rng.Perm(len(accounts[cur]))[:2+rng.Intn(3)]
			var sum int64
			for j, k := range order {
				amount := rng.Int63n(2_000_000_000) - 1_000_000_000
				if amount == 0 {
					amount = 1
				}
				if j == len(order)-1 {
					amount = -sum // the last posting balances the currency: 0 is left out by PostMoney
				}
				sum += amount
				postings = append(postings, MoneyPosting{AccountID: accounts[cur][k], AmountMinor: amount, Currency: cur})
			}
		}
		e, err := s.PostMoney(gb, MoneyEntry{WorkspaceID: ws, Capability: CapabilityCurrencyAccounts, Counterparty: "Acme Ltd", Kind: "random",
			IdempotencyKey: fmt.Sprintf("random-%d", i), Funding: funding, Postings: postings})
		if err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
		for _, p := range e.Postings {
			b := want[p.AccountID]
			b.AccountID, b.Currency = p.AccountID, p.Currency
			b.AmountMinor += p.AmountMinor
			if p.Funding == FundingLive {
				b.LiveMinor += p.AmountMinor
			} else {
				b.TestMinor += p.AmountMinor
			}
			want[p.AccountID] = b
		}
	}

	rows, err := pool.Query(gb, `SELECT COALESCE(p.account_id, b.account_id) || ' ' || COALESCE(p.funding, b.funding)
		  FROM (SELECT account_id, funding, currency, sum(amount_minor)::bigint AS total FROM money_postings GROUP BY 1, 2, 3) p
		  FULL JOIN money_account_balances b ON b.account_id = p.account_id AND b.funding = p.funding
		 WHERE p.total IS DISTINCT FROM b.balance_minor OR p.currency IS DISTINCT FROM b.currency`)
	if err != nil {
		t.Fatal(err)
	}
	off, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(off) > 0 {
		t.Fatalf("stored balances disagree with the sum of their postings for %v", off)
	}
	if n := moneyCount(t, pool, `SELECT count(*) FROM (SELECT 1 FROM money_postings GROUP BY currency, funding HAVING sum(amount_minor) <> 0) x`); n != 0 {
		t.Fatalf("%d currencies do not sum to zero across the ledger", n)
	}
	if n := moneyCount(t, pool, `SELECT count(*) FROM money_entries`); n != 1_000 {
		t.Fatalf("%d entries; want 1000", n)
	}
	for id, w := range want {
		if got := moneyBalance(t, s, ws, id); got != w {
			t.Fatalf("Balance(%s) = %+v; want %+v", id, got, w)
		}
	}
}

// Live money asks currency_accounts first: while it is uncleared the movement is refused naming class RED and
// nothing is written; test money goes through and its postings are exactly the entry's; and once a clearance for
// the country exists, the same live movement goes through.
func TestMoneyLedger_LiveMoneyAsksTheCapability(t *testing.T) {
	pool := supplyPool(t)
	gb := WithUseCountry(context.Background(), "GB")
	s := screenedStore(pool)
	const ws = "ws-b302-live"
	if _, err := pool.Exec(gb, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	planGatesOnPlan(t, pool, ws, "team", false)
	liveVerified(t, pool, ws)
	company, partner := openMoney(t, s, ws, CurrencyUSDC, MoneyCompany), openMoney(t, s, ws, CurrencyUSDC, MoneyPartner)
	in := func(key, funding string) (MoneyEntry, error) {
		return s.PostMoney(gb, MoneyEntry{WorkspaceID: ws, Capability: CapabilityCurrencyAccounts, Counterparty: "Acme Ltd", Kind: "payment_in",
			IdempotencyKey: key, Funding: funding, Postings: []MoneyPosting{
				{AccountID: company.ID, AmountMinor: 12_500_000}, {AccountID: partner.ID, AmountMinor: -12_500_000}}})
	}

	_, err := in("live-1", FundingLive)
	if !errors.Is(err, ErrCapabilityNotCleared) || !strings.Contains(err.Error(), "class RED") {
		t.Fatalf("live USDC 12.5 without a clearance = %v; want refused naming class RED", err)
	}
	if n := moneyCount(t, pool, `SELECT count(*) FROM money_entries`) + moneyCount(t, pool, `SELECT count(*) FROM money_postings`); n != 0 {
		t.Fatalf("the refused live movement wrote %d rows; want none", n)
	}

	e, err := in("test-1", FundingTest)
	if err != nil {
		t.Fatalf("test USDC 12.5 = %v; want accepted", err)
	}
	posted := []MoneyPosting{
		{AccountID: company.ID, AmountMinor: 12_500_000, Currency: CurrencyUSDC, Funding: FundingTest},
		{AccountID: partner.ID, AmountMinor: -12_500_000, Currency: CurrencyUSDC, Funding: FundingTest},
	}
	stored, err := readMoneyEntry(gb, pool, `e.id = $1`, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored.Postings, posted) || stored.Funding != FundingTest || stored.Capability != CapabilityCurrencyAccounts {
		t.Fatalf("the ledger holds %+v; want the test entry %v", stored, posted)
	}

	if _, err := s.ClearCapability(gb, CapabilityCurrencyAccounts, "nicolai", ClearanceTerms{Reference: "B30.2 test",
		Licence: "EMI-900123", Partner: "Test Payments Ltd", Countries: []string{"GB"}, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := in("live-1", FundingLive); err != nil {
		t.Fatalf("live USDC 12.5 under a clearance for GB = %v; want accepted", err)
	}
	if b := moneyBalance(t, s, ws, company.ID); b != (MoneyBalance{AccountID: company.ID, Currency: CurrencyUSDC,
		AmountMinor: 25_000_000, TestMinor: 12_500_000, LiveMinor: 12_500_000}) {
		t.Fatalf("the company holds %+v; want 12.5 USDC of test money and 12.5 of live", b)
	}
}

// The same idempotency key again answers the entry it wrote and moves no money twice; the key used for a different
// movement is refused; a posting to a closed account, to another workspace's account, or in another currency than
// its account's is refused, and so is money moved for a capability that is not a money one.
func TestMoneyLedger_ARetryMovesMoneyOnce(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := screenedStore(pool)
	const ws = "ws-b302-retry"
	sandboxConfirmed(t, pool, ws) // B61.52: they move money, so they have confirmed who they are
	company, partner := openMoney(t, s, ws, CurrencyUSD, MoneyCompany), openMoney(t, s, ws, CurrencyUSD, MoneyPartner)
	move := func(key string, cents int64, account string) (MoneyEntry, error) {
		return s.PostMoney(ctx, MoneyEntry{WorkspaceID: ws, Capability: CapabilityCurrencyAccounts, Counterparty: "Acme Ltd", Kind: "payment_in",
			IdempotencyKey: key, Funding: FundingTest, Postings: []MoneyPosting{
				{AccountID: account, AmountMinor: cents}, {AccountID: partner.ID, AmountMinor: -cents}}})
	}
	first, err := move("in-1", 40_00, company.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := move("in-1", 40_00, company.ID)
	if err != nil || again.ID != first.ID || !reflect.DeepEqual(again.Postings, first.Postings) {
		t.Fatalf("the retry answered %+v, %v; want the first entry %+v", again, err, first)
	}
	if _, err := move("in-1", 41_00, company.ID); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("the key reused for $41.00 = %v; want ErrIdempotencyKeyReused", err)
	}
	if b := moneyBalance(t, s, ws, company.ID); b.AmountMinor != 40_00 {
		t.Fatalf("after a movement, its retry and a reused key the company holds %+v; want $40.00", b)
	}

	euros := openMoney(t, s, ws, CurrencyEUR, MoneyCompany)
	if _, err := s.PostMoney(ctx, MoneyEntry{WorkspaceID: ws, Capability: CapabilityCurrencyAccounts, Counterparty: "Acme Ltd", Kind: "payment_in",
		IdempotencyKey: "in-eur", Funding: FundingTest, Postings: []MoneyPosting{
			{AccountID: euros.ID, AmountMinor: 5_00, Currency: CurrencyUSD}, {AccountID: partner.ID, AmountMinor: -5_00}}}); err == nil {
		t.Fatal("dollars posted to a euro account were accepted")
	}
	closed := openMoney(t, s, ws, CurrencyUSD, MoneyPot)
	if _, err := pool.Exec(ctx, `UPDATE money_accounts SET status = 'closed' WHERE id = $1`, closed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := move("in-closed", 5_00, closed.ID); !errors.Is(err, ErrMoneyAccountNotOpen) {
		t.Fatalf("money into a closed account = %v; want ErrMoneyAccountNotOpen", err)
	}
	theirs := openMoney(t, s, "ws-b302-someone-else", CurrencyUSD, MoneyCompany)
	if _, err := move("in-theirs", 5_00, theirs.ID); !errors.Is(err, ErrMoneyAccountNotFound) {
		t.Fatalf("money into another workspace's account = %v; want ErrMoneyAccountNotFound", err)
	}
	if _, err := s.PostMoney(ctx, MoneyEntry{WorkspaceID: ws, Capability: CapabilityBuyListings, Counterparty: "Acme Ltd", Kind: "payment_in",
		IdempotencyKey: "in-green", Funding: FundingLive, Postings: []MoneyPosting{
			{AccountID: company.ID, AmountMinor: 5_00}, {AccountID: partner.ID, AmountMinor: -5_00}}}); err == nil {
		t.Fatal("live money moved for a capability that is not a money capability")
	}
	if n := moneyCount(t, pool, `SELECT count(*) FROM money_entries`); n != 1 {
		t.Fatalf("%d entries; want only the first", n)
	}
}
