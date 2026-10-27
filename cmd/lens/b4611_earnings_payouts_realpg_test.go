package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/rooms"
)

// b4611Postings is every posting of payout id's money entries, in order: "kind purpose amount".
func b4611Postings(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT e.kind, a.purpose, p.amount_minor FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
		JOIN money_accounts a ON a.id = p.account_id WHERE e.idempotency_key LIKE 'earnings_payout:' || $1 || ':%' ORDER BY e.created_at, p.line`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, purpose string
		var amount int64
		_ = rows.Scan(&kind, &purpose, &amount)
		out = append(out, kind+" "+purpose+" "+jsonOf(amount))
	}
	return strings.Join(out, "; ")
}

func b4611Verify(t *testing.T, store *economy.DualTokenStore, ws, holder string) {
	t.Helper()
	d, err := store.SavePayoutDetails(context.Background(), ws, economy.PayoutDetails{Asset: "GBP", Holder: holder, IBAN: "GB82 WEST 1234 5698 7654 32"})
	if err != nil || d.Status != economy.PayoutDetailsVerified {
		t.Fatalf("%s saves its payout details = %+v, %v; want verified by the sandbox", ws, d, err)
	}
}

func b4611Pay(t *testing.T, store *economy.DualTokenStore, ws string, e economy.Earnings) []economy.EarningsPayout {
	t.Helper()
	paid, err := store.PayEarnings(context.Background(), ws, e)
	if err != nil {
		t.Fatalf("pay out %s %s: %v", e.Kind, e.Source, err)
	}
	return paid
}

// B46.11 — through the sandbox adapter: a marketplace sale pays its seller once, a royalty pays its creator, and a
// Room's equal split pays each member who accepted its terms their share exactly once, the shares summing to the
// total; a member without verified payout details is held, not paid, and is paid once they verify them.
func TestB4611_EarningsArePaidOutOnceOnTheMoneyLayerAndAPayeeWithoutVerifiedDetailsIsHeld(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, seller, creator, member, unverified, viewer = "ws-b4611-room-owner", "ws-b4611-seller", "ws-b4611-creator", "ws-b4611-member",
		"ws-b4611-unverified", "ws-b4611-viewer"
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			return sandboxMoneyLayer(t, pool, store, reg)
		})
	agent := b.acct.AgentID
	for w, holder := range map[string]string{ws: "Room Owner", seller: "Sam Seller", creator: "Cora Creator", member: "Mo Member"} {
		b4611Verify(t, b.store, w, holder)
	}

	// A sale pays its seller once: asked again, the same payout, and its money moved once.
	sale := economy.Earnings{Kind: economy.EarningsSale, Source: "sale:lic_b4611", AgentID: agent, Currency: "GBP",
		Shares: []economy.EarningsShare{{WorkspaceID: seller, AmountMinor: 1500}}}
	p := b4611Pay(t, b.store, ws, sale)
	if len(p) != 1 || p[0].Status != economy.PaymentCompleted || p[0].Partner != money.SandboxName || p[0].PartnerRef == "" {
		t.Fatalf("the sale paid %+v, want one payout of £15.00 completed by the sandbox", p)
	}
	if again := b4611Pay(t, b.store, ws, sale); again[0].ID != p[0].ID || again[0].Status != economy.PaymentCompleted {
		t.Fatalf("the sale again = %+v, want payout %s as it stands", again, p[0].ID)
	}
	if got, want := b4611Postings(t, pool, p[0].ID), "earnings_payout_hold agent -1500; earnings_payout_hold hold 1500; earnings_payout hold -1500; earnings_payout partner 1500"; got != want {
		t.Errorf("the sale posted %q, want %q", got, want)
	}

	// A royalty pays its creator.
	if r := b4611Pay(t, b.store, ws, economy.Earnings{Kind: economy.EarningsRoyalty, Source: "royalty:lst_b4611", AgentID: agent, Currency: "GBP",
		Shares: []economy.EarningsShare{{WorkspaceID: creator, AmountMinor: 250}}}); r[0].Status != economy.PaymentCompleted || r[0].RecipientWorkspaceID != creator {
		t.Fatalf("the royalty paid %+v, want £2.50 completed to its creator", r)
	}

	// A room whose wallet is the agent, split equally among the members who accepted its terms: the owner, a member,
	// and one who has no payout details yet. A viewer is not paid.
	const room = "room_b4611"
	for _, q := range []string{
		`INSERT INTO rooms (id, owner_workspace_id, title, wallet_agent_id) VALUES ('` + room + `', '` + ws + `', 'B46.11', '` + agent + `')`,
		`INSERT INTO room_terms (room_id, version, split_rule, remix_share_bps, default_price_usd_micros, spend_policy) VALUES ('` + room + `', 1, 'equal', 0, 0, 'owner_only')`,
		`INSERT INTO room_members (room_id, workspace_id, role, terms_version, joined_at) VALUES
			('` + room + `', '` + ws + `', 'owner', 1, now() - interval '3 minutes'), ('` + room + `', '` + member + `', 'member', 1, now() - interval '2 minutes'),
			('` + room + `', '` + unverified + `', 'editor', 1, now() - interval '1 minute'), ('` + room + `', '` + viewer + `', 'viewer', 1, now())`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	// The editor saved payout details, but the partner has not verified them yet.
	if d, err := b.store.SavePayoutDetails(ctx, unverified, economy.PayoutDetails{Asset: "GBP", Holder: "TESTPENDING Ed", IBAN: "GB82WEST12345698765432"}); err != nil ||
		d.Status != economy.PayoutDetailsPending {
		t.Fatalf("the editor's details = %+v, %v; want pending verification", d, err)
	}
	rs := rooms.NewStore(pool, 0)
	if _, err := rs.Split(ctx, room, member, "split-1", rooms.SplitPayout{AmountMinor: 1000, Currency: "GBP"}); !errors.Is(err, rooms.ErrForbidden) {
		t.Fatalf("a member pays out the room = %v, want forbidden: only its owner does", err)
	}
	e, err := rs.Split(ctx, room, ws, "split-1", rooms.SplitPayout{AmountMinor: 1000, Currency: "GBP"})
	if err != nil {
		t.Fatal(err)
	}
	split := b4611Pay(t, b.store, ws, e)
	got := map[string]economy.EarningsPayout{}
	var sum int64
	for _, s := range split {
		got[s.RecipientWorkspaceID] = s
		sum += s.AmountMinor
		if s.Agreement != "room "+room+" terms v1 equal" || s.Kind != economy.EarningsRoomSplit {
			t.Errorf("payout %+v is not traced to the room's terms", s)
		}
	}
	if len(split) != 3 || sum != 1000 || got[ws].AmountMinor != 334 || got[member].AmountMinor != 333 || got[unverified].AmountMinor != 333 {
		t.Fatalf("the split paid %+v, want £3.34, £3.33 and £3.33 to the owner, the member and the editor, summing to £10.00", split)
	}
	if got[ws].Status != economy.PaymentCompleted || got[member].Status != economy.PaymentCompleted {
		t.Fatalf("the verified members' payouts = %+v and %+v, want completed", got[ws], got[member])
	}
	held := got[unverified]
	if held.Status != economy.PayoutHeld || held.PartnerRef != "" {
		t.Fatalf("the payout to a member without verified details = %+v, want held and not sent", held)
	}
	if got, want := b4611Postings(t, pool, held.ID), "earnings_payout_hold agent -333; earnings_payout_hold hold 333"; got != want {
		t.Errorf("the held payout posted %q, want only its hold %q", got, want)
	}

	// The same payout again pays nobody twice.
	e, _ = rs.Split(ctx, room, ws, "split-1", rooms.SplitPayout{AmountMinor: 1000, Currency: "GBP"})
	for _, s := range b4611Pay(t, b.store, ws, e) {
		if s.ID != got[s.RecipientWorkspaceID].ID {
			t.Fatalf("the split again made payout %s for %s, want %s", s.ID, s.RecipientWorkspaceID, got[s.RecipientWorkspaceID].ID)
		}
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM money_earnings_payouts WHERE source = $1`, e.Source).Scan(&n)
	if n != 3 {
		t.Fatalf("%d payouts of the split, want 3", n)
	}

	// Once the member verifies their details, their held payout is paid, once.
	b4611Verify(t, b.store, unverified, "Ed Editor")
	list, err := b.store.EarningsPayouts(ctx, unverified)
	if err != nil || len(list) != 1 || list[0].ID != held.ID || list[0].Status != economy.PaymentCompleted {
		t.Fatalf("the editor's payouts = %+v, %v; want %s completed", list, err, held.ID)
	}
	if got, want := b4611Postings(t, pool, held.ID), "earnings_payout_hold agent -333; earnings_payout_hold hold 333; earnings_payout hold -333; earnings_payout partner 333"; got != want {
		t.Errorf("the released payout posted %q, want %q", got, want)
	}

	// Money leaving the agent's wallet is judged by its rules: over its limit per request, nothing is held or paid.
	if _, err := b.store.SetAgentRules(ctx, ws, agent, economy.AgentRules{MaxPerRequestULXC: 1}); err != nil {
		t.Fatal(err)
	}
	if p, err := b.store.PayEarnings(ctx, ws, economy.Earnings{Kind: economy.EarningsRoyalty, Source: "royalty:over-limit", AgentID: agent,
		Currency: "GBP", Shares: []economy.EarningsShare{{WorkspaceID: creator, AmountMinor: 100}}}); err == nil {
		t.Fatalf("a payout over the agent's limit paid %+v, want refused", p)
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM money_earnings_payouts WHERE source = 'royalty:over-limit'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d payouts made over the agent's limit, want none", n)
	}
	b.balance(t, 50000-1500-250-1000)
}

