package economy

import "testing"

// B54.7: a counterparty's subdomains are one counterparty; two addresses are never one.
func TestRegistrableDomain(t *testing.T) {
	for in, want := range map[string]string{
		"agent.acme.example": "acme.example", "Procurement.ACME.example.": "acme.example", "acme.co.uk": "acme.co.uk",
		"buy.acme.co.uk": "acme.co.uk", "10.0.0.1": "10.0.0.1", "10.9.0.1": "10.9.0.1", "[::1]": "[::1]",
	} {
		if got := RegistrableDomain(in); got != want {
			t.Errorf("RegistrableDomain(%q) = %q, want %q", in, got, want)
		}
	}
}
