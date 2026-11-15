package phonesignin

import (
	"errors"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/economy"
)

func TestNormalisePhone(t *testing.T) {
	for in, want := range map[string]string{
		"+44 7700 900123":     "+447700900123",
		"0044 (7700) 900-123": "+447700900123",
		"+1 415.555.0100":     "+14155550100",
		"+373 69 123 456":     "+37369123456",
	} {
		if got, err := NormalisePhone(in); err != nil || got != want {
			t.Errorf("NormalisePhone(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "07700 900123", "+44", "+0123456789", "+44 7700 9001234567890", "+44 7700 abc"} {
		if _, err := NormalisePhone(in); !errors.Is(err, ErrPhone) {
			t.Errorf("NormalisePhone(%q) = %v, want ErrPhone", in, err)
		}
	}
	// A number in no country is refused in sign-up's own words for a country.
	_, err := NormalisePhone("+800 1234 5678")
	if !errors.Is(err, economy.ErrSignupProfile) || !strings.Contains(err.Error(), "the country is an ISO 3166-1 alpha-2 code, like GB") {
		t.Fatalf("an international freephone number: %v, want sign-up's country refusal", err)
	}
}
