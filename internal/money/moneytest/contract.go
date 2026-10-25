// Package moneytest is the money layer's contract (B46.3): one table of cases per capability that every adapter passes,
// the sandbox and each partner's against a local fake of its API (B46.5–B46.7).
package moneytest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/money"
)

// Allow lets every call through the owner's limits.
type Allow struct{}

// Allow lets op go.
func (Allow) Allow(context.Context, money.Op) error { return nil }

// Trail is an audit trail in memory.
type Trail struct {
	mu    sync.Mutex
	Kinds []string
}

// AppendAudit keeps kind.
func (t *Trail) AppendAudit(_ context.Context, _, _, kind string, _ map[string]any) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Kinds = append(t.Kinds, kind)
	return nil
}

// PayeeFor is a payee money in asset can be sent to.
func PayeeFor(asset string) money.Payee {
	switch asset {
	case money.USDC:
		return money.Payee{Name: "Grace Hopper", Network: "base", Address: "0x00000000000000000000000000000000000000b4"}
	case money.EUR:
		return money.Payee{Name: "Grace Hopper", IBAN: "DE89370400440532013000", BIC: "COBADEFFXXX"}
	case money.USD:
		return money.Payee{Name: "Grace Hopper", RoutingNumber: "011000015", AccountNumber: "000123456789"}
	}
	return money.Payee{Name: "Grace Hopper", SortCode: "040004", AccountNumber: "12345678"}
}

// env is one case's registry and route.
type env struct {
	reg   *money.Registry
	rt    money.Route
	asset string
}

func (e env) wallets(t *testing.T) money.Wallets {
	return must(e.reg.Wallets(context.Background(), e.rt))(t)
}
func (e env) transfers(t *testing.T) money.Transfers {
	return must(e.reg.Transfers(context.Background(), e.rt))(t)
}

// wallet is a new wallet in the case's asset, holding fund minor units.
func (e env) wallet(t *testing.T, key string, fund int64) money.Wallet {
	t.Helper()
	w := must(e.wallets(t).Create(context.Background(), money.CreateWallet{Key: key, AgentID: "agent-" + key, Asset: e.asset}))(t)
	if fund > 0 {
		must(e.transfers(t).Credit(context.Background(), money.Credit{Key: "fund-" + key, To: w.Ref,
			Amount: money.Amount{Minor: fund, Asset: e.asset}, PartnerRef: "in-" + key}))(t)
	}
	return w
}

func (e env) balance(t *testing.T, ref string) int64 {
	t.Helper()
	return must(e.wallets(t).Balance(context.Background(), ref))(t).Minor
}

