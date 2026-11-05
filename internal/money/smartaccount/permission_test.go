package smartaccount

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

const (
	usdc   = "0x036CbD53842c5426634e7929541eC2318f3dCF7e"
	payee  = "0x000000000000000000000000000000000000b0b0"
	banned = "0x000000000000000000000000000000000000BAD0"
)

func transfer(t *testing.T, token, to string, amount int64) []byte {
	t.Helper()
	tw, _ := addressW(to)
	data, err := Execute(token, nil, call("transfer(address,uint256)", tw, uintW(uint64(amount))))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// B48.3 — the gate reads what an operation pays from the call itself, and refuses each payment outside the owner's
// permission for its own reason: after expiry, to a blocked or a not-allowed counterparty, in a token not allowed,
// over the cap on one payment, over the period's allowance.
func TestPermissionGate(t *testing.T) {
	now := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC) // a Wednesday
	p := Permission{Tokens: []TokenLimit{{Token: strings.ToLower(usdc), Allowance: 100_000_000, Period: PeriodDay, PerPayment: 25_000_000}},
		Blocked: []string{banned}, Expires: now.Add(24 * time.Hour)}
	p.Normalise()
	if err := p.Validate(now); err != nil {
		t.Fatal(err)
	}

	pay, err := PaymentOf(transfer(t, usdc, payee, 10_000_000))
	if err != nil || pay.Token != usdc || pay.To != payee || pay.Amount.Int64() != 10_000_000 {
		t.Fatalf("PaymentOf(transfer) = %+v, %v", pay, err)
	}
	if err := p.Check(pay, 0, now); err != nil {
		t.Fatalf("a payment inside the permission: %v", err)
	}
	coin, _ := Execute(payee, big.NewInt(5), nil)
	if pay, err := PaymentOf(coin); err != nil || pay.Token != Native || pay.Amount.Int64() != 5 {
		t.Fatalf("PaymentOf(coin) = %+v, %v", pay, err)
	}
	approve, _ := Execute(usdc, nil, call("approve(address,uint256)", word{}, uintW(1)))
	if _, err := PaymentOf(append(transfer(t, usdc, payee, 1), 0)); !errors.Is(err, ErrPermission) {
		t.Fatalf("a payment with bytes after it: %v", err)
	}
	if _, err := PaymentOf(approve); !errors.Is(err, ErrPermission) {
		t.Fatalf("an approval read as a payment: %v", err)
	}

	for _, c := range []struct {
		name  string
		pay   Payment
		spent int64
		at    time.Time
		says  string
	}{
		{"after expiry", Payment{Token: usdc, To: payee, Amount: big.NewInt(1)}, 0, now.Add(24 * time.Hour), "ended"},
		{"a blocked counterparty", Payment{Token: usdc, To: strings.ToLower(banned), Amount: big.NewInt(1)}, 0, now, "blocked"},
		{"a disallowed token", Payment{Token: Native, To: payee, Amount: big.NewInt(1)}, 0, now, "does not allow paying in"},
		{"over the per-payment cap", Payment{Token: usdc, To: payee, Amount: big.NewInt(25_000_001)}, 0, now, "cap"},
		{"over the period allowance", Payment{Token: usdc, To: payee, Amount: big.NewInt(20_000_000)}, 80_000_001, now, "allowance"},
	} {
		if err := p.Check(c.pay, c.spent, c.at); !errors.Is(err, ErrPermission) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want refused for %q", c.name, err, c.says)
		}
	}
	allowOnly := p
	allowOnly.Allowed = []string{"0x000000000000000000000000000000000000c0c0"}
	if err := allowOnly.Check(pay, 0, now); !errors.Is(err, ErrPermission) || !strings.Contains(err.Error(), "not one of") {
		t.Errorf("a counterparty not allowed: %v", err)
	}
	if err := (Permission{Expires: p.Expires}).Check(pay, 0, now); !errors.Is(err, ErrPermission) {
		t.Errorf("a revoked permission (no tokens) allows %v", err)
	}

	for period, want := range map[string]string{PeriodDay: "2026-11-04", PeriodWeek: "2026-11-02", PeriodMonth: "2026-11-01"} {
		if got := PeriodStart(period, now).Format(time.DateOnly); got != want {
			t.Errorf("PeriodStart(%s) = %s, want %s", period, got, want)
		}
	}
}

// B48.25 — a payment whose network fee an ERC-20 paymaster takes in the same token approves the paymaster that fee in
// the same operation: the gate reads both, and counts the fee with the payment against the cap and the allowance.
func TestPermissionGateCountsTheTokenFee(t *testing.T) {
	now := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)
	const paymaster = "0x000000000000000000000000000000000000FEE0"
	tw, _ := addressW(payee)
	pmw, _ := addressW(paymaster)
	batch := func(items ...batchItem) []byte {
		data, err := executeBatch(items...)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	pay := batchItem{usdc, nil, call("transfer(address,uint256)", tw, uintW(10_000_000))}
	fee := batchItem{usdc, nil, call("approve(address,uint256)", pmw, uintW(40_000))}

	got, err := PaymentOf(batch(pay, fee))
	if err != nil || got.Token != usdc || got.To != payee || got.Amount.Int64() != 10_000_000 || got.Fee.Int64() != 40_000 || got.Paymaster != paymaster {
		t.Fatalf("PaymentOf(transfer, approve) = %+v, %v", got, err)
	}
	p := Permission{Tokens: []TokenLimit{{Token: usdc, Allowance: 10_040_000, Period: PeriodDay, PerPayment: 10_040_000}}, Expires: now.Add(time.Hour)}
	if err := p.Check(got, 0, now); err != nil {
		t.Fatalf("a payment and its fee inside the permission: %v", err)
	}
	if err := p.Check(got, 1, now); !errors.Is(err, ErrPermission) || !strings.Contains(err.Error(), "allowance") {
		t.Errorf("the fee over the period's allowance: %v", err)
	}
	p.Tokens[0].PerPayment = 10_039_999
	if err := p.Check(got, 0, now); !errors.Is(err, ErrPermission) || !strings.Contains(err.Error(), "cap") {
		t.Errorf("the fee over the cap on one payment: %v", err)
	}

	other := batchItem{"0x000000000000000000000000000000000000E0C0", nil, fee.data}
	for name, data := range map[string][]byte{
		"the approval first":         batch(fee, pay),
		"a fee in another token":     batch(pay, other),
		"two payments":               batch(pay, pay),
		"a third call":               batch(pay, fee, fee),
		"the approval alone":         batch(fee),
		"bytes after the batch":      append(batch(pay, fee), 0),
		"a value sent with the call": batch(batchItem{usdc, big.NewInt(1), pay.data}, fee),
	} {
		if _, err := PaymentOf(data); !errors.Is(err, ErrPermission) {
			t.Errorf("%s read as a payment: %v", name, err)
		}
	}
}
