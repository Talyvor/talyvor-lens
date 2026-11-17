// Package fees holds Talyvor's fees — Nicolai's decisions of 5 Oct 2026 (B32.8). Each is a setting in
// lens.env.example whose default is his value, and nothing else in the code holds a fee: every feature that
// charges one reads it from Current. A setting for a feature not built yet is read only by the item that
// builds it, which flips its entry in Charged.
package fees

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/talyvor/lens/internal/plans"
)

// BPSDenominator is 100%, in basis points.
const BPSDenominator = 10_000

// Settings is every fee, under the names GET /v1/public/fees gives them.
type Settings struct {
	// Listing sales, rentals, subscriptions and per-use charges (uses inside Rooms included): Talyvor keeps
	// this much of the price before tax from the first dollar, and the seller keeps the rest.
	MarketTakeBPS int64 `json:"market_take_bps"`
	// Jobs, and a payment to another company's agent through the marketplace (B19.15).
	ServicesTakeBPS int64 `json:"services_take_bps"`
	// What a registered compute node earns for serving another workspace's request: Talyvor keeps this much (B56.20).
	ComputeTakeBPS int64 `json:"compute_take_bps"`
	LendingFeeBPS  int64 `json:"lending_fee_bps"` // on a loan arranged
	// On AI spend charged to credits, by plan (internal/plans): each plan the 5 Oct plan at its step paid (B61.57).
	PlatformFeeBPS map[string]int64 `json:"platform_fee_bps"`
	// Over the reference rate on a currency conversion, and so on an Instant transfer, by plan (B61.57). Set, each
	// by-plan fee names every plan in plans.Order and no other; one naming exactly the 5 Oct plans is out of date, and
	// Lens says so in its log and runs on the default. Any other plan set stops Lens starting rather than charging nothing.
	FXMarginBPS map[string]int64 `json:"fx_margin_bps"`
	// B61.58: what a plan converts free each calendar month (UTC), in minor units of the currency sold, counted per
	// currency, before FXMarginBPS is taken on the part above it; -1 is no limit.
	FXFreeMinor map[string]int64 `json:"fx_free_minor"`
	// On an international payment, in minor units of the currency it is sent in, plus the partner's cost.
	IntlPaymentFeeMinor map[string]int64 `json:"intl_payment_fee_minor"`
	// B61.58: how many international payments a plan sends free each calendar month (UTC), and what it pays less on
	// the others, by plan.
	IntlFreePerMonth map[string]int64 `json:"intl_free_per_month"`
	IntlDiscountBPS  map[string]int64 `json:"intl_discount_bps"`
	// B61.58: on a payment out in the account's own currency, in minor units of it, after the plan's free ones each
	// calendar month (UTC), -1 every one free. A payment to another Talyvor account is never charged.
	LocalPaymentFeeMinor map[string]int64 `json:"local_payment_fee_minor"`
	LocalFreePerMonth    map[string]int64 `json:"local_free_per_month"`
	MerchantFeeBPS       int64            `json:"merchant_fee_bps"`            // accepting a domestic consumer card payment, plus card processing
	MerchantOtherCardBPS int64            `json:"merchant_other_card_fee_bps"` // accepting any other card (B61.58)
	MerchantA2AFeeBPS    int64            `json:"merchant_a2a_fee_bps"`        // accepting a payment from an account or in USDC
	// B61.58: on every payment accepted, beside the fee above, in minor units by currency.
	MerchantFixedFeeMinor map[string]int64 `json:"merchant_fixed_fee_minor"`
	// What a Scan agent's wallet pays, in µLXC of credits, for each alert its watch sends (B52.27).
	ScanAlertULXC int64 `json:"scan_alert_ulxc"`
}

