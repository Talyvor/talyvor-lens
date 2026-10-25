package money_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/moneytest"
	"github.com/talyvor/lens/internal/partners"
)

var ctx = context.Background()

func sandbox() *money.Adapter { return money.NewSandbox(partners.NewRegistry(nil), "whsec_test") }

func registry(t *testing.T, cfg money.Config) *money.Registry {
	t.Helper()
	if cfg.Sandbox == nil {
		cfg.Sandbox = sandbox()
	}
	if cfg.Limits == nil {
		cfg.Limits = moneytest.Allow{}
	}
	if cfg.Audit == nil {
		cfg.Audit = &moneytest.Trail{}
	}
	r, err := money.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// B46.3 — the sandbox, today's test-money partners, passes the contract every adapter passes.
func TestSandboxPassesTheContract(t *testing.T) {
	moneytest.Contract(t, sandbox, "GB", money.GBP, money.EUR, money.USD, money.USDC)
}

type cleared bool

func (c cleared) CapabilityCleared(context.Context, string) (bool, error) { return bool(c), nil }

// B46.3 — a partner serves a capability in a country only while its switch is on there and the wallet capability is
// cleared; everywhere else, and with every switch off as today, the sandbox does.
func TestTheSwitchDecidesWhoServes(t *testing.T) {
	gb := money.Route{Workspace: "ws", Country: "GB", Capability: "payments_out"}
	de := money.Route{Workspace: "ws", Country: "DE", Capability: "payments_out"}
	on := money.SwitchSet{{Capability: money.CapWallets, Country: "GB", Partner: "circle"}: true}
	for _, tc := range []struct {
		name     string
		switches money.Switches
		cleared  cleared
		rt       money.Route
		want     string
	}{
		{"every switch off", nil, true, gb, money.SandboxName},
		{"on, but not cleared for real money", on, false, gb, money.SandboxName},
		{"on and cleared", on, true, gb, "circle"},
		{"on in another country only", on, true, de, money.SandboxName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := registry(t, money.Config{Switches: tc.switches, Clearances: tc.cleared})
			if err := r.Register(&money.Adapter{Name: "circle", Wallets: sandbox().Wallets}); err != nil {
				t.Fatal(err)
			}
			if got, err := r.Partner(ctx, money.CapWallets, tc.rt); err != nil || got != tc.want {
				t.Fatalf("Partner = %q, %v; want %q", got, err, tc.want)
			}
			if got, _ := r.Partner(ctx, money.CapCards, tc.rt); got != money.SandboxName {
				t.Fatalf("cards, which circle does not offer, = %q", got)
			}
		})
	}
}

type limits struct{ err error }

func (l limits) Allow(context.Context, money.Op) error { return l.err }

// spyTransfers counts the calls that reached the adapter.
type spyTransfers struct {
	money.Transfers
	calls int
}

func (s *spyTransfers) Outbound(ctx context.Context, req money.Outbound) (money.Result, error) {
	s.calls++
	return s.Transfers.Outbound(ctx, req)
}

// B46.3 — the owner's limits and one-tap approval run before the adapter is called: a payment they hold or refuse
// never reaches it, and the audit trail records the call either way.
func TestLimitsRunBeforeTheAdapter(t *testing.T) {
	for _, want := range []error{money.ErrLimit, money.ErrApprovalNeeded, errors.New("limits unreadable")} {
		t.Run(want.Error(), func(t *testing.T) {
			sb := sandbox()
			spy := &spyTransfers{Transfers: sb.Transfers}
			sb.Transfers = spy
			trail := &moneytest.Trail{}
			r := registry(t, money.Config{Sandbox: sb, Limits: limits{want}, Audit: trail})
			rt := money.Route{Workspace: "ws", Country: "GB", Capability: "payments_out"}
			w, _ := r.Wallets(ctx, rt)
			wal, err := w.Create(ctx, money.CreateWallet{Key: "w", AgentID: "a", Asset: money.GBP})
			if err != nil {
				t.Fatal(err)
			}
			tr, _ := r.Transfers(ctx, rt)
			_, err = tr.Outbound(ctx, money.Outbound{Key: "o", From: wal.Ref, Payee: moneytest.PayeeFor(money.GBP), Amount: money.Amount{Minor: 5_00, Asset: money.GBP}})
			if !errors.Is(err, money.ErrLimit) && !errors.Is(err, money.ErrApprovalNeeded) {
				t.Fatalf("err = %v, want the limits' refusal", err)
			}
			if !errors.Is(err, want) && !errors.Is(err, money.ErrLimit) {
				t.Fatalf("err = %v, want %v", err, want)
			}
			if spy.calls != 0 {
				t.Fatalf("the adapter was called %d times", spy.calls)
			}
			if got := trail.Kinds; len(got) != 2 || got[1] != "money.transfers.outbound" {
				t.Fatalf("audit trail = %q", got)
			}
		})
	}
}

