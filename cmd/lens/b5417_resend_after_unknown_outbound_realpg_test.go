package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
)

// b5417TravelRule is the Test Travel Rule adapter that can go down: while down, it neither says who holds an address
// nor takes a transfer.
type b5417TravelRule struct {
	*partners.TestTravelRuleProvider
	down   atomic.Bool
	onSend func() // runs as each transfer's identity data is sent, when set
}

func (r *b5417TravelRule) Provider(ctx context.Context, network, address string) (string, error) {
	if r.down.Load() {
		return "", errors.New("the Travel Rule provider did not answer")
	}
	return r.TestTravelRuleProvider.Provider(ctx, network, address)
}

func (r *b5417TravelRule) Send(ctx context.Context, t partners.TravelRuleTransfer) (string, error) {
	if r.down.Load() {
		return "", errors.New("the Travel Rule provider did not answer")
	}
	if r.onSend != nil {
		r.onSend()
	}
	return r.TestTravelRuleProvider.Send(ctx, t)
}

// b5417Rail stands in front of Circle's fake: while lose is set, a transfer reaches Circle but its answer is lost;
// while down is set, a transfer never reaches it, nor a question about one. Either way Lens hears a 502 and cannot know whether money went.
// While screenDown is set, Circle's screening of an address does not answer.
type b5417Rail struct {
	lose, down, screenDown atomic.Bool
}

func (r *b5417Rail) via(t *testing.T) func(string) string {
	return func(target string) string {
		u, err := url.Parse(target)
		if err != nil {
			t.Fatal(err)
		}
		proxy := httputil.NewSingleHostReverseProxy(u)
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Method == http.MethodPost && req.URL.Path == "/v1/w3s/compliance/screening/addresses" && r.screenDown.Load() {
				http.Error(w, "bad gateway", http.StatusBadGateway)
				return
			}
			if req.Method == http.MethodGet && req.URL.Path == "/v1/w3s/transactions" && r.down.Load() {
				http.Error(w, "bad gateway", http.StatusBadGateway)
				return
			}
			if req.Method == http.MethodPost && req.URL.Path == "/v1/w3s/developer/transactions/transfer" {
				switch {
				case r.down.Load():
					http.Error(w, "bad gateway", http.StatusBadGateway)
					return
				case r.lose.Load():
					proxy.ServeHTTP(httptest.NewRecorder(), req)
					http.Error(w, "bad gateway", http.StatusBadGateway)
					return
				}
			}
			proxy.ServeHTTP(w, req)
		}))
		t.Cleanup(s.Close)
		return s.URL
	}
}

// B54.17 — once an Outbound may have gone, a resend never fails the payment on the Travel Rule or on screening: it
// asks the rail what happened under the same key. Here Circle made the transfer but its answer was lost; then the
// Travel Rule provider is down for longer than TravelRuleMaxAttempts reads and the address is flagged meanwhile. The
// payment stays held, nothing is given back, and when the rail answers it is sent once.
func TestB5417_AnUnknownOutboundThenATravelRuleOutageKeepsThePaymentHeldAndSendsItOnce(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5417-out"
	const exchange = "0x00000000000000000000000000000000000B5417"
	flagged := b487Screening{}
	travel := &b5417TravelRule{TestTravelRuleProvider: &partners.TestTravelRuleProvider{VASPs: map[string]string{
		strings.ToLower(exchange): "Example Exchange Ltd"}}}
	rail := &b5417Rail{}
	b := newB487PayerWith(t, pool, ws, flagged, travel, rail.via(t), nil)
	const held = "payment_out_hold agent -25000000; payment_out_hold hold 25000000"

	rail.lose.Store(true)
	p, err := b.pay(ws, b.save(t, pool, ws, "Ada Lovelace", exchange), "unknown", 25_000000)
	if err != nil || p.Status != economy.PaymentPending || b.circle.Outbound() != 1 || len(travel.Sent()) != 1 {
		t.Fatalf("pay while Circle's answer is lost = %+v, %v with %d transfers and %d Travel Rule sends; want it pending, sent once each",
			p, err, b.circle.Outbound(), len(travel.Sent()))
	}

	rail.lose.Store(false)
	rail.down.Store(true)
	travel.down.Store(true)
	flagged[strings.ToLower(exchange)] = partners.AddressSanctioned
	for i := 0; i < economy.TravelRuleMaxAttempts+2; i++ {
		if p, err = b.store.RefreshOutsidePayment(ctx, ws, "", p.ID); err != nil || p.Status != economy.PaymentPending {
			t.Fatalf("resend %d while the Travel Rule is down = %+v, %v; want it still pending: the first transfer may have paid", i+1, p, err)
		}
	}
	if got := b468Postings(t, pool, ws, "unknown"); got != held {
		t.Fatalf("while the rail is silent the payment posted %q, want %q: still held, nothing given back", got, held)
	}

	rail.down.Store(false)
	if p, err = b.store.RefreshOutsidePayment(ctx, ws, "", p.ID); err != nil || p.Status != economy.PaymentSent || p.PartnerRef == "" {
		t.Fatalf("resend once the rail answers = %+v, %v; want it sent, with Circle's transfer", p, err)
	}
	if b.circle.Outbound() != 1 || len(travel.Sent()) != 1 {
		t.Fatalf("Circle made %d transfers and the Travel Rule was sent %d times, want one each", b.circle.Outbound(), len(travel.Sent()))
	}
	if got := b468Postings(t, pool, ws, "unknown"); got != held {
		t.Fatalf("the sent payment posted %q, want %q once", got, held)
	}
}

