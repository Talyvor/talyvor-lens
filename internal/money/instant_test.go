package money_test

import (
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/moneytest"
	"github.com/talyvor/lens/internal/partners"
)

// instant is a sandbox registry whose transfer hooks are h, and a GBP and a USDC wallet holding a hundred of each.
func instant(t *testing.T, sb *money.Adapter, h partners.TransferHooks) (*money.Registry, money.Route, money.Wallet, money.Wallet) {
	t.Helper()
	r := registry(t, money.Config{Sandbox: sb, Hooks: h})
	rt := money.Route{Workspace: "ws", Country: "GB", Capability: "payments_out"}
	w, _ := r.Wallets(ctx, rt)
	tr, _ := r.Transfers(ctx, rt)
	var out []money.Wallet
	for _, a := range []money.Amount{{Minor: 100_00, Asset: money.GBP}, {Minor: 100_000_000, Asset: money.USDC}} {
		wal, err := w.Create(ctx, money.CreateWallet{Key: "w-" + a.Asset, AgentID: "agent", Asset: a.Asset})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tr.Credit(ctx, money.Credit{Key: "c-" + a.Asset, To: wal.Ref, Amount: a, PartnerRef: "p-" + a.Asset}); err != nil {
			t.Fatal(err)
		}
		out = append(out, wal)
	}
	return r, rt, out[0], out[1]
}

