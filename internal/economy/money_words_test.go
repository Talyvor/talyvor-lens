package economy

import "testing"

// B55.30 — the amounts Lens writes for a person: dollars, pounds and euros by their sign with thousands separators,
// USDC by name to its six places, and LXC beside its dollars at the peg, in the order LENS_MONEY_ORDER names.
func TestMoney_WritesAmountsForAPerson(t *testing.T) {
	for _, c := range []struct {
		minor    int64
		currency string
		want     string
	}{
		{0, "USD", "$0.00"},
		{1, "USD", "$0.01"},
		{999, "USD", "$9.99"},
		{100000, "USD", "$1,000.00"},
		{124000, "USD", "$1,240.00"},
		{-400, "USD", "-$4.00"},
		{12050, "GBP", "£120.50"},
		{300, "EUR", "€3.00"},
		{1_500_000, "USDC", "1.50 USDC"},
		{1, "USDC", "0.000001 USDC"},
		{1_500_000_000, "USDC", "1,500.00 USDC"},
	} {
		if got := Money(c.minor, c.currency); got != c.want {
			t.Errorf("Money(%d, %s) = %q, want %q", c.minor, c.currency, got, c.want)
		}
	}
}

func TestLXCMoney_DollarsBesideLXCInEitherOrder(t *testing.T) {
	for _, c := range []struct {
		ulxc          int64
		usd, lxcFirst string
	}{
		{0, "$0.00 (0 LXC)", "0 LXC ($0.00)"},
		{1, "$0.00 (0.000001 LXC)", "0.000001 LXC ($0.00)"},
		{999, "$0.00 (0.000999 LXC)", "0.000999 LXC ($0.00)"},
		{100000, "$0.01 (0.1 LXC)", "0.1 LXC ($0.01)"},
		{3_400_000_000, "$340.00 (3,400 LXC)", "3,400 LXC ($340.00)"},
		{-4_000_000, "-$0.40 (-4 LXC)", "-4 LXC (-$0.40)"},
	} {
		t.Setenv(MoneyOrderEnv, "")
		if got := LXCMoney(c.ulxc); got != c.usd {
			t.Errorf("LXCMoney(%d) = %q, want %q", c.ulxc, got, c.usd)
		}
		if got := Money(c.ulxc, "LXC"); got != c.usd {
			t.Errorf("Money(%d, LXC) = %q, want %q", c.ulxc, got, c.usd)
		}
		t.Setenv(MoneyOrderEnv, "lxc_first")
		if got := LXCMoney(c.ulxc); got != c.lxcFirst {
			t.Errorf("lxc_first: LXCMoney(%d) = %q, want %q", c.ulxc, got, c.lxcFirst)
		}
	}
}

func TestCheckMoneyOrder_RefusesAnythingElse(t *testing.T) {
	for v, ok := range map[string]bool{"": true, "usd_first": true, "lxc_first": true, "USD_FIRST": false, "dollars": false} {
		t.Setenv(MoneyOrderEnv, v)
		if err := CheckMoneyOrder(); (err == nil) != ok {
			t.Errorf("LENS_MONEY_ORDER=%q: CheckMoneyOrder() = %v", v, err)
		}
	}
}

// B55.32 — a request's heading: every item blocked pays nothing, two currencies join with "and", one covered item says
// so, and a seller's title cannot carry a control or bidi character into the heading.
func TestPaymentRequestSummary_ReadsAsASentence(t *testing.T) {
	item := func(name, decision string) PaymentRequestItem {
		return PaymentRequestItem{PayeeName: name, Decision: decision}
	}
	for _, c := range []struct {
		items  []PaymentRequestItem
		totals map[string]int64
		want   string
	}{
		{[]PaymentRequestItem{item("Acme", ItemBlocked), item("Kite", ItemBlocked)}, map[string]int64{}, "Nothing to pay: every payment was blocked"},
		{[]PaymentRequestItem{item("Acme", ItemNeedsApproval), item("Kite", ItemNeedsApproval)}, map[string]int64{"USD": 10000, "GBP": 5000},
			"Pay 2 payments, £50.00 and $100.00"},
		{[]PaymentRequestItem{item("Kite Accounting", ItemAuto)}, map[string]int64{"USD": 45000}, "Pay Kite Accounting $450.00: 1 inside a permission you signed"},
		{[]PaymentRequestItem{item("Acme\u202e00.1$ \nData", ItemNeedsApproval)}, map[string]int64{"USD": 34000}, "Pay Acme00.1$ Data $340.00"},
		{[]PaymentRequestItem{item("Acme", ItemNeedsApproval), item("Kite", ItemBlocked)}, map[string]int64{"USD": 34000}, "Pay 2 payments, $340.00: 1 waits for you, 1 blocked"},
	} {
		counts := map[string]int{}
		for _, it := range c.items {
			counts[it.Decision]++
		}
		if got := paymentRequestSummary(PaymentRequest{Items: c.items, Totals: c.totals}, counts); got != c.want {
			t.Errorf("summary = %q, want %q", got, c.want)
		}
	}
}
