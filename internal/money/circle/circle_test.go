package circle_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/circle"
	"github.com/talyvor/lens/internal/money/circle/circlefake"
	"github.com/talyvor/lens/internal/money/moneytest"
	"github.com/talyvor/lens/internal/partners"
)

var ctx = context.Background()

// allowlist is the addresses the owner approved, for any wallet.
type allowlist map[string]bool

func (a allowlist) Approved(_ context.Context, _, network, address string) (bool, error) {
	return network == "base" && a[strings.ToLower(address)], nil
}

func newCircle(t *testing.T, f *circlefake.Fake, approved ...string) *money.Adapter {
	t.Helper()
	al := allowlist{}
	for _, a := range approved {
		al[strings.ToLower(a)] = true
	}
	a, err := circle.New(circle.Config{BaseURL: f.URL, APIKey: f.APIKey, EntitySecret: f.EntitySecret, WalletSetID: f.WalletSetID,
		Blockchain: circlefake.Blockchain, Allowlist: al})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// B46.5 — Circle's adapter, against a fake of Circle's API, passes the contract every adapter passes: wallets and
// transfers in USDC; the capabilities Circle does not offer are skipped.
func TestCirclePassesTheContract(t *testing.T) {
	moneytest.ContractWithDeposits(t, func() (*money.Adapter, moneytest.Depositor) {
		f := circlefake.New(t)
		return newCircle(t, f, moneytest.PayeeFor(money.USDC).Address), f
	}, "GB", money.USDC)
}

// layer is a money layer with Circle registered and a ledger that keeps what webhooks post.
type layer struct {
	reg     *money.Registry
	router  chi.Router
	ledger  *ledger
	fake    *circlefake.Fake
	wallets money.Wallets
	trans   money.Transfers
}

type cleared struct{}

func (cleared) CapabilityCleared(context.Context, string) (bool, error) { return true, nil }

type ledger struct {
	mu      sync.Mutex
	credits []money.Event
}

func (l *ledger) PostCredit(_ context.Context, _ string, e money.Event) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.credits = append(l.credits, e)
	return "ws", nil
}

type replays struct{ taken sync.Map }

func (r *replays) Taken(_ context.Context, partner, id string) (bool, error) {
	_, ok := r.taken.Load(partner + "/" + id)
	return ok, nil
}

func (r *replays) Take(_ context.Context, partner, id string) error {
	r.taken.Store(partner+"/"+id, true)
	return nil
}

