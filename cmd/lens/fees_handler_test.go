package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/talyvor/lens/internal/billing"
)

// B32.8 — GET /v1/public/fees answers every fee Nicolai decided, with his values.
func TestPublicFees_EveryFeeWithNicolaisValues(t *testing.T) {
	w := httptest.NewRecorder()
	publicFeesHandler(w, httptest.NewRequest(http.MethodGet, "/v1/public/fees", nil))
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); w.Code != http.StatusOK || err != nil {
		t.Fatalf("GET /v1/public/fees = %d %s (%v)", w.Code, w.Body, err)
	}
	want := map[string]any{
		"market_take_bps":        1500.0,
		"services_take_bps":      500.0,
		"compute_take_bps":       500.0,
		"lending_fee_bps":        100.0,
		"platform_fee_bps":       map[string]any{"free": 550.0, "team": 300.0, "business": 100.0, "enterprise": 100.0},
		"fx_margin_bps":          map[string]any{"free": 0.0, "team": 50.0, "business": 25.0, "enterprise": 15.0},
		"intl_payment_fee_minor": map[string]any{"GBP": 500.0, "EUR": 600.0, "USD": 700.0},
		"merchant_fee_bps":       75.0,
		"merchant_a2a_fee_bps":   100.0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GET /v1/public/fees = %v\nwant %v", got, want)
	}
}

// B51.2 — GET /v1/pricing says whether a card can be charged, in each of the three states, and never any of the key.
func TestPublicPricing_CardPayments(t *testing.T) {
	for _, c := range []struct {
		enabled bool
		key     string
		want    string
	}{
		{true, "sk_live_b512secretpart", "live"},
		{true, "rk_test_b512secretpart", "test"},
		{false, "sk_test_b512secretpart", "off"},
	} {
		w := httptest.NewRecorder()
		publicPricingHandler(billing.CardPayments(c.enabled, c.key))(w, httptest.NewRequest(http.MethodGet, "/v1/pricing", nil))
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); w.Code != http.StatusOK || err != nil {
			t.Fatalf("GET /v1/pricing = %d %s (%v)", w.Code, w.Body, err)
		}
		if want := map[string]any{"card_payments": c.want}; !reflect.DeepEqual(got, want) {
			t.Errorf("billing %v, key %.8s…: GET /v1/pricing = %v, want %v", c.enabled, c.key, got, want)
		}
	}
}
