package fees

import (
	"reflect"
	"testing"
)

// B32.8, B61.57 — every fee defaults to Nicolai's value, by the new plans where it is by plan, and a setting in
// lens.env replaces it.
func TestLoad_DefaultsAreNicolaisValuesAndASettingReplacesOne(t *testing.T) {
	s, err := Load(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{
		MarketTakeBPS: 1500, ServicesTakeBPS: 500, ComputeTakeBPS: 500, LendingFeeBPS: 100,
		PlatformFeeBPS: map[string]int64{"personal_solo": 550, "personal_crew": 300, "personal_fleet": 100,
			"business_studio": 550, "business_company": 300, "business_group": 100, "business_custom": 100},
		FXMarginBPS: map[string]int64{"personal_solo": 100, "personal_crew": 50, "personal_fleet": 0,
			"business_studio": 60, "business_company": 60, "business_group": 60, "business_custom": 15},
		IntlPaymentFeeMinor: map[string]int64{"GBP": 500, "EUR": 600, "USD": 700},
		MerchantFeeBPS:      75, MerchantA2AFeeBPS: 100,
	}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("defaults = %+v, want %+v", s, want)
	}

	fx := `{"personal_solo":90,"personal_crew":40,"personal_fleet":0,"business_studio":40,"business_company":40,"business_group":40,"business_custom":10}`
	env := map[string]string{"LENS_MARKET_TAKE_BPS": "1200", "LENS_FX_MARGIN_BPS": fx}
	s, err = Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if s.MarketTakeBPS != 1200 || s.FXMarginBPS["personal_solo"] != 90 || s.FXMarginBPS["business_company"] != 40 || s.ServicesTakeBPS != 500 {
		t.Fatalf("with settings: %+v", s)
	}

	for k, v := range map[string]string{
		"LENS_MARKET_TAKE_BPS":        "15%",
		"LENS_SERVICES_TAKE_BPS":      "10001",
		"LENS_PLATFORM_FEE_BPS":       `{"personal_solo":-1}`,
		"LENS_FX_MARGIN_BPS":          `{"free":0,"team":50,"business":25}`, // three of the 5 Oct plans; all four run on the default (B61.57)
		"LENS_INTL_PAYMENT_FEE_MINOR": `500`,
	} {
		if _, err := Load(func(n string) string {
			if n == k {
				return v
			}
			return ""
		}); err == nil {
			t.Errorf("%s=%s loaded; it must refuse to start", k, v)
		}
	}
}