func newLayer(t *testing.T, approved ...string) *layer {
	t.Helper()
	f := circlefake.New(t)
	l := &ledger{}
	on := money.SwitchSet{{Capability: money.CapWallets, Country: "GB", Partner: circle.Name}: true,
		{Capability: money.CapTransfers, Country: "GB", Partner: circle.Name}: true}
	reg, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(partners.NewRegistry(nil), ""), Switches: on, Clearances: cleared{},
		Limits: moneytest.Allow{}, Audit: &moneytest.Trail{}, Ledger: l, Replays: &replays{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(newCircle(t, f, approved...)); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Post("/v1/money/webhooks/{partner}", reg.ServeWebhook)
	rt := money.Route{Workspace: "ws", Country: "GB", Capability: "payments_out"}
	if p, err := reg.Partner(ctx, money.CapTransfers, rt); err != nil || p != circle.Name {
		t.Fatalf("transfers are served by %q, %v; want circle", p, err)
	}
	w, _ := reg.Wallets(ctx, rt)
	tr, _ := reg.Transfers(ctx, rt)
	return &layer{reg: reg, router: r, ledger: l, fake: f, wallets: w, trans: tr}
}

func (l *layer) wallet(t *testing.T, key string, fund int64) money.Wallet {
	t.Helper()
	w, err := l.wallets.Create(ctx, money.CreateWallet{Key: key, AgentID: "agent-" + key, Asset: money.USDC})
	if err != nil {
		t.Fatal(err)
	}
	if fund > 0 {
		l.fake.Deposit(w.Ref, "in-"+key, money.Amount{Minor: fund, Asset: money.USDC})
	}
	return w
}

func (l *layer) post(body []byte, h http.Header) int {
	req := httptest.NewRequest(http.MethodPost, "/v1/money/webhooks/circle", bytes.NewReader(body))
	for k, v := range h {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	l.router.ServeHTTP(rec, req)
	return rec.Code
}

// B46.5 — Circle's signed notification of a USDC deposit posts its money once: the same notification again is refused
// and posts nothing.
func TestADepositNotificationCreditsOnce(t *testing.T) {
	l := newLayer(t)
	w := l.wallet(t, "dep", 25_500_000)
	body, h := l.fake.Notify("b1f0c3a2-0000-4000-8000-0000000notif", "transactions.inbound", "in-dep")
	if got := l.post(body, h); got != http.StatusOK {
		t.Fatalf("a signed deposit = %d, want 200", got)
	}
	if got := l.post(body, h); got != http.StatusConflict {
		t.Fatalf("the same notification again = %d, want 409", got)
	}
	want := money.Event{ID: "b1f0c3a2-0000-4000-8000-0000000notif", To: w.Ref, Amount: money.Amount{Minor: 25_500_000, Asset: money.USDC},
		PartnerRef: "in-dep", Payer: "0x00000000000000000000000000000000000a11ce"}
	if len(l.ledger.credits) != 1 || l.ledger.credits[0] != want {
		t.Fatalf("posted %+v, want the one credit %+v", l.ledger.credits, want)
	}
}

// B46.5 — a notification without Circle's signature, signed by another key, or changed after signing posts nothing.
func TestAnUnsignedNotificationIsRefused(t *testing.T) {
	l := newLayer(t)
	l.wallet(t, "uns", 1_000_000)
	body, h := l.fake.Notify("c2e1d4b3-0000-4000-8000-0000000notif", "transactions.inbound", "in-uns")
	other := circlefake.New(t)
	for _, tc := range []struct {
		name string
		body []byte
		h    http.Header
	}{
		{"no signature", body, http.Header{}},
		{"a key id but no signature", body, http.Header{"X-Circle-Key-Id": h["X-Circle-Key-Id"]}},
		{"signed by another key", body, other.Sign(body)},
		{"changed after signing", bytes.Replace(body, []byte(`"1"`), []byte(`"1000"`), 1), h},
	} {
		if got := l.post(tc.body, tc.h); got != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", tc.name, got)
		}
	}
	if len(l.ledger.credits) != 0 {
		t.Fatalf("posted %+v from notifications that do not verify", l.ledger.credits)
	}
}

// B46.5 — a genuine notification that brings no USDC in — a token that calls itself USDC at another address, or money
// going out — is answered and posts nothing.
func TestANotificationThatIsNotAUSDCDepositPostsNothing(t *testing.T) {
	l := newLayer(t, moneytest.PayeeFor(money.USDC).Address)
	w := l.wallet(t, "spoof", 0)
	l.fake.DepositSpoof(w.Ref, "in-spoof", 9_000_000)
	body, h := l.fake.Notify("d3f2e5c4-0000-4000-8000-0000000notif", "transactions.inbound", "in-spoof")
	if got := l.post(body, h); got != http.StatusOK {
		t.Fatalf("a spoof token's deposit = %d, want 200", got)
	}
	if b, err := l.wallets.Balance(ctx, w.Ref); err != nil || b.Minor != 0 {
		t.Fatalf("balance = %+v, %v; a spoof token is not USDC", b, err)
	}
	if _, err := l.trans.Credit(ctx, money.Credit{Key: "c-spoof", To: w.Ref, Amount: money.Amount{Minor: 9_000_000, Asset: money.USDC}, PartnerRef: "in-spoof"}); !errors.Is(err, money.ErrRefused) {
		t.Fatalf("crediting a spoof token's deposit: err = %v, want ErrRefused", err)
	}
	paid := l.wallet(t, "paid", 2_000_000)
	out, err := l.trans.Outbound(ctx, money.Outbound{Key: "out-paid", From: paid.Ref, Payee: moneytest.PayeeFor(money.USDC), Amount: money.Amount{Minor: 1_000_000, Asset: money.USDC}})
	if err != nil {
		t.Fatal(err)
	}
	l.fake.Complete(out.Ref)
	body, h = l.fake.Notify("e4a3f6d5-0000-4000-8000-0000000notif", "transactions.outbound", out.Ref)
	if got := l.post(body, h); got != http.StatusOK {
		t.Fatalf("a transfer out's notification = %d, want 200", got)
	}
	if _, err := l.trans.Credit(ctx, money.Credit{Key: "c-out", To: paid.Ref, Amount: money.Amount{Minor: 1_000_000, Asset: money.USDC}, PartnerRef: out.Ref}); !errors.Is(err, money.ErrRefused) {
		t.Fatalf("crediting a transfer out: err = %v, want ErrRefused", err)
	}
	if len(l.ledger.credits) != 0 {
		t.Fatalf("posted %+v", l.ledger.credits)
	}
}

// B46.5 — money out goes only to an address the owner approved: any other is refused before Circle is asked, and
// nothing leaves.
func TestOutboundToANonAllowlistedAddressIsRefused(t *testing.T) {
	l := newLayer(t, moneytest.PayeeFor(money.USDC).Address)
	w := l.wallet(t, "al", 5_000_000)
	stranger := money.Payee{Name: "Mallory", Network: "base", Address: "0x00000000000000000000000000000000000bad00"}
	_, err := l.trans.Outbound(ctx, money.Outbound{Key: "out-al", From: w.Ref, Payee: stranger, Amount: money.Amount{Minor: 1_000_000, Asset: money.USDC}})
	if !errors.Is(err, money.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if n := l.fake.Outbound(); n != 0 {
		t.Fatalf("Circle was asked for %d transfers", n)
	}
	if len(l.fake.Screened) != 0 {
		t.Fatalf("an address not on the allowlist was screened: %q", l.fake.Screened)
	}
	if b, _ := l.wallets.Balance(ctx, w.Ref); b.Minor != 5_000_000 {
		t.Fatalf("balance = %d after a refused transfer", b.Minor)
	}
}

// B46.5 — an approved address is screened before money goes to it: one screening denies, or that carries a sanctions
// signal whatever the rule decided, is refused and nothing leaves; one it clears is paid.
func TestOutboundIsScreenedFirst(t *testing.T) {
	denied, flagged := "0x00000000000000000000000000000000000de41e", "0x00000000000000000000000000000000005a1c7e"
	l := newLayer(t, denied, flagged, moneytest.PayeeFor(money.USDC).Address)
	l.fake.Screening(denied, "DENIED")                 // a console rule, such as Russia- or Belarus-linked addresses
	l.fake.Screening(flagged, "APPROVED", "SANCTIONS") // a sanctions signal the rule let through
	w := l.wallet(t, "scr", 5_000_000)
	for _, addr := range []string{denied, flagged} {
		_, err := l.trans.Outbound(ctx, money.Outbound{Key: "out-" + addr, From: w.Ref, Payee: money.Payee{Name: "Listed", Network: "base", Address: addr},
			Amount: money.Amount{Minor: 1_000_000, Asset: money.USDC}})
		if !errors.Is(err, money.ErrRefused) {
			t.Fatalf("%s: err = %v, want ErrRefused", addr, err)
		}
	}
	if n := l.fake.Outbound(); n != 0 {
		t.Fatalf("Circle was asked for %d transfers to addresses screening refused", n)
	}
	if _, err := l.trans.Outbound(ctx, money.Outbound{Key: "out-ok", From: w.Ref, Payee: moneytest.PayeeFor(money.USDC),
		Amount: money.Amount{Minor: 1_000_000, Asset: money.USDC}}); err != nil {
		t.Fatal(err)
	}
	if got := l.fake.Screened; len(got) != 3 || !strings.EqualFold(got[2], moneytest.PayeeFor(money.USDC).Address) {
		t.Fatalf("screened %q, want all three addresses", got)
	}
}

// B46.5 — a transfer asked again after its money left answers the transfer it made, not insufficient funds, pays
// once, and is screened again; the same key for another transfer is refused.
func TestARetriedTransferAfterTheMoneyLeftFindsIt(t *testing.T) {
	l := newLayer(t, moneytest.PayeeFor(money.USDC).Address)
	w := l.wallet(t, "re", 3_000_000)
	req := money.Outbound{Key: "out-re", From: w.Ref, Payee: moneytest.PayeeFor(money.USDC), Amount: money.Amount{Minor: 3_000_000, Asset: money.USDC}}
	first, err := l.trans.Outbound(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := l.trans.Outbound(ctx, req)
	if err != nil || again.Ref != first.Ref {
		t.Fatalf("the retry = %+v, %v; want %+v", again, err, first)
	}
	if n := l.fake.Outbound(); n != 1 {
		t.Fatalf("Circle made %d transfers, want 1", n)
	}
	if n := len(l.fake.Screened); n != 2 {
		t.Fatalf("the payee was screened %d times, want once per attempt", n)
	}
	req.Amount.Minor = 2_000_000
	if _, err := l.trans.Outbound(ctx, req); !errors.Is(err, money.ErrKeyReused) {
		t.Fatalf("the same key for another amount: err = %v, want ErrKeyReused", err)
	}
}

// B46.5 — Circle is not offered without its keys, an entity secret, a wallet set and an allowlist.
func TestCircleNeedsItsConfig(t *testing.T) {
	f := circlefake.New(t)
	full := circle.Config{BaseURL: f.URL, APIKey: f.APIKey, EntitySecret: f.EntitySecret, WalletSetID: f.WalletSetID, Blockchain: circlefake.Blockchain, Allowlist: allowlist{}}
	for name, cut := range map[string]func(*circle.Config){
		"no API key":            func(c *circle.Config) { c.APIKey = "" },
		"no entity secret":      func(c *circle.Config) { c.EntitySecret = "" },
		"a short entity secret": func(c *circle.Config) { c.EntitySecret = "abcd" },
		"no wallet set":         func(c *circle.Config) { c.WalletSetID = "" },
		"no allowlist":          func(c *circle.Config) { c.Allowlist = nil },
		"another chain":         func(c *circle.Config) { c.Blockchain = "ETH" },
	} {
		c := full
		cut(&c)
		if _, err := circle.New(c); err == nil {
			t.Errorf("%s: New answered no error", name)
		}
	}
}