// B46.3 — the sandbox answers as today's Test partners do: an amount ending in 13 is refused, and nothing leaves.
func TestSandboxRefusesWhatTheTestPartnersRefuse(t *testing.T) {
	r := registry(t, money.Config{})
	rt := money.Route{Workspace: "ws", Country: "GB", Capability: "payments_out"}
	w, _ := r.Wallets(ctx, rt)
	tr, _ := r.Transfers(ctx, rt)
	wal, _ := w.Create(ctx, money.CreateWallet{Key: "w13", AgentID: "a", Asset: money.GBP})
	if _, err := tr.Credit(ctx, money.Credit{Key: "c13", To: wal.Ref, Amount: money.Amount{Minor: 10_00, Asset: money.GBP}, PartnerRef: "p13"}); err != nil {
		t.Fatal(err)
	}
	_, err := tr.Outbound(ctx, money.Outbound{Key: "o13", From: wal.Ref, Payee: moneytest.PayeeFor(money.GBP), Amount: money.Amount{Minor: 1_13, Asset: money.GBP}})
	if !errors.Is(err, money.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if b, _ := w.Balance(ctx, wal.Ref); b.Minor != 10_00 {
		t.Fatalf("balance = %d after a refused payment", b.Minor)
	}
}

// ledger keeps the credits a webhook posted.
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

// B46.3 — a partner's webhook enters through its adapter's endpoint: a good signature posts its money once, a replayed
// id is refused, and a bad or stale signature is refused with nothing posted.
func TestWebhooksAreVerifiedAndTakenOnce(t *testing.T) {
	l, trail := &ledger{}, &moneytest.Trail{}
	r := registry(t, money.Config{Ledger: l, Replays: &replays{}, Audit: trail})
	router := chi.NewRouter()
	router.Post("/v1/money/webhooks/{partner}", r.ServeWebhook)
	body := []byte(`{"id":"evt_1","to":"acct_ref","amount":{"amount_minor":2500,"asset":"GBP"},"partner_ref":"in_1","payer":"Ada Lovelace"}`)
	post := func(partner, sig string, b []byte) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/money/webhooks/"+partner, bytes.NewReader(b))
		if sig != "" {
			req.Header.Set(money.SandboxSignatureHeader, sig)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}
	now := time.Now()
	for _, tc := range []struct {
		name    string
		partner string
		sig     string
		body    []byte
		want    int
	}{
		{"no signature", "sandbox", "", body, http.StatusUnauthorized},
		{"signed with another secret", "sandbox", money.SignSandbox("whsec_other", body, now), body, http.StatusUnauthorized},
		{"the body changed after signing", "sandbox", money.SignSandbox("whsec_test", body, now), append([]byte(" "), body...), http.StatusUnauthorized},
		{"signed too long ago", "sandbox", money.SignSandbox("whsec_test", body, now.Add(-time.Hour)), body, http.StatusUnauthorized},
		{"an unknown partner", "nobody", money.SignSandbox("whsec_test", body, now), body, http.StatusNotFound},
		{"good", "sandbox", money.SignSandbox("whsec_test", body, now), body, http.StatusOK},
		{"replayed", "sandbox", money.SignSandbox("whsec_test", body, now), body, http.StatusConflict},
	} {
		if got := post(tc.partner, tc.sig, tc.body); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
	if len(l.credits) != 1 || l.credits[0].PartnerRef != "in_1" || l.credits[0].Amount != (money.Amount{Minor: 25_00, Asset: money.GBP}) {
		t.Fatalf("posted %+v, want the one credit", l.credits)
	}
	if len(trail.Kinds) != 1 || trail.Kinds[0] != "money.webhook.credit" {
		t.Fatalf("audit trail = %q", trail.Kinds)
	}
}

// B46.3 — with no secret set, the sandbox takes no webhook at all.
func TestNoSecretTakesNoWebhook(t *testing.T) {
	body := []byte(`{"id":"evt_2"}`)
	_, err := money.HMACWebhooks{}.Verify(http.Header{money.SandboxSignatureHeader: {money.SignSandbox("", body, time.Now())}}, body, time.Now())
	if !errors.Is(err, money.ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}
