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
	"strconv"
	"strings"
	"sync"
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
	// On AI spend charged to credits, by plan.
	PlatformFeeBPS map[string]int64 `json:"platform_fee_bps"`
	// Over the reference rate on a currency conversion, by plan.
	FXMarginBPS map[string]int64 `json:"fx_margin_bps"`
	// On an international payment, in minor units of the currency it is sent in, plus the partner's cost.
	IntlPaymentFeeMinor map[string]int64 `json:"intl_payment_fee_minor"`
	MerchantFeeBPS      int64            `json:"merchant_fee_bps"`     // accepting a card payment, plus card processing
	MerchantA2AFeeBPS   int64            `json:"merchant_a2a_fee_bps"` // accepting a payment from an account
	// What a Scan agent's wallet pays, in µLXC of credits, for each alert its watch sends (B52.27).
	ScanAlertULXC int64 `json:"scan_alert_ulxc"`
}

// Charged is, by each fee's name in GET /v1/public/fees, whether Lens charges it today (B56.8). The item that
// builds a fee flips its entry in the same change; a test holds each entry to whether code outside this package
// reads that fee.
var Charged = map[string]bool{
	"market_take_bps": true, "services_take_bps": true, "compute_take_bps": true, "lending_fee_bps": false,
	"platform_fee_bps": true, "fx_margin_bps": true, "intl_payment_fee_minor": true,
	"merchant_fee_bps": false, "merchant_a2a_fee_bps": false, "scan_alert_ulxc": true,
}

// Defaults are Nicolai's values.
func Defaults() Settings {
	return Settings{
		MarketTakeBPS:       1500,
		ServicesTakeBPS:     500,
		ComputeTakeBPS:      500,
		LendingFeeBPS:       100,
		PlatformFeeBPS:      map[string]int64{"free": 550, "team": 300, "business": 100, "enterprise": 100},
		FXMarginBPS:         map[string]int64{"free": 0, "team": 50, "business": 25, "enterprise": 15},
		IntlPaymentFeeMinor: map[string]int64{"GBP": 500, "EUR": 600, "USD": 700},
		MerchantFeeBPS:      75,
		MerchantA2AFeeBPS:   100,
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
		{"LENS_MERCHANT_A2A_FEE_BPS", &s.MerchantA2AFeeBPS},
	} {
		if err := bps(f.name, getenv(f.name), f.into); err != nil {
			return s, err
		}
	}
	for _, f := range []struct {
		name  string
		into  *map[string]int64
		ceil  int64
		units string
	}{
		{"LENS_PLATFORM_FEE_BPS", &s.PlatformFeeBPS, BPSDenominator, "basis points"},
		{"LENS_FX_MARGIN_BPS", &s.FXMarginBPS, BPSDenominator, "basis points"},
		{"LENS_INTL_PAYMENT_FEE_MINOR", &s.IntlPaymentFeeMinor, -1, "minor units"},
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
			if n < 0 || (f.ceil >= 0 && n > f.ceil) {
				return s, fmt.Errorf("fees: %s[%q] is %d %s, outside 0 to %d", f.name, k, n, f.units, f.ceil)
			}
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
	s.IntlPaymentFeeMinor = maps.Clone(s.IntlPaymentFeeMinor)
	return s
}
