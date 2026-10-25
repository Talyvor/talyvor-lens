package coinbase_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/coinbase"
	"github.com/talyvor/lens/internal/money/coinbase/cdpfake"
	"github.com/talyvor/lens/internal/money/moneytest"
	"github.com/talyvor/lens/internal/partners"
)

var ctx = context.Background()

// allowlist is the addresses the owner approved, for any wallet.
type allowlist map[string]bool

func (a allowlist) Approved(_ context.Context, _, network, address string) (bool, error) {
	return network == "base" && a[strings.ToLower(address)], nil
}

// screener refuses the addresses in refused and keeps every address it was asked about.
type screener struct {
	mu       sync.Mutex
	refused  map[string]bool
	screened []string
}

func (s *screener) Screen(_ context.Context, _, address string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.screened = append(s.screened, strings.ToLower(address))
	if s.refused[strings.ToLower(address)] {
		return fmt.Errorf("%w: %s is sanctioned", money.ErrRefused, address)
	}
	return nil
}

func (s *screener) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.screened)
}

func config(f *cdpfake.Fake, al allowlist, s coinbase.Screener) coinbase.Config {
	return coinbase.Config{BaseURL: f.URL, APIKeyID: f.APIKeyID, APIKeySecret: f.APIKeySecret, WalletSecret: f.WalletSecret,
		Network: cdpfake.Network, RPCURL: f.RPCURL, WebhookSecret: f.WebhookSecret, Allowlist: al, Screener: s}
}

