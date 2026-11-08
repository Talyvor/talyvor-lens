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
