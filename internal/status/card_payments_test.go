package status

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/billing"
)

// B51.2 — /status.json says whether a card can be charged, in each of the three states, and never any of the key.
func TestStatusJSON_CardPayments(t *testing.T) {
	for _, c := range []struct {
		enabled bool
		key     string
		want    string
	}{
		{true, "sk_live_b512secretpart", "live"},
		{true, "sk_test_b512secretpart", "test"},
		{false, "sk_live_b512secretpart", "off"},
	} {
		page := newStatusPage(&fakePinger{}, nil, nil, "test")
		page.SetCardPayments(billing.CardPayments(c.enabled, c.key))
		page.UpdateCache(page.Check(context.Background()))
		rec := httptest.NewRecorder()
		page.ServeJSON(rec, httptest.NewRequest(http.MethodGet, "/status.json", nil))
		var got struct {
			CardPayments string `json:"card_payments"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.CardPayments != c.want {
			t.Errorf("billing %v, key %.8s…: card_payments %q (%v), want %q", c.enabled, c.key, got.CardPayments, err, c.want)
		}
		if strings.Contains(rec.Body.String(), "b512secretpart") {
			t.Errorf("/status.json carries part of the Stripe key")
		}
	}
}