// b5417Payouts is a payout partner's fake whose answer can be lost after it took the payout. to is the last address
// each payout was asked to go to.
type b5417Payouts struct {
	lose, refuse atomic.Bool
	mu           sync.Mutex
	paid         map[string]int
	to           map[string]string
}

func (f *b5417Payouts) Pay(_ context.Context, req money.Payout) (money.Result, error) {
	f.mu.Lock()
	f.paid[req.Key]++
	f.to[req.Key] = req.To.Address
	f.mu.Unlock()
	if f.lose.Load() {
		return money.Result{}, money.ErrPartnerDown
	}
	if f.refuse.Load() {
		return money.Result{}, money.ErrRefused
	}
	return money.Result{Ref: "fp_" + req.Key, Status: money.StatusPending}, nil
}

func (f *b5417Payouts) times(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paid[key]
}

// B54.17 — a USDC earnings payout takes the same rule: a Travel Rule error waits and is tried again, ending the payout
// as failed only after TravelRuleMaxAttempts with nothing paid out; and once the payout may have gone, a resend never
// fails it on the Travel Rule or on screening, and is asked of the partner under the same key.
func TestB5417_AnEarningsPayoutRetriesTheTravelRuleAndNeverFailsOnceItMayHaveGone(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, seller = "ws-b5417-earnings", "ws-b5417-seller"
	const exchange = "0x00000000000000000000000000000000000b5418"
	flagged := b487Screening{}
	travel := &b5417TravelRule{TestTravelRuleProvider: &partners.TestTravelRuleProvider{VASPs: map[string]string{exchange: "Example Exchange Ltd"}}}
	f := &b5417Payouts{paid: map[string]int{}, to: map[string]string{}}
	b := newB487PayerWith(t, pool, ws, flagged, travel, nil, f)
	if d, err := b.store.SavePayoutDetails(ctx, seller, economy.PayoutDetails{Asset: money.USDC, Holder: "Sam Seller", Network: "base",
		Address: exchange}); err != nil || d.Status != economy.PayoutDetailsVerified {
		t.Fatalf("the seller saves its USDC payout details = %+v, %v; want verified", d, err)
	}
	sale := func(source string) economy.EarningsPayout {
		t.Helper()
		return b4611Pay(t, b.store, ws, economy.Earnings{Kind: economy.EarningsSale, Source: source, AgentID: b.agentID, Currency: money.USDC,
			Shares: []economy.EarningsShare{{WorkspaceID: seller, AmountMinor: 30_000000}}})[0]
	}
	const held = "earnings_payout_hold agent -30000000; earnings_payout_hold hold 30000000"

	// The Travel Rule is down before anything is paid: the payout waits, tried again, and fails only after the bounded tries.
	travel.down.Store(true)
	p := sale("sale:b5417-down")
	for i := 1; i < economy.TravelRuleMaxAttempts; i++ {
		if p.Status != economy.PaymentPending {
			t.Fatalf("try %d while the Travel Rule is down = %+v; want the payout waiting", i, p)
		}
		p = sale("sale:b5417-down")
	}
	if p.Status != economy.PaymentFailed || !strings.Contains(p.Detail, "Travel Rule") || f.times(p.ID) != 0 {
		t.Fatalf("after %d tries the payout is %+v, the partner asked %d times; want it failed for the Travel Rule, never paid",
			economy.TravelRuleMaxAttempts, p, f.times(p.ID))
	}
	if got, want := b4611Postings(t, pool, p.ID), held+"; earnings_payout_failed hold -30000000; earnings_payout_failed agent 30000000"; got != want {
		t.Fatalf("the failed payout posted %q, want %q: held, then given back", got, want)
	}

	// The partner took the payout but its answer was lost: the Travel Rule going down and the address flagged change nothing.
	travel.down.Store(false)
	f.lose.Store(true)
	p = sale("sale:b5417-lost")
	if p.Status != economy.PaymentPending || f.times(p.ID) != 1 {
		t.Fatalf("a payout whose answer was lost = %+v, the partner asked %d times; want it pending, asked once", p, f.times(p.ID))
	}
	travel.down.Store(true)
	flagged[exchange] = partners.AddressSanctioned
	for i := 0; i < economy.TravelRuleMaxAttempts+1; i++ {
		if p = sale("sale:b5417-lost"); p.Status != economy.PaymentPending {
			t.Fatalf("resend %d while the Travel Rule is down = %+v; want it still pending: the first payout may have gone", i+1, p)
		}
	}
	f.lose.Store(false)
	if p = sale("sale:b5417-lost"); p.Status != economy.PaymentPending || p.PartnerRef != "fp_"+p.ID {
		t.Fatalf("resend once the partner answers = %+v; want it pending with the partner's reference", p)
	}
	if got := b4611Postings(t, pool, p.ID); got != held {
		t.Fatalf("the payout posted %q, want %q: held once, never given back", got, held)
	}
	if len(travel.Sent()) != 1 {
		t.Fatalf("the Travel Rule was sent %d times, want once", len(travel.Sent()))
	}

	// Its details changed after a send whose answer was lost: a resend never pays the new details, unscreened.
	travel.down.Store(false)
	delete(flagged, exchange)
	f.lose.Store(true)
	p = sale("sale:b5417-moved")
	const moved = "0x00000000000000000000000000000000000b5419"
	if d, err := b.store.SavePayoutDetails(ctx, seller, economy.PayoutDetails{Asset: money.USDC, Holder: "Sam Seller", Network: "base",
		Address: moved}); err != nil || d.Status != economy.PayoutDetailsVerified {
		t.Fatalf("the seller changes its USDC payout details = %+v, %v; want verified", d, err)
	}
	if p = sale("sale:b5417-moved"); p.Status != economy.PaymentPending || f.times(p.ID) != 1 || f.to[p.ID] != exchange {
		t.Fatalf("resend after the details changed = %+v, the partner asked %d times, last to %s; want it waiting, asked once, to %s",
			p, f.times(p.ID), f.to[p.ID], exchange)
	}
}

