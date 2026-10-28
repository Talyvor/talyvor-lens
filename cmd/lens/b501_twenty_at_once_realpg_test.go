package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/issuerfake"
	"github.com/talyvor/lens/internal/partners"
)

// b501AtOnce runs fn 20 times at once and waits for all of them.
func b501AtOnce(fn func()) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fn()
		}()
	}
	close(start)
	wg.Wait()
}

// b501Balanced fails the test unless every money entry of ws sums to zero per currency and funding.
func b501Balanced(t *testing.T, pool *pgxpool.Pool, ws string) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM (SELECT 1 FROM money_postings p JOIN money_entries e ON e.id = p.entry_id
		WHERE e.workspace_id = $1 GROUP BY p.entry_id, p.currency, p.funding HAVING sum(p.amount_minor) <> 0) x`, ws).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d of %s's money entries do not sum to zero per currency and funding (%v)", n, ws, err)
	}
}

// B50.1 — the same card batch approval, payout batch approval, standing-order tick or payment out sent 20 times at once
// moves money once, read from the ledger's rows.
func TestB501_TwentyIdenticalRequestsAtOnceMoveMoneyOnce(t *testing.T) {
	ctx := context.Background()
	pool := agentRoutesDB(t)
	ada := economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"}
	count := func(t *testing.T, sql string, args ...any) (n int, sum int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n, &sum); err != nil {
			t.Fatal(err)
		}
		return n, sum
	}

	t.Run("card batch approval", func(t *testing.T) {
		const ws = "ws-b501-cards"
		var issuer *issuerfake.Issuer
		b := newB468Payer(t, pool, ws, "GBP", ada, func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b501"), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
				Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool),
				Switches: money.SwitchSet{{Capability: money.CapCards, Partner: issuerfake.Name}: true}})
			if err != nil {
				t.Fatal(err)
			}
			issuer = issuerfake.New(store.AuthorizeCardCharge)
			if err := layer.Register(issuer.Adapter()); err != nil {
				t.Fatal(err)
			}
			return layer
		})
		if _, err := b.store.SetAgentRules(ctx, ws, b.acct.AgentID, economy.AgentRules{ApprovalAboveULXC: 1}); err != nil {
			t.Fatal(err)
		}
		batch, err := b.store.RequestCardBatch(ctx, ws, economy.CardBatch{AgentID: b.acct.AgentID, Purpose: "Book a trip", Currency: "GBP",
			IdempotencyKey: "trip", Cards: []economy.MoneyCard{{Merchant: "British Airways", CapMinor: 20000}, {Merchant: "Hertz", CapMinor: 5000}}})
		if err != nil || batch.Status != economy.CardBatchAwaitingApproval {
			t.Fatalf("ask for two cards = %+v, %v; want a batch awaiting approval", batch, err)
		}
		b501AtOnce(func() { _, _ = b.store.ApproveCardBatch(ctx, ws, batch.ID, "owner") })
		got, want := b4610Postings(t, pool, ws), "card_hold agent -25000; card_hold hold 25000"
		if strings.Count(got, "card_hold agent") != 1 {
			t.Fatalf("20 approvals at once posted %q, want the cards' caps held once: %q", got, want)
		}
		if got != want {
			t.Fatalf("20 approvals at once posted %q, want %q: a second approval released the cards the first issued", got, want)
		}
		if _, ok := issuer.Card("ic_0003"); ok {
			t.Fatal("20 approvals at once issued a third card")
		}
		b.balance(t, 50000-25000)
		b501Balanced(t, pool, ws)
	})

	t.Run("payout batch approval", func(t *testing.T) {
		const ws = "ws-b501-payouts"
		b := newB468Payer(t, pool, ws, "GBP", ada, func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			return sandboxMoneyLayer(t, pool, store, reg)
		})
		var rows []economy.PayoutRow
		for _, minor := range []int64{1000, 2000, 3000} {
			rows = append(rows, economy.PayoutRow{PayeeID: b.payee.ID, AmountMinor: minor, Currency: "GBP", Reference: "Pay " + jsonOf(minor)})
		}
		batch, err := b.store.CreatePayoutBatch(ctx, ws, economy.PayoutBatch{AgentID: b.acct.AgentID, IdempotencyKey: "payroll", Rows: rows})
		if err != nil || batch.RowsValid != 3 {
			t.Fatalf("upload three payouts = %+v, %v", batch, err)
		}
		b501AtOnce(func() { _, _ = b.store.ApprovePayoutBatch(ctx, ws, batch.ID, "owner") })
		if n, sum := count(t, `SELECT count(*), COALESCE(sum(amount_minor), 0)::bigint FROM money_payments WHERE workspace_id = $1`, ws); n != 3 || sum != 6000 {
			t.Fatalf("20 approvals at once made %d payments out of %d pence, want 3 of 6000", n, sum)
		}
		b.balance(t, 50000-6000)
		b501Balanced(t, pool, ws)
	})

	t.Run("standing order tick", func(t *testing.T) {
		const ws = "ws-b501-standing"
		b := newB468Payer(t, pool, ws, "GBP", ada, func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			return sandboxMoneyLayer(t, pool, store, reg)
		})
		t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
		if _, err := b.store.CreateStandingOrder(ctx, ws, b.acct.AgentID, b.payee.ID, 2000, "GBP", "Rent", "week", t0, nil); err != nil {
			t.Fatal(err)
		}
		b501AtOnce(func() { _, _ = b.store.RunAgentSchedules(ctx, t0) })
		if n, sum := count(t, `SELECT count(*), COALESCE(sum(amount_minor), 0)::bigint FROM money_payments WHERE workspace_id = $1`, ws); n != 1 || sum != 2000 {
			t.Fatalf("20 runs of one tick at once made %d payments out of %d pence, want 1 of 2000", n, sum)
		}
		b.balance(t, 50000-2000)
		b501Balanced(t, pool, ws)
	})

	t.Run("payment out", func(t *testing.T) {
		const ws = "ws-b501-pay"
		b := newB468Payer(t, pool, ws, "GBP", ada, func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			return sandboxMoneyLayer(t, pool, store, reg)
		})
		b501AtOnce(func() {
			_, _ = b.store.PayOutside(ctx, ws, economy.OutsidePayment{AccountID: b.acct.ID, PayeeID: b.payee.ID, Currency: "GBP", AmountMinor: 2000,
				Reference: "Invoice 42", IdempotencyKey: "invoice-42"})
		})
		if n, sum := count(t, `SELECT count(*), COALESCE(sum(amount_minor), 0)::bigint FROM money_payments WHERE workspace_id = $1`, ws); n != 1 || sum != 2000 {
			t.Fatalf("20 payments at once under one key made %d payments out of %d pence, want 1 of 2000", n, sum)
		}
		b.balance(t, 50000-2000)
		b501Balanced(t, pool, ws)
	})
}
