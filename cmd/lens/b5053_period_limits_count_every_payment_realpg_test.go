package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/issuerfake"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/workspace"
)

// B50.53 on real Postgres — an agent's daily limit counts every kind of money it moves out of its currency account, not
// only its payments out of Talyvor: its Instant transfers, its payments to people, its payments to other agents in
// currency and its card spends. Each kind is paid twice under a daily limit the two together pass, each payment within
// the limit alone; the second is refused by the daily limit and the ledger holds one.
func TestB5053_DailyLimitCountsEveryKindOfPayment(t *testing.T) {
	ctx := context.Background()
	refusedByDay := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, economy.ErrAgentRule) || !strings.Contains(err.Error(), "daily limit") {
			t.Fatalf("the second payment: %v, want it refused by the agent's daily limit", err)
		}
	}
	entries := func(t *testing.T, pool *pgxpool.Pool, ws, kind string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_entries WHERE workspace_id = $1 AND kind = $2`, ws, kind).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	approveLatest := func(t *testing.T, pool *pgxpool.Pool, store *economy.DualTokenStore, ws, agent string) {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `SELECT id FROM agent_approvals WHERE agent_id = $1 AND status = 'pending' ORDER BY created_at DESC LIMIT 1`,
			agent).Scan(&id); err != nil {
			t.Fatalf("the approval the first payment filed: %v", err)
		}
		if _, err := store.DecideAgentApproval(ctx, ws, id, true); err != nil {
			t.Fatal(err)
		}
	}
	daily := func(t *testing.T, store *economy.DualTokenStore, ws, agent, currency string) {
		t.Helper()
		on := true
		if _, err := store.SetAgentRules(ctx, ws, agent, economy.AgentRules{Currency: currency, DailyLimitMinor: 100_00, MayInstantTransfer: &on}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a payment to another agent in currency", func(t *testing.T) {
		pool := agentRoutesDB(t)
		const ws = "ws-b5053-agent"
		b := newB468Payer(t, pool, ws, "EUR", economy.OutsidePayee{Name: "Grace Hopper", Country: "DE", IBAN: "DE89370400440532013000", BIC: "COBADEFFXXX"},
			func(store *economy.DualTokenStore, r *partners.Registry) *money.Registry {
				return sandboxMoneyLayer(t, pool, store, r)
			})
		payee, err := b.store.CreateAgent(ctx, ws, "Ledger", "owner")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.store.OpenCurrencyAccount(ctx, ws, payee.ID, "EUR", "test"); err != nil {
			t.Fatal(err)
		}
		daily(t, b.store, ws, b.acct.AgentID, "EUR")
		pay := func(key string) error {
			_, err := b.store.PayAgentMoney(ctx, ws, b.acct.AgentID, economy.AgentMoneyPayment{ToAgentID: payee.ID, AmountMinor: 60_00,
				Currency: "EUR", Funding: economy.FundingTest, IdempotencyKey: key})
			return err
		}
		if err := pay("first"); err != nil {
			t.Fatalf("the first €60.00: %v", err)
		}
		refusedByDay(t, pay("second"))
		if n := entries(t, pool, ws, "agent_payment"); n != 1 {
			t.Fatalf("the ledger holds %d payments to the agent, want 1", n)
		}
		b.balance(t, 500_00-60_00)
	})

	t.Run("a payment to a person", func(t *testing.T) {
		pool := agentRoutesDB(t)
		const ws = "ws-b5053-person"
		confirmedAccounts(t, pool, ws)
		b := newB468Payer(t, pool, ws, "EUR", economy.OutsidePayee{Name: "Grace Hopper", Country: "DE", IBAN: "DE89370400440532013000", BIC: "COBADEFFXXX"},
			func(store *economy.DualTokenStore, r *partners.Registry) *money.Registry {
				return sandboxMoneyLayer(t, pool, store, r)
			})
		exec := func(sql string, args ...any) {
			t.Helper()
			if _, err := pool.Exec(ctx, sql, args...); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		verified := func(w, name string) {
			exec(`INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref, verified_name)
				VALUES ($1, 2, 'person', 'test', 'completed', $2, $3)`, w, "ev-"+w, name)
		}
		verified(ws, "Ada Lovelace")
		const phone = "+447700900123"
		grace := workspace.FirstAccountID("test|per_grace")
		exec(`INSERT INTO people (id, phone, identity, phone_sandbox, phone_confirmed_at) VALUES ('per_grace', $1, 'test|per_grace', true, now())`, phone)
		exec(`INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, 'Grace Hopper', $1, 'personal', 'hybrid')`, grace)
		verified(grace, "Grace Hopper")
		confirmedAccounts(t, pool, grace)
		if _, err := b.store.OpenCurrencyAccount(ctx, grace, "", "EUR", "test"); err != nil {
			t.Fatal(err)
		}
		r := chi.NewRouter()
		mountPeopleFindingRoutes(r, "provision-secret", b.store)
		req := httptest.NewRequest(http.MethodPost, "/v1/people/finding/change", strings.NewReader(`{"person_id":"per_grace","name":"grace.h","by_phone":true}`))
		req.Header.Set(provisionSecretHeader, "provision-secret")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("Grace says yes to being found by her number: %d %s", rec.Code, rec.Body.String())
		}
		daily(t, b.store, ws, b.acct.AgentID, "EUR")
		pay := func(key string) error {
			_, err := b.store.PayPerson(ctx, ws, economy.PersonPayment{AgentID: b.acct.AgentID, To: phone, AmountMinor: 60_00, Currency: "EUR",
				Funding: economy.FundingTest, IdempotencyKey: key})
			return err
		}
		// The agent's first payment to her waits for the owner, then goes through.
		var need *economy.ApprovalNeededError
		if err := pay("first"); !errors.As(err, &need) {
			t.Fatalf("the first €60.00 to a new person: %v, want it to wait for the owner", err)
		}
		approveLatest(t, pool, b.store, ws, b.acct.AgentID)
		if err := pay("first"); err != nil {
			t.Fatalf("the first €60.00, approved: %v", err)
		}
		refusedByDay(t, pay("second"))
		if n := entries(t, pool, ws, "person_payment"); n != 1 {
			t.Fatalf("the ledger holds %d payments to the person, want 1", n)
		}
		b.balance(t, 500_00-60_00)
	})

	t.Run("an Instant transfer", func(t *testing.T) {
		pool := agentRoutesDB(t)
		const ws, to = "ws-b5053-instant", "ws-b5053-instant-to"
		confirmedAccounts(t, pool, ws, to)
		for _, w := range []string{ws, to} {
			if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, $1, $1, 'business', 'hybrid')`,
				w); err != nil {
				t.Fatal(err)
			}
		}
		registry := partners.NewRegistry(nil)
		store := economy.NewDualTokenStore(nil, pool, nil)
		store.SetAccountPartners(registry)
		store.SetScreener(screening.NewScreener(pool, registry))
		store.SetFXMargin(func(context.Context, economy.FeeQuerier, string) (int64, string, error) { return 50, "team", nil })
		layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(registry, "b5053"), Limits: store.MoneyLimits(), Audit: store,
			Hooks: store.InstantTransferHooks(&partners.TestTransferHooks{})})
		if err != nil {
			t.Fatal(err)
		}
		store.SetMoneyLayer(layer)
		agent, err := store.CreateAgent(ctx, ws, "Payer", "owner")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.OpenCurrencyAccount(ctx, ws, "", "GBP", "test"); err != nil {
			t.Fatal(err)
		}
		from, err := store.OpenCurrencyAccount(ctx, ws, agent.ID, "GBP", "test")
		if err != nil {
			t.Fatal(err)
		}
		var topUp string
		if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = 'GBP'`, ws).Scan(&topUp); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
			IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
				{AccountID: topUp, AmountMinor: -500_00}, {AccountID: from.ID, AmountMinor: 500_00}}}); err != nil {
			t.Fatal(err)
		}
		euros, err := store.OpenCurrencyAccount(ctx, to, "", "EUR", "test")
		if err != nil {
			t.Fatal(err)
		}
		daily(t, store, ws, agent.ID, "GBP")
		send := func(key string) error {
			t.Helper()
			q, err := store.QuoteInstantTransfer(ctx, ws, economy.InstantQuoteRequest{SendMinor: 60_00, SendCurrency: "GBP", ReceiveCurrency: "EUR",
				To: economy.InstantToAccount})
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.SendInstantTransfer(ctx, ws, agent.ID, economy.InstantSend{QuoteID: q.ID, ToAccountID: euros.ID, IdempotencyKey: key})
			return err
		}
		// The first transfer to an account the agent has never paid waits for the owner, then goes through.
		var need *economy.ApprovalNeededError
		if err := send("first-asked"); !errors.As(err, &need) {
			t.Fatalf("the first £60.00 to a new account: %v, want it to wait for the owner", err)
		}
		approveLatest(t, pool, store, ws, agent.ID)
		if err := send("first"); err != nil {
			t.Fatalf("the first £60.00, approved: %v", err)
		}
		refusedByDay(t, send("second"))
		if n := entries(t, pool, ws, "instant_transfer_convert"); n != 1 {
			t.Fatalf("the ledger holds %d Instant transfers, want 1", n)
		}
		if got, err := store.Balance(ctx, ws, from.ID); err != nil || got.TestMinor != 500_00-60_00 {
			t.Fatalf("the agent's account holds %+v (%v), want 44000", got, err)
		}
	})

	t.Run("a card spend", func(t *testing.T) {
		pool := agentRoutesDB(t)
		const ws = "ws-b5053-card"
		b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
			func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
				layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b5053"), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
					Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool),
					Switches: money.SwitchSet{{Capability: money.CapCards, Partner: issuerfake.Name}: true}})
				if err != nil {
					t.Fatal(err)
				}
				if err := layer.Register(issuerfake.New(store.AuthorizeCardCharge).Adapter()); err != nil {
					t.Fatal(err)
				}
				return layer
			})
		daily(t, b.store, ws, b.acct.AgentID, "GBP")
		spend := func(key string) error {
			_, err := b.store.RequestCardSpend(ctx, ws, economy.CardSpend{AgentID: b.acct.AgentID, Merchant: "Figma", AmountMinor: 60_00,
				Currency: "GBP", IdempotencyKey: key})
			return err
		}
		if err := spend("first"); err != nil {
			t.Fatalf("the first £60.00 by card: %v", err)
		}
		refusedByDay(t, spend("second"))
		if n := entries(t, pool, ws, "card_hold"); n != 1 {
			t.Fatalf("the ledger holds %d card holds, want 1", n)
		}
		b.balance(t, 500_00-60_00)
	})
}