func must[T any](v T, err error) func(*testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

type contractCase struct {
	name string
	run  func(t *testing.T, e env)
}

var ctx = context.Background()

// The cases, one table per capability.
var contract = map[money.Capability][]contractCase{
	money.CapAccounts: {
		{"open gives details to pay into", func(t *testing.T, e env) {
			a := must(must(e.reg.Accounts(ctx, e.rt))(t).Open(ctx, money.OpenAccount{Key: "acct-1", Holder: "Ada Ltd", Asset: e.asset, Country: e.rt.Country}))(t)
			if a.Ref == "" || a.Asset != e.asset || a.Status != money.StatusCompleted {
				t.Fatalf("account = %+v", a)
			}
			d := must(must(e.reg.Accounts(ctx, e.rt))(t).Details(ctx, a.Ref))(t)
			if d.Asset != e.asset || d.IBAN == "" && d.AccountNumber == "" && d.Address == "" {
				t.Fatalf("details = %+v", d)
			}
		}},
		{"the same key is the same account", func(t *testing.T, e env) {
			acc := must(e.reg.Accounts(ctx, e.rt))(t)
			req := money.OpenAccount{Key: "acct-2", Holder: "Ada Ltd", Asset: e.asset, Country: e.rt.Country}
			a, b := must(acc.Open(ctx, req))(t), must(acc.Open(ctx, req))(t)
			if a.Ref != b.Ref {
				t.Fatalf("a retry opened %q then %q", a.Ref, b.Ref)
			}
			req.Holder = "Someone Else Ltd"
			_, err := acc.Open(ctx, req)
			wantErr(t, err, money.ErrKeyReused)
		}},
		{"no key is refused", func(t *testing.T, e env) {
			_, err := must(e.reg.Accounts(ctx, e.rt))(t).Open(ctx, money.OpenAccount{Holder: "Ada Ltd", Asset: e.asset})
			wantErr(t, err, money.ErrInvalid)
		}},
		{"a closed account has no details", func(t *testing.T, e env) {
			acc := must(e.reg.Accounts(ctx, e.rt))(t)
			a := must(acc.Open(ctx, money.OpenAccount{Key: "acct-3", Holder: "Ada Ltd", Asset: e.asset, Country: e.rt.Country}))(t)
			if r := must(acc.Close(ctx, "close-3", a.Ref))(t); r.Status != money.StatusCompleted {
				t.Fatalf("close = %+v", r)
			}
			_, err := acc.Details(ctx, a.Ref)
			wantErr(t, err, money.ErrNotFound)
		}},
		{"an unknown account is not found", func(t *testing.T, e env) {
			_, err := must(e.reg.Accounts(ctx, e.rt))(t).Details(ctx, "no-such-account")
			wantErr(t, err, money.ErrNotFound)
		}},
	},
	money.CapWallets: {
		{"a new wallet is empty and can be paid into", func(t *testing.T, e env) {
			w := e.wallet(t, "wal-1", 0)
			if w.Ref == "" || w.Asset != e.asset || e.balance(t, w.Ref) != 0 {
				t.Fatalf("wallet = %+v", w)
			}
			d := must(e.wallets(t).Receive(ctx, w.Ref))(t)
			if e.asset == money.USDC && (d.Address == "" || d.Network == "") || e.asset != money.USDC && d.IBAN == "" && d.AccountNumber == "" {
				t.Fatalf("receive details = %+v", d)
			}
		}},
		{"the same key is the same wallet", func(t *testing.T, e env) {
			a, b := e.wallet(t, "wal-2", 0), e.wallet(t, "wal-2", 0)
			if a.Ref != b.Ref {
				t.Fatalf("a retry created %q then %q", a.Ref, b.Ref)
			}
		}},
		{"an unknown wallet is not found", func(t *testing.T, e env) {
			_, err := e.wallets(t).Balance(ctx, "no-such-wallet")
			wantErr(t, err, money.ErrNotFound)
		}},
	},
	money.CapTransfers: {
		{"a credit adds once", func(t *testing.T, e env) {
			w := e.wallet(t, "tr-1", 10_00)
			must(e.transfers(t).Credit(ctx, money.Credit{Key: "fund-tr-1", To: w.Ref, Amount: money.Amount{Minor: 10_00, Asset: e.asset}, PartnerRef: "in-tr-1"}))(t)
			if got := e.balance(t, w.Ref); got != 10_00 {
				t.Fatalf("balance = %d, want 1000", got)
			}
		}},
		{"an internal transfer moves money once", func(t *testing.T, e env) {
			a, b := e.wallet(t, "tr-2a", 10_00), e.wallet(t, "tr-2b", 0)
			req := money.Internal{Key: "move-2", From: a.Ref, To: b.Ref, Amount: money.Amount{Minor: 4_00, Asset: e.asset}}
			must(e.transfers(t).Internal(ctx, req))(t)
			must(e.transfers(t).Internal(ctx, req))(t)
			if e.balance(t, a.Ref) != 6_00 || e.balance(t, b.Ref) != 4_00 {
				t.Fatalf("balances %d and %d, want 600 and 400", e.balance(t, a.Ref), e.balance(t, b.Ref))
			}
		}},
		{"more than the wallet holds is insufficient funds", func(t *testing.T, e env) {
			a, b := e.wallet(t, "tr-3a", 1_00), e.wallet(t, "tr-3b", 0)
			_, err := e.transfers(t).Internal(ctx, money.Internal{Key: "move-3", From: a.Ref, To: b.Ref, Amount: money.Amount{Minor: 2_00, Asset: e.asset}})
			wantErr(t, err, money.ErrInsufficientFunds)
			_, err = e.transfers(t).Outbound(ctx, money.Outbound{Key: "out-3", From: a.Ref, Payee: PayeeFor(e.asset), Amount: money.Amount{Minor: 2_00, Asset: e.asset}})
			wantErr(t, err, money.ErrInsufficientFunds)
			if e.balance(t, a.Ref) != 1_00 {
				t.Fatalf("a refused transfer moved money: %d", e.balance(t, a.Ref))
			}
		}},
		{"an outbound payment debits once", func(t *testing.T, e env) {
			a := e.wallet(t, "tr-4", 10_00)
			req := money.Outbound{Key: "out-4", From: a.Ref, Payee: PayeeFor(e.asset), Amount: money.Amount{Minor: 3_00, Asset: e.asset}, Reference: "invoice 4"}
			r := must(e.transfers(t).Outbound(ctx, req))(t)
			if r.Ref == "" || r.Status == money.StatusFailed {
				t.Fatalf("outbound = %+v", r)
			}
			must(e.transfers(t).Outbound(ctx, req))(t)
			if got := e.balance(t, a.Ref); got != 7_00 {
				t.Fatalf("balance = %d, want 700", got)
			}
		}},
		{"a non-positive amount is refused", func(t *testing.T, e env) {
			a := e.wallet(t, "tr-5", 10_00)
			_, err := e.transfers(t).Outbound(ctx, money.Outbound{Key: "out-5", From: a.Ref, Payee: PayeeFor(e.asset), Amount: money.Amount{Minor: 0, Asset: e.asset}})
			wantErr(t, err, money.ErrInvalid)
		}},
	},
	money.CapCards: {
		{"a card is issued, locked, frozen and closed", func(t *testing.T, e env) {
			w := e.wallet(t, "card-1", 0)
			cards := must(e.reg.Cards(ctx, e.rt))(t)
			c := must(cards.Issue(ctx, money.IssueCard{Key: "card-1", Wallet: w.Ref, Merchant: "OpenAI", Cap: money.Amount{Minor: 50_00, Asset: e.asset}}))(t)
			if c.Ref == "" || c.State != money.CardActive || c.Last4 == "" || c.Merchant != "OpenAI" {
				t.Fatalf("card = %+v", c)
			}
			c = must(cards.Lock(ctx, money.LockCard{Key: "lock-1", Ref: c.Ref, Merchant: "Anthropic", Cap: money.Amount{Minor: 20_00, Asset: e.asset}}))(t)
			if c.Merchant != "Anthropic" || c.Cap.Minor != 20_00 {
				t.Fatalf("locked card = %+v", c)
			}
			if c = must(cards.Freeze(ctx, "freeze-1", c.Ref))(t); c.State != money.CardFrozen {
				t.Fatalf("frozen card = %+v", c)
			}
			if c = must(cards.Close(ctx, "close-1", c.Ref))(t); c.State != money.CardClosed {
				t.Fatalf("closed card = %+v", c)
			}
			_, err := cards.Freeze(ctx, "freeze-1b", c.Ref)
			wantErr(t, err, money.ErrRefused)
		}},
		{"a card on an unknown wallet is not found", func(t *testing.T, e env) {
			_, err := must(e.reg.Cards(ctx, e.rt))(t).Issue(ctx, money.IssueCard{Key: "card-2", Wallet: "no-such-wallet", Cap: money.Amount{Minor: 1_00, Asset: e.asset}})
			wantErr(t, err, money.ErrNotFound)
		}},
	},
	money.CapPayouts: {
		{"a seller is paid once", func(t *testing.T, e env) {
			w := e.wallet(t, "po-1", 10_00)
			req := money.Payout{Key: "po-1", From: w.Ref, Recipient: money.PayoutSeller, To: PayeeFor(e.asset), Amount: money.Amount{Minor: 8_50, Asset: e.asset}}
			must(must(e.reg.Payouts(ctx, e.rt))(t).Pay(ctx, req))(t)
			must(must(e.reg.Payouts(ctx, e.rt))(t).Pay(ctx, req))(t)
			if got := e.balance(t, w.Ref); got != 1_50 {
				t.Fatalf("balance = %d, want 150", got)
			}
		}},
		{"a payout over the balance is insufficient funds", func(t *testing.T, e env) {
			w := e.wallet(t, "po-2", 1_00)
			_, err := must(e.reg.Payouts(ctx, e.rt))(t).Pay(ctx, money.Payout{Key: "po-2", From: w.Ref, Recipient: money.PayoutRoomMember,
				To: PayeeFor(e.asset), Amount: money.Amount{Minor: 5_00, Asset: e.asset}})
			wantErr(t, err, money.ErrInsufficientFunds)
		}},
		{"a payout to nobody in particular is refused", func(t *testing.T, e env) {
			w := e.wallet(t, "po-3", 10_00)
			_, err := must(e.reg.Payouts(ctx, e.rt))(t).Pay(ctx, money.Payout{Key: "po-3", From: w.Ref, Recipient: "anyone",
				To: PayeeFor(e.asset), Amount: money.Amount{Minor: 1_00, Asset: e.asset}})
			wantErr(t, err, money.ErrInvalid)
		}},
	},
	money.CapKYC: {
		{"a verification starts and is read back", func(t *testing.T, e env) {
			kyc := must(e.reg.KYC(ctx, e.rt))(t)
			for _, subject := range []string{money.KYCPerson, money.KYCCompany} {
				v := must(kyc.Start(ctx, money.StartKYC{Key: "kyc-" + subject, Subject: subject, Name: "Ada Lovelace", Country: e.rt.Country}))(t)
				if v.Ref == "" || v.Status == "" {
					t.Fatalf("%s verification = %+v", subject, v)
				}
				if s := must(kyc.Status(ctx, v.Ref))(t); s.Ref != v.Ref {
					t.Fatalf("status of %q = %+v", v.Ref, s)
				}
			}
		}},
		{"an unknown verification is not found", func(t *testing.T, e env) {
			_, err := must(e.reg.KYC(ctx, e.rt))(t).Status(ctx, "no-such-check")
			wantErr(t, err, money.ErrNotFound)
		}},
		{"a verification of nobody in particular is refused", func(t *testing.T, e env) {
			_, err := must(e.reg.KYC(ctx, e.rt))(t).Start(ctx, money.StartKYC{Key: "kyc-x", Subject: "pet", Name: "Rex"})
			wantErr(t, err, money.ErrInvalid)
		}},
	},
}

// Contract runs every case of every capability adapter offers, in each of assets, each case on a fresh adapter from
// newAdapter. Every call goes through a Registry, as Lens's do.
func Contract(t *testing.T, newAdapter func() *money.Adapter, country string, assets ...string) {
	for _, c := range money.Capabilities {
		for _, cc := range contract[c] {
			for _, asset := range assets {
				t.Run(string(c)+"/"+asset+"/"+cc.name, func(t *testing.T) {
					a := newAdapter()
					reg, err := money.NewRegistry(money.Config{Sandbox: a, Limits: Allow{}, Audit: &Trail{}})
					if err != nil {
						t.Fatal(err)
					}
					rt := money.Route{Workspace: "ws-contract", Country: country, Capability: "payments_out"}
					if _, err := reg.Partner(ctx, c, rt); err != nil {
						t.Skipf("%s does not offer %s", a.Name, c)
					}
					cc.run(t, env{reg: reg, rt: rt, asset: asset})
				})
			}
		}
	}
}