// b5417Started is a payment to a fresh address whose transfer Circle made but whose answer was lost: it may have paid.
func b5417Started(t *testing.T, pool *pgxpool.Pool, ws, address string) (b487Payer, *b5417Rail, economy.OutsidePayment) {
	t.Helper()
	rail := &b5417Rail{}
	b := newB487PayerWith(t, pool, ws, b487Screening{}, nil, rail.via(t), nil)
	rail.lose.Store(true)
	p, err := b.pay(ws, b.save(t, pool, ws, "Ada Lovelace", address), "unknown", 25_000000)
	if err != nil || p.Status != economy.PaymentPending || b.circle.Outbound() != 1 {
		t.Fatalf("pay while Circle's answer is lost = %+v, %v with %d transfers; want it pending, sent once", p, err, b.circle.Outbound())
	}
	rail.lose.Store(false)
	return b, rail, p
}

// b5417StillHeld refreshes p twice and fails unless it stays pending with its money held and nothing more sent.
func b5417StillHeld(t *testing.T, pool *pgxpool.Pool, b b487Payer, ws string, p economy.OutsidePayment, why string) {
	t.Helper()
	for i := 0; i < 2; i++ {
		q, err := b.store.RefreshOutsidePayment(context.Background(), ws, "", p.ID)
		if err != nil || q.Status != economy.PaymentPending {
			t.Fatalf("resend %d %s = %+v, %v; want it still pending: the first transfer may have paid", i+1, why, q, err)
		}
	}
	if got, want := b468Postings(t, pool, ws, "unknown"), "payment_out_hold agent -25000000; payment_out_hold hold 25000000"; got != want {
		t.Fatalf("%s the payment posted %q, want %q: still held, nothing given back", why, got, want)
	}
	if b.circle.Outbound() != 1 {
		t.Fatalf("%s Circle made %d transfers, want one", why, b.circle.Outbound())
	}
}

// B54.17 — the agent paused by the time its payment is resent: the limits gate refusing never fails a payment that
// may have paid. Resumed, the rail is asked under the same key and it is sent once.
func TestB5417_APausedAgentNeverFailsAPaymentThatMayHavePaid(t *testing.T) {
	pool := agentRoutesDB(t)
	const ws = "ws-b5417-paused"
	b, _, p := b5417Started(t, pool, ws, "0x00000000000000000000000000000000000B5420")
	if err := b.store.PauseAgent(context.Background(), ws, b.agentID, "b5417"); err != nil {
		t.Fatal(err)
	}
	b5417StillHeld(t, pool, b, ws, p, "while the agent is paused")
	if err := b.store.ResumeAgent(context.Background(), ws, b.agentID); err != nil {
		t.Fatal(err)
	}
	if p, err := b.store.RefreshOutsidePayment(context.Background(), ws, "", p.ID); err != nil || p.Status != economy.PaymentSent || b.circle.Outbound() != 1 {
		t.Fatalf("resend once resumed = %+v, %v with %d transfers; want it sent, once", p, err, b.circle.Outbound())
	}
}

