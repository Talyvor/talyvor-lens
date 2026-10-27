package market

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/opsusage"
	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/workspace"
)

// everyoneVerified verifies every agent's owner, so the wall is the only thing that can refuse.
type everyoneVerified struct{}

func (everyoneVerified) MayEarn(context.Context, pgx.Tx, string) (bool, error) { return true, nil }

// wallWorld is the migrated schema with real and test (synthetic) workspaces and a funded agent in each.
type wallWorld struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	bank   *economy.DualTokenStore
	agents map[string]economy.Agent
}

const wallLXC = int64(1_000_000)

func newWallWorld(t *testing.T, real, test []string) *wallWorld {
	pool := migratedDB(t)
	w := &wallWorld{t: t, ctx: context.Background(), pool: pool, bank: economy.NewDualTokenStore(nil, pool, nil), agents: map[string]economy.Agent{}}
	w.bank.SetOwnerVerifier(everyoneVerified{})
	for _, ws := range append(append([]string{}, real...), test...) {
		synthetic := false
		for _, x := range test {
			synthetic = synthetic || x == ws
		}
		if _, err := pool.Exec(w.ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, $2, true)`,
			ws, synthetic); err != nil {
			t.Fatal(err)
		}
		a, err := w.bank.CreateAgent(w.ctx, ws, ws+" agent", "owner-"+ws)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.bank.CreditLXC(w.ctx, ws, 100*wallLXC, "stripe top-up", map[string]interface{}{"funding": economy.FundingTest}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.bank.FundAgent(w.ctx, ws, a.ID, 100*wallLXC); err != nil {
			t.Fatal(err)
		}
		w.agents[ws] = a
	}
	return w
}

// rows counts every money row there is, table by table.
func (w *wallWorld) rows() map[string]int {
	w.t.Helper()
	n := map[string]int{}
	for _, tbl := range []string{"lxc_ledger", "agent_postings", "agent_transfers", "agent_money_requests", "agent_escrows", "agent_loans",
		"agent_payment_schedules", "market_uses", "pool_royalty_mints", "lens_token_ledger"} {
		var c int
		if err := w.pool.QueryRow(w.ctx, `SELECT count(*) FROM `+tbl).Scan(&c); err != nil {
			w.t.Fatal(err)
		}
		n[tbl] = c
	}
	return n
}

func mintHit(requester, contributor, answer string) poolroyalty.ServedHit {
	return poolroyalty.ServedHit{RequestID: "req-" + answer, RequesterWorkspace: requester, ContributorWorkspace: contributor, Layer: "exact",
		EntryID: "e-" + answer, Provider: "openai", Model: "gpt-4o", AvoidedCOGSUSD: 2,
		AnswerSHA256: poolroyalty.SHA256Hex([]byte(answer)), PromptSHA256: poolroyalty.SHA256Hex([]byte("p-" + answer))}
}

// B25.1 — a test workspace's money never reaches a real one, nor a real one's a test one: a transfer, a money request,
// a loan, an escrow, a recurring transfer, a marketplace purchase and a royalty across the wall are each refused with the
// rule's own words, and not one row is written.
func TestMoneyWall_TestToRealIsRefusedAndWritesNoRow(t *testing.T) {
	const real, test = "ws-real", "ws-test"
	w := newWallWorld(t, []string{real}, []string{test})
	ctx := w.ctx
	s := NewStore(w.pool)
	listing, err := s.Publish(ctx, real, Draft{Kind: "prompt", Title: "Summarise", PricePerUseULXC: wallLXC,
		Artifact: json.RawMessage(`{"template":"Summarise this.","model":"gpt-4o"}`)})
	if err != nil {
		t.Fatal(err)
	}
	minter := poolroyalty.NewMinter(w.pool, mining.NewLedgerStore(w.pool), 0.5, func() bool { return true })
	minter.SetMoneyWall(workspace.CheckMoneyWall)
	before := w.rows()

	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, workspace.ErrMoneyWall) || !strings.Contains(err.Error(), "a test workspace and a real one never pay each other") {
			t.Errorf("%s across the wall = %v, want refused with the rule's words", what, err)
		}
	}
	_, err = w.bank.SendCredits(ctx, test, w.agents[test].ID, w.agents[real].ID, wallLXC, "test to real")
	refused("a transfer from test to real", err)
	_, err = w.bank.SendCredits(ctx, real, w.agents[real].ID, w.agents[test].ID, wallLXC, "real to test")
	refused("a transfer from real to test", err)
	_, err = w.bank.RequestCredits(ctx, test, w.agents[test].ID, w.agents[real].ID, wallLXC, "pay me")
	refused("a request from test to real", err)
	_, err = w.bank.OfferLoan(ctx, real, w.agents[real].ID, w.agents[test].ID, economy.LoanTerms{PrincipalULXC: 10 * wallLXC,
		InterestBPS: 100, Instalments: 2, Every: "week"})
	refused("a loan from real to test", err)
	_, err = w.bank.PayIntoEscrow(ctx, test, w.agents[test].ID, w.agents[real].ID, wallLXC, "deal", time.Now().Add(time.Hour))
	refused("an escrow from test to real", err)
	_, err = w.bank.CreateAgentSchedule(ctx, test, w.agents[test].ID, w.agents[real].ID, wallLXC, "retainer", "day", time.Now())
	refused("a recurring transfer from test to real", err)
	_, err = s.Use(ctx, UseDeps{}, test, "", listing.ID, UseRequest{})
	refused("a test workspace buying a real listing", err)
	res, err := minter.MintServedHit(ctx, mintHit(test, real, "answer-1"))
	if err != nil || res.Minted || res.Refused != poolroyalty.RefusedMoneyWall {
		t.Errorf("a royalty from a test requester to a real contributor = %+v, %v; want refused %q", res, err, poolroyalty.RefusedMoneyWall)
	}

	if after := w.rows(); !reflect.DeepEqual(after, before) {
		t.Fatalf("refused moves wrote rows:\nbefore %v\nafter  %v", before, after)
	}
	var bal int64
	if err := w.pool.QueryRow(ctx, `SELECT balance FROM lxc_balances WHERE workspace_id = $1`, real).Scan(&bal); err != nil || bal != 100*wallLXC {
		t.Fatalf("the real workspace's balance = %d, %v; want untouched at %d", bal, err, 100*wallLXC)
	}
}

// realFigures are the figures a test run must never move: LENS supply (the backing value), the operator screen's
// spend, LENS held and activity, and the operator's usage totals — each read for real workspaces only.
type realFigures struct {
	Supply, Circulating int64
	Spend               []opsusage.WorkspaceSpend
	Held                []opsusage.WorkspaceHeld
	Activity            []opsusage.WorkspaceActivity
	Usage               opsusage.Summary
}

func (w *wallWorld) realFigures(since time.Time) realFigures {
	w.t.Helper()
	ledger, ops := mining.NewLedgerStore(w.pool), opsusage.NewReader(w.pool)
	var f realFigures
	var err error
	if f.Supply, err = ledger.GetTotalSupply(w.ctx); err != nil {
		w.t.Fatal(err)
	}
	if f.Circulating, err = ledger.GetCirculatingSupply(w.ctx); err != nil {
		w.t.Fatal(err)
	}
	if f.Spend, err = ops.SpendByWorkspace(w.ctx, workspace.AudienceReal); err != nil {
		w.t.Fatal(err)
	}
	if f.Held, err = ops.HeldByWorkspace(w.ctx, workspace.AudienceReal); err != nil {
		w.t.Fatal(err)
	}
	if f.Activity, err = ops.ActivityByWorkspace(w.ctx, workspace.AudienceReal); err != nil {
		w.t.Fatal(err)
	}
	if f.Usage, err = ops.Summarize(w.ctx, workspace.AudienceReal, since); err != nil {
		w.t.Fatal(err)
	}
	return f
}

// B25.1 — a full test run among test workspaces (transfers, a request, an escrow, LENS earned, a royalty, marketplace
// earnings paid on a live invoice) leaves every real figure exactly as it was: LENS supply, the operator screen, and the
// live payout run, which pays the real seller its live earnings and the test seller nothing. Every row the test run
// wrote carries the test mark, and no real row does.
func TestMoneyWall_AFullTestRunLeavesEveryRealFigureAsItWas(t *testing.T) {
	const realBuyer, realSeller, tester1, tester2 = "ws-real-buyer", "ws-real-seller", "ws-tester-1", "ws-tester-2"
	w := newWallWorld(t, []string{realBuyer, realSeller}, []string{tester1, tester2})
	ctx := w.ctx
	s := NewStore(w.pool)
	ledger := mining.NewLedgerStore(w.pool)
	ledger.SetMoneyWall(workspace.CheckMoneyWall)
	since := time.Now().Add(-time.Hour)
	for _, ws := range []string{realSeller, tester2} {
		if _, err := w.pool.Exec(ctx, `INSERT INTO market_sellers (workspace_id, stripe_account_id, details_submitted, payouts_enabled)
			VALUES ($1, 'acct_'||$1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	paid := time.Now().Add(-30 * 24 * time.Hour) // past the holdback
	sale := func(id, buyer, seller string, ulxc int64) {
		t.Helper()
		if _, err := w.pool.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, price_ulxc,
			charge, used_at, ran_at, metered_at) VALUES ($1, 'lst_'||$1, 1, $2, $3, $4, 'billed', $5, $5, $5)`, id, seller, buyer, ulxc, paid.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if n, err := s.ClearInvoice(ctx, buyer, "in_"+id, paid.Add(-2*time.Hour), paid, paid, true); err != nil || n != 1 {
			t.Fatalf("clear %s's live invoice = %d, %v", id, n, err)
		}
	}

	// The real world: a transfer, LENS earned, and $50 of marketplace sales on a live invoice.
	if _, err := w.bank.SendCredits(ctx, realBuyer, w.agents[realBuyer].ID, w.agents[realSeller].ID, 5*wallLXC, "real work"); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Credit(ctx, realSeller, 7_000_000, mining.TypeCacheMine, "real mining", nil); err != nil {
		t.Fatal(err)
	}
	sale("use_real", realBuyer, realSeller, 500_000_000)
	before := w.realFigures(since)

	// The full test run, test workspace to test workspace only.
	if _, err := w.bank.SendCredits(ctx, tester1, w.agents[tester1].ID, w.agents[tester2].ID, 10*wallLXC, "test pay"); err != nil {
		t.Fatalf("a transfer between test workspaces: %v", err)
	}
	req, err := w.bank.RequestCredits(ctx, tester2, w.agents[tester2].ID, w.agents[tester1].ID, 3*wallLXC, "test invoice")
	if err != nil {
		t.Fatalf("a request between test workspaces: %v", err)
	}
	if _, err := w.bank.AnswerMoneyRequest(ctx, tester1, req.ID, true); err != nil {
		t.Fatalf("accepting it: %v", err)
	}
	esc, err := w.bank.PayIntoEscrow(ctx, tester1, w.agents[tester1].ID, w.agents[tester2].ID, 2*wallLXC, "test deal", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("an escrow between test workspaces: %v", err)
	}
	if _, err := w.bank.ConfirmEscrow(ctx, tester1, esc.ID); err != nil {
		t.Fatalf("releasing it: %v", err)
	}
	if err := ledger.Credit(ctx, tester2, 9_000_000, mining.TypeCacheMine, "test mining", nil); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Transfer(ctx, tester2, tester1, 1_000_000, "test LENS"); err != nil {
		t.Fatalf("a LENS transfer between test workspaces: %v", err)
	}
	minter := poolroyalty.NewMinter(w.pool, ledger, 0.5, func() bool { return true })
	minter.SetMoneyWall(workspace.CheckMoneyWall)
	if res, err := minter.MintServedHit(ctx, mintHit(tester1, tester2, "answer-2")); err != nil || !res.Minted {
		t.Fatalf("a royalty between test workspaces = %+v, %v; want minted, marked test", res, err)
	}
	sale("use_test", tester1, tester2, 900_000_000)

	if after := w.realFigures(since); !reflect.DeepEqual(after, before) {
		t.Fatalf("a test run moved the real figures:\nbefore %+v\nafter  %+v", before, after)
	}

	// Every row the test run wrote is marked test; no real workspace's row is.
	testers, reals := []string{tester1, tester2}, []string{realBuyer, realSeller}
	for tbl, col := range map[string]string{"lxc_ledger": "workspace_id", "agent_postings": "workspace_id", "agent_transfers": "from_workspace_id",
		"agent_money_requests": "from_workspace_id", "agent_escrows": "payer_workspace_id", "lens_token_ledger": "workspace_id",
		"pool_royalty_mints": "contributor_workspace_id", "market_uses": "seller_workspace_id", "market_earnings": "seller_workspace_id"} {
		var marked, unmarked, realMarked int
		if err := w.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE `+col+` = ANY($1) AND test), count(*) FILTER (WHERE `+col+` = ANY($1) AND NOT test),
			count(*) FILTER (WHERE `+col+` = ANY($2) AND test) FROM `+tbl, testers, reals).Scan(&marked, &unmarked, &realMarked); err != nil {
			t.Fatal(err)
		}
		if marked == 0 || unmarked != 0 || realMarked != 0 {
			t.Errorf("%s: %d test rows marked, %d unmarked, %d real rows marked test; want >0, 0, 0", tbl, marked, unmarked, realMarked)
		}
	}

	// Payouts due: a live run pays the real seller exactly its live earnings — its 85% of $50 of sales, $42.50 —
	// and the test seller — whose $90
	// was paid on a live invoice too — nothing.
	live := &connectLive{live: true}
	if _, err := s.PayOut(ctx, live, time.Now()); err != nil {
		t.Fatal(err)
	}
	payouts := map[string]int64{}
	rows, err := w.pool.Query(ctx, `SELECT workspace_id, gross_usd_micros FROM market_payouts WHERE livemode`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var ws string
		var gross int64
		if err := rows.Scan(&ws, &gross); err != nil {
			t.Fatal(err)
		}
		payouts[ws] = gross
	}
	if rows.Err() != nil || !reflect.DeepEqual(payouts, map[string]int64{realSeller: 42_500_000}) || len(live.transfers) != 1 {
		t.Fatalf("the live payout run paid %v (%d transfers, %v); want the real seller's $42.50 and nothing to the test seller",
			payouts, len(live.transfers), rows.Err())
	}
}
