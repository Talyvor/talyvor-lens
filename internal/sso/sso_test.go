package sso

import (
	"errors"
	"testing"
)

// B56.18 — the issuer the BFF will fetch names a public host: an address, localhost, a bare name or another port is
// refused before Lens reads the plan or the database.
func TestSaveRefusesAnIssuerThatIsNotAPublicHost(t *testing.T) {
	s := &Store{}
	for _, issuer := range []string{"http://idp.example.com", "https://10.0.0.1", "https://[::1]", "https://169.254.169.254",
		"https://localhost", "https://idp.localhost", "https://idp", "https://idp.example.com:8443", "https://u@idp.example.com"} {
		if _, err := s.Save(t.Context(), "ws", issuer, "client", "secret", []string{"example.com"}); !errors.Is(err, ErrInvalid) {
			t.Errorf("issuer %q: %v, want ErrInvalid", issuer, err)
		}
	}
}
