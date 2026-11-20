package economy

import (
	"fmt"
	"math"
	"time"
)

// B55.30 — amounts written for a person: pushes, the briefing, approval memos and refusals. API fields, CSV
// statements and logs keep µLXC and FormatMinor; a sentence never says LXC (B61.16).

var moneySigns = map[string]string{CurrencyUSD: "$", CurrencyGBP: "£", CurrencyEUR: "€"}

// usdTenMillionthsPerULXC is what one µLXC is worth at the peg, in ten-millionths of a dollar (1 at $0.10), so
// an amount is written to its last µLXC.
var usdTenMillionthsPerULXC = int64(math.Round(LXCUSDValue * 10))

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

// LXCMoney writes µLXC as the dollars it is worth at the peg, and nothing else (B61.16): "$340.00", and to the
// last µLXC below a cent, "$0.0000001", so a refusal never reads a limit as $0.00.
func LXCMoney(ulxc int64) string {
	sign := ""
	if ulxc < 0 {
		sign, ulxc = "-", -ulxc
	}
	return sign + "$" + grouped(ulxc*usdTenMillionthsPerULXC, 7, 2)
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