func newCoinbase(t *testing.T, f *cdpfake.Fake, s coinbase.Screener, approved ...string) *money.Adapter {
	t.Helper()
	al := allowlist{}
	for _, a := range approved {
		al[strings.ToLower(a)] = true
	}
	a, err := coinbase.New(config(f, al, s))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// B46.6 — Coinbase's adapter, against a fake of CDP's API, passes the contract every adapter passes: wallets and
// transfers in USDC; the capabilities it does not offer are skipped.
func TestCoinbasePassesTheContract(t *testing.T) {
	moneytest.ContractWithDeposits(t, func() (*money.Adapter, moneytest.Depositor) {
		f := cdpfake.New(t)
		return newCoinbase(t, f, &screener{}, moneytest.PayeeFor(money.USDC).Address), f
	}, "GB", money.USDC)
}

// layer is a money layer with Coinbase registered and a ledger that keeps what webhooks post.
type layer struct {
	router  chi.Router
	ledger  *ledger
	fake    *cdpfake.Fake
	screen  *screener
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

func newLayer(t *testing.T, refused []string, approved ...string) *layer {
	t.Helper()
	f := cdpfake.New(t)
	s := &screener{refused: map[string]bool{}}
	for _, a := range refused {
		s.refused[strings.ToLower(a)] = true
	}
	l := &ledger{}
	on := money.SwitchSet{{Capability: money.CapWallets, Country: "GB", Partner: coinbase.Name}: true,
		{Capability: money.CapTransfers, Country: "GB", Partner: coinbase.Name}: true}
	reg, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(partners.NewRegistry(nil), ""), Switches: on, Clearances: cleared{},
		Limits: moneytest.Allow{}, Audit: &moneytest.Trail{}, Ledger: l, Replays: &replays{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(newCoinbase(t, f, s, approved...)); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Post("/v1/money/webhooks/{partner}", reg.ServeWebhook)
	rt := money.Route{Workspace: "ws", Country: "GB", Capability: "payments_out"}
	if p, err := reg.Partner(ctx, money.CapTransfers, rt); err != nil || p != coinbase.Name {
		t.Fatalf("transfers are served by %q, %v; want coinbase", p, err)
	}
	w, _ := reg.Wallets(ctx, rt)
	tr, _ := reg.Transfers(ctx, rt)
	return &layer{router: r, ledger: l, fake: f, screen: s, wallets: w, trans: tr}
}

func (l *layer) wallet(t *testing.T, key string, fund int64) money.Wallet {
	t.Helper()
	w, err := l.wallets.Create(ctx, money.CreateWallet{Key: key, AgentID: "agent-" + key, Asset: money.USDC})
	if err != nil {
		t.Fatal(err)
	}
	if fund > 0 {
		l.fake.Deposit(w.Ref, "0xin-"+key, money.Amount{Minor: fund, Asset: money.USDC})
	}
	return w
}

func (l *layer) post(body []byte, h http.Header) int {
	req := httptest.NewRequest(http.MethodPost, "/v1/money/webhooks/coinbase", bytes.NewReader(body))
	for k, v := range h {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	l.router.ServeHTTP(rec, req)
	return rec.Code
}

// B46.6 — CDP's signed notification of a USDC deposit posts its money once: the same notification again is refused
// and posts nothing.
func TestADepositNotificationCreditsOnce(t *testing.T) {
	l := newLayer(t, nil)
	w := l.wallet(t, "dep", 25_500_000)
	body, h := l.fake.Notify("evt_dep", "0xin-dep")
	if got := l.post(body, h); got != http.StatusOK {
		t.Fatalf("a signed deposit = %d, want 200", got)
	}
	if got := l.post(body, h); got != http.StatusConflict {
		t.Fatalf("the same notification again = %d, want 409", got)
	}
	want := money.Event{ID: "evt_dep", To: w.Ref, Amount: money.Amount{Minor: 25_500_000, Asset: money.USDC},
		PartnerRef: "0xin-dep:0", Payer: cdpfake.Payer}
	if len(l.ledger.credits) != 1 || l.ledger.credits[0] != want {
		t.Fatalf("posted %+v, want the one credit %+v", l.ledger.credits, want)
	}
	if _, err := l.trans.Credit(ctx, money.Credit{Key: "c-dep", To: w.Ref, Amount: want.Amount, PartnerRef: want.PartnerRef}); err != nil {
		t.Fatalf("the notification's partner ref does not credit: %v", err)
	}
	if _, err := l.trans.Credit(ctx, money.Credit{Key: "c-dep2", To: w.Ref, Amount: money.Amount{Minor: 26_000_000, Asset: money.USDC}, PartnerRef: want.PartnerRef}); !errors.Is(err, money.ErrRefused) {
		t.Fatalf("crediting more than the deposit brought: err = %v, want ErrRefused", err)
	}
}

// B46.6 — a notification without CDP's signature, signed with another secret, changed after signing, or signed too
// long ago posts nothing.
func TestAnUnsignedNotificationIsRefused(t *testing.T) {
	l := newLayer(t, nil)
	l.wallet(t, "uns", 1_000_000)
	body, h := l.fake.Notify("evt_uns", "0xin-uns")
	for _, tc := range []struct {
		name string
		body []byte
		h    http.Header
	}{
		{"no signature", body, http.Header{}},
		{"signed with another secret", body, cdpfake.New(t).Sign(body)},
		{"changed after signing", bytes.Replace(body, []byte(`"1000000"`), []byte(`"1000000000"`), 1), h},
		{"signed ten minutes ago", body, l.fake.SignAt(body, time.Now().Add(-10*time.Minute))},
	} {
		if got := l.post(tc.body, tc.h); got != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", tc.name, got)
		}
	}
	if len(l.ledger.credits) != 0 {
		t.Fatalf("posted %+v from notifications that do not verify", l.ledger.credits)
	}
}

// B46.6 — a genuine notification that brings no USDC in — a token that calls itself USDC at another address, or money
// going out — is answered and posts nothing.
func TestANotificationThatIsNotAUSDCDepositPostsNothing(t *testing.T) {
	l := newLayer(t, nil, moneytest.PayeeFor(money.USDC).Address)
	w := l.wallet(t, "spoof", 0)
	l.fake.DepositSpoof(w.Ref, "0xin-spoof", 9_000_000)
	body, h := l.fake.Notify("evt_spoof", "0xin-spoof")
	if got := l.post(body, h); got != http.StatusOK {
		t.Fatalf("a spoof token's deposit = %d, want 200", got)
	}
	if b, err := l.wallets.Balance(ctx, w.Ref); err != nil || b.Minor != 0 {
		t.Fatalf("balance = %+v, %v; a spoof token is not USDC", b, err)
	}
	if _, err := l.trans.Credit(ctx, money.Credit{Key: "c-spoof", To: w.Ref, Amount: money.Amount{Minor: 9_000_000, Asset: money.USDC}, PartnerRef: "0xin-spoof"}); !errors.Is(err, money.ErrRefused) {
		t.Fatalf("crediting a spoof token's deposit: err = %v, want ErrRefused", err)
	}
	paid := l.wallet(t, "paid", 2_000_000)
	out, err := l.trans.Outbound(ctx, money.Outbound{Key: "out-paid", From: paid.Ref, Payee: moneytest.PayeeFor(money.USDC), Amount: money.Amount{Minor: 1_000_000, Asset: money.USDC}})
	if err != nil {
		t.Fatal(err)
	}
	body, h = l.fake.Notify("evt_out", out.Ref)
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

// B46.6 — money out goes only to an address the owner approved: any other is refused before CDP is asked, and
// nothing leaves.
func TestOutboundToANonAllowlistedAddressIsRefused(t *testing.T) {
	l := newLayer(t, nil, moneytest.PayeeFor(money.USDC).Address)
	w := l.wallet(t, "al", 5_000_000)
	stranger := money.Payee{Name: "Mallory", Network: "base", Address: "0x00000000000000000000000000000000000bad00"}
	_, err := l.trans.Outbound(ctx, money.Outbound{Key: "out-al", From: w.Ref, Payee: stranger, Amount: money.Amount{Minor: 1_000_000, Asset: money.USDC}})
	if !errors.Is(err, money.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if n := l.fake.Sent(); n != 0 {
		t.Fatalf("CDP was asked for %d transfers", n)
	}
	if n := l.screen.count(); n != 0 {
		t.Fatalf("an address not on the allowlist was screened %d times", n)
	}
	if b, _ := l.wallets.Balance(ctx, w.Ref); b.Minor != 5_000_000 {
		t.Fatalf("balance = %d after a refused transfer", b.Minor)
	}
}

// B46.6 — an approved address is screened before money goes to it: one screening refuses is not paid and nothing
// leaves; one it clears is paid.
func TestOutboundIsScreenedFirst(t *testing.T) {
	sanctioned := "0x00000000000000000000000000000000005a1c7e"
	l := newLayer(t, []string{sanctioned}, sanctioned, moneytest.PayeeFor(money.USDC).Address)
	w := l.wallet(t, "scr", 5_000_000)
	_, err := l.trans.Outbound(ctx, money.Outbound{Key: "out-sanctioned", From: w.Ref, Payee: money.Payee{Name: "Listed", Network: "base", Address: sanctioned},
		Amount: money.Amount{Minor: 1_000_000, Asset: money.USDC}})
	if !errors.Is(err, money.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if n := l.fake.Sent(); n != 0 {
		t.Fatalf("CDP was asked for %d transfers to an address screening refused", n)
	}
	if _, err := l.trans.Outbound(ctx, money.Outbound{Key: "out-ok", From: w.Ref, Payee: moneytest.PayeeFor(money.USDC),
		Amount: money.Amount{Minor: 1_000_000, Asset: money.USDC}}); err != nil {
		t.Fatal(err)
	}
	if n := l.screen.count(); n != 2 || l.fake.Sent() != 1 {
		t.Fatalf("screened %d addresses and sent %d, want 2 and 1", n, l.fake.Sent())
	}
}

// B46.6 — a transfer asked again after its money left answers the transfer it made, not insufficient funds, pays
// once, and is screened again; the same key for another transfer is refused.
func TestARetriedTransferAfterTheMoneyLeftFindsIt(t *testing.T) {
	l := newLayer(t, nil, moneytest.PayeeFor(money.USDC).Address)
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
	if n := l.fake.Sent(); n != 1 {
		t.Fatalf("CDP made %d transfers, want 1", n)
	}
	if n := l.screen.count(); n != 2 {
		t.Fatalf("the payee was screened %d times, want once per attempt", n)
	}
	req.Amount.Minor = 2_000_000
	if _, err := l.trans.Outbound(ctx, req); !errors.Is(err, money.ErrKeyReused) {
		t.Fatalf("the same key for another amount: err = %v, want ErrKeyReused", err)
	}
}

// B46.6 — the same key for another agent's wallet is refused, never the first agent's wallet.
func TestTheSameKeyForAnotherAgentIsRefused(t *testing.T) {
	l := newLayer(t, nil)
	l.wallet(t, "k", 0)
	if _, err := l.wallets.Create(ctx, money.CreateWallet{Key: "k", AgentID: "someone-else", Asset: money.USDC}); !errors.Is(err, money.ErrKeyReused) {
		t.Fatalf("err = %v, want ErrKeyReused", err)
	}
}

// B46.6 — Coinbase is not offered without its keys, a node, a webhook secret, an allowlist and a screener, or on a
// chain other than Base.
func TestCoinbaseNeedsItsConfig(t *testing.T) {
	f := cdpfake.New(t)
	full := config(f, allowlist{}, &screener{})
	for name, cut := range map[string]func(*coinbase.Config){
		"no API key":               func(c *coinbase.Config) { c.APIKeySecret = "" },
		"a short API key":          func(c *coinbase.Config) { c.APIKeySecret = "abcd" },
		"no wallet secret":         func(c *coinbase.Config) { c.WalletSecret = "" },
		"no node":                  func(c *coinbase.Config) { c.RPCURL = "" },
		"no webhook secret":        func(c *coinbase.Config) { c.WebhookSecret = "" },
		"no allowlist":             func(c *coinbase.Config) { c.Allowlist = nil },
		"no screener":              func(c *coinbase.Config) { c.Screener = nil },
		"another chain":            func(c *coinbase.Config) { c.Network = "ethereum" },
		"Base without a paymaster": func(c *coinbase.Config) { c.Network = "base" },
	} {
		c := full
		cut(&c)
		if _, err := coinbase.New(c); err == nil {
			t.Errorf("%s: New answered no error", name)
		}
	}
}