// B54.17 — Circle's screening down when a payment is resent: Circle answers the transfer it made under the key, with
// no screening before it, so the payment is sent once and never failed.
func TestB5417_ACircleScreeningOutageNeverFailsAPaymentThatMayHavePaid(t *testing.T) {
	pool := agentRoutesDB(t)
	const ws = "ws-b5417-screen"
	b, rail, p := b5417Started(t, pool, ws, "0x00000000000000000000000000000000000B5421")
	rail.screenDown.Store(true)
	if p, err := b.store.RefreshOutsidePayment(context.Background(), ws, "", p.ID); err != nil || p.Status != economy.PaymentSent || b.circle.Outbound() != 1 {
		t.Fatalf("resend while Circle's screening is down = %+v, %v with %d transfers; want it sent, once", p, err, b.circle.Outbound())
	}
}

// B54.17 — the payee's details edited after a send whose answer was lost: the payment is never sent to the new
// details, and never failed with its money back either: it waits.
func TestB5417_APayeeEditAfterAnUnknownSendKeepsThePaymentHeld(t *testing.T) {
	pool := agentRoutesDB(t)
	const ws = "ws-b5417-edit"
	b, _, p := b5417Started(t, pool, ws, "0x00000000000000000000000000000000000B5422")
	if _, err := pool.Exec(context.Background(), `UPDATE money_payees SET address = '0x00000000000000000000000000000000000B5423',
		details_changed_at = now() WHERE id = $1`, p.PayeeID); err != nil {
		t.Fatal(err)
	}
	b5417StillHeld(t, pool, b, ws, p, "after the payee's details changed")
}

// B54.17 — an earnings payout that may have gone: a refusal from the partner on a resend never fails it, and a send
// racing another one to other details never pays them.
func TestB5417_AStartedEarningsPayoutIsNeverFailedOrSentToOtherDetails(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, seller = "ws-b5417-payout-race", "ws-b5417-payout-seller"
	const exchange = "0x00000000000000000000000000000000000b5424"
	travel := &b5417TravelRule{TestTravelRuleProvider: &partners.TestTravelRuleProvider{VASPs: map[string]string{exchange: "Example Exchange Ltd"}}}
	f := &b5417Payouts{paid: map[string]int{}, to: map[string]string{}}
	b := newB487PayerWith(t, pool, ws, b487Screening{}, travel, nil, f)
	if d, err := b.store.SavePayoutDetails(ctx, seller, economy.PayoutDetails{Asset: money.USDC, Holder: "Sam Seller", Network: "base",
		Address: exchange}); err != nil || d.Status != economy.PayoutDetailsVerified {
		t.Fatalf("the seller saves its USDC payout details = %+v, %v; want verified", d, err)
	}
	sale := func(source string) economy.EarningsPayout {
		t.Helper()
		return b4611Pay(t, b.store, ws, economy.Earnings{Kind: economy.EarningsSale, Source: source, AgentID: b.agentID, Currency: money.USDC,
			Shares: []economy.EarningsShare{{WorkspaceID: seller, AmountMinor: 30_000000}}})[0]
	}
	const held = "earnings_payout_hold agent -30000000; earnings_payout_hold hold 30000000"

	f.lose.Store(true)
	p := sale("sale:b5417-refused")
	f.lose.Store(false)
	f.refuse.Store(true)
	for i := 0; i < 2; i++ {
		if p = sale("sale:b5417-refused"); p.Status != economy.PaymentPending {
			t.Fatalf("resend %d refused by the partner = %+v; want it still pending: the first payout may have gone", i+1, p)
		}
	}
	if got := b4611Postings(t, pool, p.ID); got != held {
		t.Fatalf("the refused resend posted %q, want %q: still held, nothing given back", got, held)
	}
	f.refuse.Store(false)

	// Another send of the same payout, to other details, got to the partner first, as this one sent its Travel Rule data.
	travel.onSend = func() {
		if _, err := pool.Exec(ctx, `UPDATE money_earnings_payouts SET sent_to = 'Sam Seller||||base|0x00000000000000000000000000000000000b5425'
			WHERE recipient_workspace_id = $1 AND source = 'sale:b5417-race'`, seller); err != nil {
			t.Error(err)
		}
	}
	if p = sale("sale:b5417-race"); p.Status != economy.PaymentPending || f.times(p.ID) != 0 {
		t.Fatalf("a send racing one to other details = %+v, the partner asked %d times; want it waiting, never paid to these details", p, f.times(p.ID))
	}
}
