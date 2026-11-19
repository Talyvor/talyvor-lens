package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/issuerfake"
	"github.com/talyvor/lens/internal/partners"
)

// B50.56 — a card charge screens its payment to the merchant on a connection of its own before its transaction
// begins, and reads the account it pays from on that transaction, so as many charges at once as the pool has
// connections still finish: none holds one connection while it waits for a second.
func TestB5056_EightCardChargesAtOnceOnAFourConnectionPoolAllFinish(t *testing.T) {
	ctx := context.Background()
	cfg := agentRoutesDB(t).Config()
	cfg.MaxConns = 4 // CI's size
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	const ws = "ws-b5056-pool"
	var issuer *issuerfake.Issuer
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b5056"), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
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
	var refs []string
	for i := range 8 {
		card, err := b.store.IssueReusableCard(ctx, ws, economy.ReusableCard{AgentID: b.acct.AgentID, Currency: "GBP", PerPaymentLimitMinor: 100_00,
			DailyLimitMinor: 100_00, IdempotencyKey: fmt.Sprintf("b5056-card-%d", i)})
		if err != nil || card.Status != economy.ReusableCardActive {
			t.Fatalf("card %d = %+v %v, want active", i, card, err)
		}
		var ref string
		if err := pool.QueryRow(ctx, `SELECT partner_ref FROM money_reusable_cards WHERE id = $1`, card.ID).Scan(&ref); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	oks := make([]bool, len(refs))
	reasons := make([]string, len(refs))
	errs := make([]error, len(refs))
	start := make(chan struct{})
	for i, ref := range refs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			oks[i], reasons[i], errs[i] = issuer.Charge(cctx, ref, "OpenAI", money.Amount{Minor: 10_00, Asset: "GBP"})
		}()
	}
	close(start)
	wg.Wait()
	for i := range refs {
		if errs[i] != nil || !oks[i] {
			t.Errorf("charge %d = %v %q %v; want approved", i, oks[i], reasons[i], errs[i])
		}
	}
	var charges, keys int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(DISTINCT idempotency_key) FROM money_entries WHERE workspace_id = $1 AND kind = 'card_charge'`,
		ws).Scan(&charges, &keys); err != nil {
		t.Fatal(err)
	}
	if charges != 8 || keys != 8 {
		t.Errorf("the ledger holds %d card charges under %d keys; want each of the 8 once", charges, keys)
	}
	b.balance(t, 500_00-8*10_00)
}
