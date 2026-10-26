package swan_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/moneytest"
	"github.com/talyvor/lens/internal/money/swan"
	"github.com/talyvor/lens/internal/money/swan/swanfake"
	"github.com/talyvor/lens/internal/partners"
)

var ctx = context.Background()

// holder answers one Swan account holder for every workspace and agent.
type holder string

func (h holder) AccountHolder(context.Context, string) (string, error) { return string(h), nil }

func config(f *swanfake.Fake, h swan.Holders) swan.Config {
	return swan.Config{APIURL: f.URL, TokenURL: f.TokenURL, ClientID: f.ClientID, ClientSecret: f.ClientSecret, ProjectID: f.ProjectID,
		ConsentKey: f.ConsentKey, ConsentRedirectURL: "https://app.talyvor.test/consent", WebhookSecret: f.WebhookSecret, Instant: true, Holders: h}
}

func newSwan(t *testing.T, f *swanfake.Fake) *money.Adapter {
	t.Helper()
	a, err := swan.New(config(f, holder(f.HolderID)))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// B46.7 — Swan's adapter, against a fake of Swan's API, passes the contract every adapter passes: accounts, wallets,
// transfers and verification in euros; cards and payouts, which it does not offer, are skipped.
func TestSwanPassesTheContract(t *testing.T) {
	moneytest.ContractWithDeposits(t, func() (*money.Adapter, moneytest.Depositor) {
		f := swanfake.New(t)
		return newSwan(t, f), f
	}, "FR", money.EUR)
}

type cleared struct{}

func (cleared) CapabilityCleared(context.Context, string) (bool, error) { return true, nil }

type ledger struct {
	mu        sync.Mutex
	credits   []money.Event
	transfers []money.Event
}

func (l *ledger) PostCredit(_ context.Context, _ string, e money.Event) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.credits = append(l.credits, e)
	return "ws", nil
}