// Charged is, by each fee's name in GET /v1/public/fees, whether Lens charges it today (B56.8). The item that
// builds a fee flips its entry in the same change; a test holds each entry to whether code outside this package
// reads that fee.
var Charged = map[string]bool{
	"market_take_bps": true, "services_take_bps": true, "compute_take_bps": true, "lending_fee_bps": false,
	"platform_fee_bps": true, "fx_margin_bps": true, "fx_free_minor": true, "intl_payment_fee_minor": true,
	"intl_free_per_month": true, "intl_discount_bps": true, "local_payment_fee_minor": true, "local_free_per_month": true,
	"merchant_fee_bps": true, "merchant_other_card_fee_bps": false, "merchant_a2a_fee_bps": true, "merchant_fixed_fee_minor": false,
	"scan_alert_ulxc": true,
}

// Unlimited, in a by-plan allowance, is no limit.
const Unlimited = -1

// Defaults are Nicolai's values.
func Defaults() Settings {
	return Settings{
		MarketTakeBPS:   1500,
		ServicesTakeBPS: 500,
		ComputeTakeBPS:  500,
		LendingFeeBPS:   100,
		PlatformFeeBPS: map[string]int64{"personal_solo": 550, "personal_crew": 300, "personal_fleet": 100,
			"business_studio": 550, "business_company": 300, "business_group": 100, "business_custom": 100},
		FXMarginBPS: map[string]int64{"personal_solo": 100, "personal_crew": 50, "personal_fleet": 0,
			"business_studio": 60, "business_company": 60, "business_group": 60, "business_custom": 15},
		// B61.58, Nick's 15 Nov fee table: 1,000, 3,000 and no limit for a person; 1,000, 15,000 and 60,000 for a company.
		FXFreeMinor: map[string]int64{"personal_solo": 100_000, "personal_crew": 300_000, "personal_fleet": Unlimited,
			"business_studio": 100_000, "business_company": 1_500_000, "business_group": 6_000_000, "business_custom": 0},
		IntlPaymentFeeMinor: map[string]int64{"GBP": 500, "EUR": 600, "USD": 700},
		IntlFreePerMonth: map[string]int64{"personal_solo": 0, "personal_crew": 0, "personal_fleet": 0,
			"business_studio": 0, "business_company": 5, "business_group": 25, "business_custom": 0},
		IntlDiscountBPS: map[string]int64{"personal_solo": 0, "personal_crew": 0, "personal_fleet": 2000,
			"business_studio": 0, "business_company": 0, "business_group": 0, "business_custom": 0},
		LocalPaymentFeeMinor: map[string]int64{"GBP": 20, "EUR": 20, "USD": 20},
		LocalFreePerMonth: map[string]int64{"personal_solo": Unlimited, "personal_crew": Unlimited, "personal_fleet": Unlimited,
			"business_studio": 10, "business_company": 100, "business_group": 1000, "business_custom": Unlimited},
		MerchantFeeBPS:        100,
		MerchantOtherCardBPS:  280,
		MerchantA2AFeeBPS:     100,
		MerchantFixedFeeMinor: map[string]int64{"GBP": 20, "EUR": 20, "USD": 20},
	}
}

