package economy

import (
	"fmt"
	"math"
	"os"
	"time"
)

// B55.30 — amounts written for a person: pushes, the briefing, approval memos and refusals. API fields, CSV
// statements and logs keep µLXC and FormatMinor.

// MoneyOrderEnv names which comes first beside an LXC amount: usd_first (the default) or lxc_first.
const MoneyOrderEnv = "LENS_MONEY_ORDER"

// CheckMoneyOrder reports a LENS_MONEY_ORDER Lens does not know; Lens will not start with one.
func CheckMoneyOrder() error {
	switch v := os.Getenv(MoneyOrderEnv); v {
	case "", "usd_first", "lxc_first":
		return nil
	default:
		return fmt.Errorf("%s=%q: want usd_first or lxc_first", MoneyOrderEnv, v)
	}
}

var moneySigns = map[string]string{CurrencyUSD: "$", CurrencyGBP: "£", CurrencyEUR: "€"}

// ulxcPerCent is how many µLXC one US cent buys at the peg (100,000 at $0.10).
var ulxcPerCent = int64(math.Round(1e4 / LXCUSDValue))

// Money writes minor units for a person: "$1,240.00", "£120.50", "-$4.00", "1.50 USDC"; µLXC as LXCMoney does.
func Money(minor int64, currency string) string {
	if currency == "LXC" {
		return LXCMoney(minor)
	}
	places, ok := MoneyCurrencies[currency]
	if !ok {
		return FormatMinor(minor, currency)
	}
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	if s, ok := moneySigns[currency]; ok {
		return sign + s + grouped(minor, places, 2)
	}
	return sign + grouped(minor, places, 2) + " " + currency
}

// LXCMoney writes µLXC as its dollars at the peg beside the LXC: "$340.00 (3,400 LXC)", or "3,400 LXC ($340.00)"
// under LENS_MONEY_ORDER=lxc_first.
func LXCMoney(ulxc int64) string {
	sign := ""
	if ulxc < 0 {
		sign, ulxc = "-", -ulxc
	}
	usd := sign + "$" + grouped((ulxc+ulxcPerCent/2)/ulxcPerCent, 2, 2)
	lxc := sign + grouped(ulxc, 6, 0) + " LXC"
	if os.Getenv(MoneyOrderEnv) == "lxc_first" {
		return lxc + " (" + usd + ")"
	}
	return usd + " (" + lxc + ")"
}

// When writes a moment for a person: "Oct 31, 2026 at 09:00 UTC".
func When(t time.Time) string {
	return t.UTC().Format("Jan 2, 2006 at 15:04 UTC")
}

// grouped writes n / 10^places with thousands separators, keeping at least keep decimals and dropping trailing
// zeros past them.
func grouped(n int64, places, keep int) string {
	digits := fmt.Sprintf("%0*d", places+1, n)
	whole, frac := digits[:len(digits)-places], digits[len(digits)-places:]
	for len(frac) > keep && frac[len(frac)-1] == '0' {
		frac = frac[:len(frac)-1]
	}
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	if frac == "" {
		return whole
	}
	return whole + "." + frac
}