func balance(t *testing.T, r *money.Registry, rt money.Route, ref string) int64 {
	t.Helper()
	w, _ := r.Wallets(ctx, rt)
	b, err := w.Balance(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	return b.Minor
}

var (
	ada   = money.Party{Name: "Ada Ltd", Country: "GB"}
	grace = money.Party{Name: "Grace Hopper", Country: "US"}
)

// B59.13 — a test fails an Instant transfer at any leg on request: the leg answers the error it was given, nothing
// moves, the leg's record says how it ended, and once the switch is off the same call goes through.
func TestSandboxFailsAnyLegOnRequest(t *testing.T) {
	for _, leg := range []money.Capability{money.CapConvert, money.CapOnchain, money.CapBankPayout} {
		t.Run(string(leg), func(t *testing.T) {
			sb, h := sandbox(), &partners.TestTransferHooks{}
			r, rt, gbp, usdc := instant(t, sb, h)
			run, from := func() error { return nil }, gbp
			switch leg {
			case money.CapConvert:
				cv, _ := r.Convert(ctx, rt)
				q, err := cv.Quote(ctx, money.ConvertQuoteRequest{Key: "q", Sell: money.Amount{Minor: 40_00, Asset: money.GBP}, Buy: money.USDC})
				if err != nil {
					t.Fatal(err)
				}
				run = func() error {
					_, err := cv.Convert(ctx, money.Conversion{Key: "leg", Transfer: "it", Quote: q.Ref, From: gbp.Ref, To: usdc.Ref})
					return err
				}
			case money.CapOnchain:
				oc, _ := r.Onchain(ctx, rt)
				to := moneytest.PayeeFor(money.USDC)
				run, from = func() error {
					_, err := oc.Send(ctx, money.OnchainSend{Key: "leg", Transfer: "it", From: usdc.Ref, Network: to.Network, Address: to.Address,
						Amount: money.Amount{Minor: 40_000_000, Asset: money.USDC}, Originator: ada, Beneficiary: grace})
					return err
				}, usdc
			case money.CapBankPayout:
				bp, _ := r.BankPayouts(ctx, rt)
				run = func() error {
					_, err := bp.Pay(ctx, money.BankPayout{Key: "leg", Transfer: "it", From: gbp.Ref, To: moneytest.PayeeFor(money.GBP),
						Country: "GB", Amount: money.Amount{Minor: 40_00, Asset: money.GBP}, Reference: "rent"})
					return err
				}
			}
			before := balance(t, r, rt, from.Ref)
			if err := money.FailAt(sb, leg, money.ErrPartnerDown); err != nil {
				t.Fatal(err)
			}
			if err := run(); !errors.Is(err, money.ErrPartnerDown) {
				t.Fatalf("err = %v, want ErrPartnerDown", err)
			}
			if got := balance(t, r, rt, from.Ref); got != before {
				t.Fatalf("a failed leg moved money: %d, then %d", before, got)
			}
			if legs := h.Legs(); len(legs) != 1 || legs[0].Leg != string(leg) || legs[0].Transfer != "it" || legs[0].Outcome != "partner_down" {
				t.Fatalf("leg records = %+v", legs)
			}
			if err := money.FailAt(sb, leg, nil); err != nil {
				t.Fatal(err)
			}
			if err := run(); err != nil {
				t.Fatalf("after the switch is off: %v", err)
			}
			if got := balance(t, r, rt, from.Ref); got >= before {
				t.Fatalf("the leg moved nothing: %d, then %d", before, got)
			}
			if legs := h.Legs(); len(legs) != 2 || legs[1].Outcome != "ok" || legs[1].Ref == "" {
				t.Fatalf("leg records = %+v", legs)
			}
		})
	}
	if err := money.FailAt(&money.Adapter{Name: "circle"}, money.CapConvert, money.ErrRefused); err == nil {
		t.Fatal("a partner's adapter was switched to fail")
	}
}

// B59.13 — every leg asks its hooks before money moves: a flagged address and a sanctioned beneficiary are refused, a
// close match is held, an on-chain move keeps its originator's and beneficiary's details, and a registry with no hooks
// hands out no leg.
func TestEveryLegAsksItsHooks(t *testing.T) {
	flagged := "0x00000000000000000000000000000000000000f1"
	h := &partners.TestTransferHooks{Screening: partners.TestScreeningProvider{Addresses: map[string]string{flagged: partners.AddressMixer}}}
	r, rt, gbp, usdc := instant(t, sandbox(), h)
	oc, _ := r.Onchain(ctx, rt)
	bp, _ := r.BankPayouts(ctx, rt)
	send := func(key, address string, to money.Party) error {
		_, err := oc.Send(ctx, money.OnchainSend{Key: key, Transfer: "it-" + key, From: usdc.Ref, Network: money.SandboxNetwork,
			Address: address, Amount: money.Amount{Minor: 10_000_000, Asset: money.USDC}, Originator: ada, Beneficiary: to})
		return err
	}
	pay := func(key, name string) error {
		_, err := bp.Pay(ctx, money.BankPayout{Key: key, Transfer: "it-" + key, From: gbp.Ref, To: money.Payee{Name: name, SortCode: "040004",
			AccountNumber: "12345678"}, Country: "GB", Amount: money.Amount{Minor: 10_00, Asset: money.GBP}})
		return err
	}
	good := moneytest.PayeeFor(money.USDC).Address
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"a flagged address", send("flagged", flagged, grace), money.ErrRefused},
		{"a sanctioned beneficiary on chain", send("sanctioned", good, money.Party{Name: "TESTSANCTION Ltd"}), money.ErrRefused},
		{"a sanctioned beneficiary at a bank", pay("sanctioned-bank", "TESTSANCTION Ltd"), money.ErrRefused},
		{"a close match", pay("close", "TESTPENDING Ltd"), money.ErrHeld},
		{"screening that cannot answer", pay("down", "TESTFAIL Ltd"), money.ErrPartnerDown},
	} {
		if !errors.Is(tc.err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, tc.err, tc.want)
		}
	}
	if balance(t, r, rt, usdc.Ref) != 100_000_000 || balance(t, r, rt, gbp.Ref) != 100_00 {
		t.Fatal("a refused leg moved money")
	}
	if err := send("good", good, grace); err != nil {
		t.Fatal(err)
	}
	details := h.Details()
	if len(details) != 1 || details[0].ID != "good" || details[0].Originator.Name != ada.Name || details[0].Beneficiary.Name != grace.Name ||
		details[0].Beneficiary.Address != good || details[0].AmountMinor != 10_000_000 {
		t.Fatalf("on-chain details = %+v", details)
	}
	var outcomes []string
	for _, l := range h.Legs() {
		outcomes = append(outcomes, l.Transfer+":"+l.Outcome)
	}
	want := []string{"it-flagged:refused", "it-sanctioned:refused", "it-sanctioned-bank:refused", "it-close:held", "it-down:partner_down", "it-good:ok"}
	if len(outcomes) != len(want) {
		t.Fatalf("leg records = %q, want %q", outcomes, want)
	}
	for i := range want {
		if outcomes[i] != want[i] {
			t.Fatalf("leg records = %q, want %q", outcomes, want)
		}
	}

	bare := registry(t, money.Config{})
	if _, err := bare.Onchain(ctx, rt); !errors.Is(err, money.ErrInvalid) {
		t.Fatalf("a registry with no hooks: err = %v, want ErrInvalid", err)
	}
}