// Load reads every fee from getenv, each unset one taking its default.
func Load(getenv func(string) string) (Settings, error) {
	s := Defaults()
	for _, f := range []struct {
		name string
		into *int64
	}{
		{"LENS_MARKET_TAKE_BPS", &s.MarketTakeBPS},
		{"LENS_SERVICES_TAKE_BPS", &s.ServicesTakeBPS},
		{"LENS_COMPUTE_TAKE_BPS", &s.ComputeTakeBPS},
		{"LENS_LENDING_FEE_BPS", &s.LendingFeeBPS},
		{"LENS_MERCHANT_FEE_BPS", &s.MerchantFeeBPS},
		{"LENS_MERCHANT_OTHER_CARD_FEE_BPS", &s.MerchantOtherCardBPS},
		{"LENS_MERCHANT_A2A_FEE_BPS", &s.MerchantA2AFeeBPS},
	} {
		if err := bps(f.name, getenv(f.name), f.into); err != nil {
			return s, err
		}
	}
	for _, f := range []struct {
		name   string
		into   *map[string]int64
		ceil   int64
		units  string
		byPlan bool
		floor  int64 // Unlimited where a by-plan allowance may have no limit
	}{
		{"LENS_PLATFORM_FEE_BPS", &s.PlatformFeeBPS, BPSDenominator, "basis points", true, 0},
		{"LENS_FX_MARGIN_BPS", &s.FXMarginBPS, BPSDenominator, "basis points", true, 0},
		{"LENS_FX_FREE_MINOR", &s.FXFreeMinor, -1, "minor units", true, Unlimited},
		{"LENS_INTL_PAYMENT_FEE_MINOR", &s.IntlPaymentFeeMinor, -1, "minor units", false, 0},
		{"LENS_INTL_FREE_PER_MONTH", &s.IntlFreePerMonth, -1, "payments", true, 0},
		{"LENS_INTL_DISCOUNT_BPS", &s.IntlDiscountBPS, BPSDenominator, "basis points", true, 0},
		{"LENS_LOCAL_PAYMENT_FEE_MINOR", &s.LocalPaymentFeeMinor, -1, "minor units", false, 0},
		{"LENS_LOCAL_FREE_PER_MONTH", &s.LocalFreePerMonth, -1, "payments", true, Unlimited},
		{"LENS_MERCHANT_FIXED_FEE_MINOR", &s.MerchantFixedFeeMinor, -1, "minor units", false, 0},
	} {
		v := strings.TrimSpace(getenv(f.name))
		if v == "" {
			continue
		}
		var m map[string]int64
		if err := json.Unmarshal([]byte(v), &m); err != nil || len(m) == 0 {
			return s, fmt.Errorf("fees: %s must be a JSON object of %s, like %s", f.name, f.units, example(*f.into))
		}
		for k, n := range m {
			if n < f.floor || (f.ceil >= 0 && n > f.ceil) {
				return s, fmt.Errorf("fees: %s[%q] is %d %s, outside %d to %d", f.name, k, n, f.units, f.floor, f.ceil)
			}
		}
		named := slices.Sorted(maps.Keys(m))
		if f.byPlan && plans.NamesOctoberPlans(named) {
			plans.OutOfDate(f.name)
			continue
		}
		if f.byPlan && !slices.Equal(named, slices.Sorted(slices.Values(plans.Order))) {
			return s, fmt.Errorf("fees: %s names %s; it names every plan, %s, and no other", f.name, strings.Join(named, ", "), strings.Join(plans.Order, ", "))
		}
		*f.into = m
	}
	if v := strings.TrimSpace(getenv("LENS_SCAN_ALERT_ULXC")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return s, fmt.Errorf("fees: LENS_SCAN_ALERT_ULXC=%q must be whole µLXC, 0 or more", v)
		}
		s.ScanAlertULXC = n
	}
	return s, nil
}

func bps(name, v string, into *int64) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || n > BPSDenominator {
		return fmt.Errorf("fees: %s=%q must be whole basis points from 0 to %d", name, v, BPSDenominator)
	}
	*into = n
	return nil
}

func example(m map[string]int64) string {
	b, _ := json.Marshal(m)
	return string(b)
}

var (
	once    sync.Once
	current Settings
	loadErr error
)

func load() { current, loadErr = Load(os.Getenv) }

// Check reports a malformed fee setting; Lens will not start with one.
func Check() error {
	once.Do(load)
	return loadErr
}

// Current is the fees this process runs with, read from its environment once.
func Current() Settings {
	once.Do(load)
	s := current
	s.PlatformFeeBPS = maps.Clone(s.PlatformFeeBPS)
	s.FXMarginBPS = maps.Clone(s.FXMarginBPS)
	for _, m := range []*map[string]int64{&s.FXFreeMinor, &s.IntlPaymentFeeMinor, &s.IntlFreePerMonth, &s.IntlDiscountBPS,
		&s.LocalPaymentFeeMinor, &s.LocalFreePerMonth, &s.MerchantFixedFeeMinor} {
		*m = maps.Clone(*m)
	}
	return s
}