func (l *ledger) SettleTransfer(_ context.Context, _ string, e money.Event) (string, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.transfers = append(l.transfers, e)
	return "ws", "mpay_" + e.PartnerRef, nil
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

// layer is a money layer with Swan registered and switched on in France, and a ledger that keeps what webhooks post.
type layer struct {
	router  chi.Router
	ledger  *ledger
	fake    *swanfake.Fake
	wallets money.Wallets
	trans   money.Transfers
	kyc     money.KYC
}

func newLayer(t *testing.T) *layer {
	t.Helper()
	f := swanfake.New(t)
	l := &ledger{}
	on := money.SwitchSet{}
	for _, c := range []money.Capability{money.CapAccounts, money.CapWallets, money.CapTransfers, money.CapKYC} {
		on[money.Switch{Capability: c, Country: "FR", Partner: swan.Name}] = true
	}
	reg, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(partners.NewRegistry(nil), ""), Switches: on, Clearances: cleared{},
		Limits: moneytest.Allow{}, Audit: &moneytest.Trail{}, Ledger: l, Replays: &replays{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(newSwan(t, f)); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Post("/v1/money/webhooks/{partner}", reg.ServeWebhook)
	rt := money.Route{Workspace: "ws", Country: "FR", Capability: "payments_out"}
	if p, err := reg.Partner(ctx, money.CapTransfers, rt); err != nil || p != swan.Name {
		t.Fatalf("transfers are served by %q, %v; want swan", p, err)
	}
	w, _ := reg.Wallets(ctx, rt)
	tr, _ := reg.Transfers(ctx, rt)
	k, _ := reg.KYC(ctx, rt)
	return &layer{router: r, ledger: l, fake: f, wallets: w, trans: tr, kyc: k}
}

func (l *layer) wallet(t *testing.T, key string, fund int64) money.Wallet {
	t.Helper()
	w, err := l.wallets.Create(ctx, money.CreateWallet{Key: key, AgentID: "agent-" + key, Asset: money.EUR})
	if err != nil {
		t.Fatal(err)
	}
	if fund > 0 {
		l.fake.Deposit(w.Ref, "tx-in-"+key, money.Amount{Minor: fund, Asset: money.EUR})
	}
	return w
}

func (l *layer) post(body []byte, h http.Header) int {
	req := httptest.NewRequest(http.MethodPost, "/v1/money/webhooks/swan", bytes.NewReader(body))
	for k, v := range h {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	l.router.ServeHTTP(rec, req)
	return rec.Code
}

func (l *layer) balance(t *testing.T, ref string) int64 {
	t.Helper()
	b, err := l.wallets.Balance(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	return b.Minor
}

// B46.7 — Swan's webhook that an incoming SEPA credit was booked posts its money once, read from Swan's API: the same
// notification again is refused and posts nothing.
func TestAnIncomingSEPACreditPostsOnce(t *testing.T) {
	l := newLayer(t)
	w := l.wallet(t, "dep", 250_00)
	body, h := l.fake.Notify("evt-dep", "tx-in-dep")
	if got := l.post(body, h); got != http.StatusOK {
		t.Fatalf("a booked credit = %d, want 200", got)
	}
	if got := l.post(body, h); got != http.StatusConflict {
		t.Fatalf("the same notification again = %d, want 409", got)
	}
	want := money.Event{ID: "evt-dep", To: w.Ref, Amount: money.Amount{Minor: 250_00, Asset: money.EUR}, PartnerRef: "tx-in-dep", Payer: swanfake.Payer}
	if len(l.ledger.credits) != 1 || l.ledger.credits[0] != want {
		t.Fatalf("posted %+v, want the one credit %+v", l.ledger.credits, want)
	}
	if _, err := l.trans.Credit(ctx, money.Credit{Key: "c-dep", To: w.Ref, Amount: want.Amount, PartnerRef: want.PartnerRef}); err != nil {
		t.Fatalf("the notification's transaction does not credit: %v", err)
	}
	if _, err := l.trans.Credit(ctx, money.Credit{Key: "c-dep2", To: w.Ref, Amount: money.Amount{Minor: 251_00, Asset: money.EUR}, PartnerRef: want.PartnerRef}); !errors.Is(err, money.ErrRefused) {
		t.Fatalf("crediting more than the transfer brought: err = %v, want ErrRefused", err)
	}
}

// B46.7 — a webhook without Swan's secret, with another secret, or for another project posts nothing.
func TestAnUnsignedWebhookIsRefused(t *testing.T) {
	l := newLayer(t)
	l.wallet(t, "uns", 10_00)
	body, h := l.fake.Notify("evt-uns", "tx-in-uns")
	other := swanfake.New(t)
	otherBody, otherH := other.Notify("evt-uns", "tx-in-uns")
	for name, tc := range map[string]struct {
		body []byte
		h    http.Header
	}{
		"no secret":           {body, http.Header{}},
		"another secret":      {body, otherH},
		"another project":     {otherBody, h},
		"the secret, garbled": {body, http.Header{"X-Swan-Secret": {h.Get("X-Swan-Secret") + "x"}}},
	} {
		if got := l.post(tc.body, tc.h); got != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, got)
		}
	}
	if len(l.ledger.credits) != 0 {
		t.Fatalf("posted %+v from webhooks that do not verify", l.ledger.credits)
	}
}

// B46.7 — a genuine webhook that brings no money in from outside — money moved between two of the holder's own
// accounts, or a transaction that is not a booking — is answered and posts nothing; paying the holder's own account
// out is refused.
func TestAWebhookThatIsNotAnIncomingCreditPostsNothing(t *testing.T) {
	l := newLayer(t)
	a, b := l.wallet(t, "own-a", 20_00), l.wallet(t, "own-b", 0)
	moved, err := l.trans.Internal(ctx, money.Internal{Key: "move", From: a.Ref, To: b.Ref, Amount: money.Amount{Minor: 5_00, Asset: money.EUR}})
	if err != nil {
		t.Fatal(err)
	}
	body, h := l.fake.Notify("evt-moved", l.fake.CreditOf(moved.Ref))
	if got := l.post(body, h); got != http.StatusOK {
		t.Fatalf("a transfer between two of ours = %d, want 200", got)
	}
	l.wallet(t, "in", 7_00)
	body, h = l.fake.Notify("evt-in", "tx-in-in")
	if got := l.post(bytes.Replace(body, []byte("Transaction.Booked"), []byte("Transaction.Pending"), 1), h); got != http.StatusOK {
		t.Fatalf("a Transaction.Pending event = %d, want 200", got)
	}
	unbooked := l.wallet(t, "unbooked", 3_00)
	l.fake.Pend("tx-in-unbooked")
	body, h = l.fake.Notify("evt-unbooked", "tx-in-unbooked")
	if got := l.post(body, h); got != http.StatusOK {
		t.Fatalf("a credit not yet booked = %d, want 200", got)
	}
	if _, err := l.trans.Credit(ctx, money.Credit{Key: "c-unbooked", To: unbooked.Ref, Amount: money.Amount{Minor: 3_00, Asset: money.EUR},
		PartnerRef: "tx-in-unbooked"}); !errors.Is(err, money.ErrRefused) {
		t.Fatalf("crediting a credit not yet booked: err = %v, want ErrRefused", err)
	}
	if len(l.ledger.credits) != 0 {
		t.Fatalf("posted %+v", l.ledger.credits)
	}
	d, err := l.wallets.Receive(ctx, b.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.trans.Outbound(ctx, money.Outbound{Key: "out-own", From: a.Ref, Payee: money.Payee{Name: "Own", IBAN: d.IBAN},
		Amount: money.Amount{Minor: 1_00, Asset: money.EUR}}); !errors.Is(err, money.ErrInvalid) {
		t.Fatalf("paying one of ours out: err = %v, want ErrInvalid", err)
	}
}

// B46.7 — a SEPA payment out is consented to with the server-consent key and made once however often it is asked; a
// payee whose IBAN does not verify is refused before Swan is asked.
func TestASEPAPaymentOutIsMadeOnce(t *testing.T) {
	l := newLayer(t)
	w := l.wallet(t, "out", 100_00)
	req := money.Outbound{Key: "out-1", From: w.Ref, Payee: moneytest.PayeeFor(money.EUR), Amount: money.Amount{Minor: 40_00, Asset: money.EUR}, Reference: "invoice 1"}
	first, err := l.trans.Outbound(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := l.trans.Outbound(ctx, req)
	if err != nil || again != first {
		t.Fatalf("the retry = %+v, %v; want %+v", again, err, first)
	}
	if n := l.fake.Sent(); n != 1 {
		t.Fatalf("Swan made %d payments, want 1", n)
	}
	if got := l.balance(t, w.Ref); got != 60_00 {
		t.Fatalf("balance = %d, want 6000", got)
	}
	bad := req
	bad.Key, bad.Payee.IBAN = "out-bad", "DE89370400440532013001"
	if _, err := l.trans.Outbound(ctx, bad); !errors.Is(err, money.ErrInvalid) || l.fake.Sent() != 1 {
		t.Fatalf("a payee whose IBAN does not verify: err = %v, %d payments; want ErrInvalid and 1", err, l.fake.Sent())
	}
}

// B46.7 — a company is handed to Swan's hosted onboarding, and its verification completes with the account holder its
// accounts open under; an agent's account past Swan's account limit is refused.
func TestACompanyIsVerifiedThroughSwansOnboarding(t *testing.T) {
	l := newLayer(t)
	v, err := l.kyc.Start(ctx, money.StartKYC{Key: "kyb", Subject: money.KYCCompany, Name: "Ada Ltd", Country: "FR"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != money.StatusPending || v.Link == "" {
		t.Fatalf("started = %+v, want pending with a link", v)
	}
	l.fake.Finish(v.Ref)
	done, err := l.kyc.Status(ctx, v.Ref)
	if err != nil || done.Status != money.StatusCompleted || done.Detail == "" {
		t.Fatalf("status = %+v, %v; want completed with the account holder", done, err)
	}
	a, err := swan.New(config(l.fake, holder(done.Detail)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.KYC.Start(ctx, money.StartKYC{Key: "kyc-us", Subject: money.KYCPerson, Name: "Ada", Country: "US"}); !errors.Is(err, money.ErrInvalid) {
		t.Fatalf("a country Swan has no accounts in: err = %v, want ErrInvalid", err)
	}
	l.fake.AccountLimit = 1
	theirs, err := a.Wallets.Create(ctx, money.CreateWallet{Key: "w1", AgentID: "agent-1", Asset: money.EUR})
	if err != nil {
		t.Fatalf("the verified holder's first account: %v", err)
	}
	ours := l.wallet(t, "ours", 10_00)
	if _, err := l.trans.Internal(ctx, money.Internal{Key: "across", From: ours.Ref, To: theirs.Ref, Amount: money.Amount{Minor: 1_00, Asset: money.EUR}}); !errors.Is(err, money.ErrInvalid) {
		t.Fatalf("an internal transfer to another holder's account: err = %v, want ErrInvalid", err)
	}
	if _, err := a.Wallets.Create(ctx, money.CreateWallet{Key: "w2", AgentID: "agent-2", Asset: money.EUR}); !errors.Is(err, money.ErrRefused) {
		t.Fatalf("an account past the limit: err = %v, want ErrRefused", err)
	}
}

// B46.7 — Swan is not offered without its credentials, its project, a server-consent key, a webhook secret and the
// account holders.
func TestSwanNeedsItsConfig(t *testing.T) {
	f := swanfake.New(t)
	full := config(f, holder(f.HolderID))
	for name, cut := range map[string]func(*swan.Config){
		"no client secret":    func(c *swan.Config) { c.ClientSecret = "" },
		"no API URL":          func(c *swan.Config) { c.APIURL = "" },
		"no project":          func(c *swan.Config) { c.ProjectID = "" },
		"no consent key":      func(c *swan.Config) { c.ConsentKey = "" },
		"a garbled key":       func(c *swan.Config) { c.ConsentKey = "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n" },
		"no consent redirect": func(c *swan.Config) { c.ConsentRedirectURL = "" },
		"no webhook secret":   func(c *swan.Config) { c.WebhookSecret = "" },
		"no account holders":  func(c *swan.Config) { c.Holders = nil },
	} {
		c := full
		cut(&c)
		if _, err := swan.New(c); err == nil {
			t.Errorf("%s: New answered no error", name)
		}
	}
}