// b4611Payouts is a payout partner's fake: it takes each payout and answers pending, and counts what it was asked.
type b4611Payouts struct {
	mu   sync.Mutex
	paid map[string]int
}

func (f *b4611Payouts) Pay(_ context.Context, req money.Payout) (money.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paid[req.Key]++
	return money.Result{Ref: "fp_" + req.Key, Status: money.StatusPending}, nil
}

// B46.11 — through a fake payout partner switched on for payouts: a sale's payout is sent once however often it is
// asked, waits until the partner's notification says it completed, and is settled once.
func TestB4611_APayoutOnAFakePartnerIsSentOnceAndSettledOnceByItsNotification(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, seller = "ws-b4611-fake", "ws-b4611-fake-seller"
	f := &b4611Payouts{paid: map[string]int{}}
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, ""), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
				Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool),
				Switches: money.SwitchSet{{Capability: money.CapPayouts, Partner: "fakepay"}: true}})
			if err != nil {
				t.Fatal(err)
			}
			if err := layer.Register(&money.Adapter{Name: "fakepay", Payouts: f}); err != nil {
				t.Fatal(err)
			}
			return layer
		})
	b4611Verify(t, b.store, seller, "Sam Seller")
	sale := economy.Earnings{Kind: economy.EarningsSale, Source: "sale:lic_fake", AgentID: b.acct.AgentID, Currency: "GBP",
		Shares: []economy.EarningsShare{{WorkspaceID: seller, AmountMinor: 4200}}}
	p := b4611Pay(t, b.store, ws, sale)[0]
	again := b4611Pay(t, b.store, ws, sale)[0]
	if p.Status != economy.PaymentPending || p.Partner != "fakepay" || p.PartnerRef != "fp_"+p.ID || again.ID != p.ID || f.paid[p.ID] != 1 {
		t.Fatalf("the sale twice = %+v then %+v, the partner asked %d times; want one payout sent once, pending", p, again, f.paid[p.ID])
	}
	l := moneyLedger{b.store}
	for i := 0; i < 2; i++ {
		if _, id, err := l.SettleTransfer(ctx, "fakepay", money.Event{PartnerRef: p.PartnerRef, Status: money.StatusCompleted}); err != nil || id != p.ID {
			t.Fatalf("the partner's notification %d settled %q, %v; want %s", i+1, id, err, p.ID)
		}
	}
	if got, want := b4611Postings(t, pool, p.ID), "earnings_payout_hold agent -4200; earnings_payout_hold hold 4200; earnings_payout hold -4200; earnings_payout partner 4200"; got != want {
		t.Errorf("the settled payout posted %q, want %q", got, want)
	}
	if _, _, err := l.SettleTransfer(ctx, "fakepay", money.Event{PartnerRef: "fp_nobody", Status: money.StatusCompleted}); !errors.Is(err, money.ErrNotFound) {
		t.Fatalf("a notification about no payout = %v, want not found", err)
	}
}
