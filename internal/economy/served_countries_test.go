package economy

import (
	"errors"
	"slices"
	"testing"
)

// B61.53 — the identity check refuses Russia, Belarus and a country outside the list Talyvor serves, and still takes
// a listed one; a home address in such a country is refused the same way.
func TestB6153_TheIdentityCheckRefusesACountryTalyvorDoesNotServe(t *testing.T) {
	for _, country := range []string{"RU", "by", "KZ"} {
		for _, level := range []VerificationLevel{LevelIdentity, LevelCompany} {
			in := VerificationRequest{Level: level, Name: "Ada Lovelace", Country: country, DateOfBirth: "1990-12-10",
				CompanyNumber: "17299143", Directors: []string{"Ada Lovelace"}}
			if err := checkVerificationRequest(&in); !errors.Is(err, ErrCountryNotServed) || !errors.Is(err, ErrVerificationInvalid) {
				t.Errorf("a %s check in %s = %v; want ErrCountryNotServed", level, country, err)
			}
		}
		in := VerificationRequest{Level: LevelIdentity, Name: "Ada Lovelace", Country: "GB", DateOfBirth: "1990-12-10",
			Address: &HomeAddress{Lines: []string{"1 Main St"}, Town: "Town", Postcode: "1", Country: country}}
		if err := checkVerificationRequest(&in); !errors.Is(err, ErrCountryNotServed) {
			t.Errorf("an identity check with a home address in %s = %v; want ErrCountryNotServed", country, err)
		}
	}
	in := VerificationRequest{Level: LevelIdentity, Name: "Ada Lovelace", Country: "gb", DateOfBirth: "1990-12-10"}
	if err := checkVerificationRequest(&in); err != nil || in.Country != "GB" {
		t.Fatalf("an identity check in GB = %v (%q); want it to pass", err, in.Country)
	}
}

// Russia and Belarus are refused by name, even when the list is changed to hold them.
func TestB6153_RussiaAndBelarusAreRefusedWhateverTheListHolds(t *testing.T) {
	was := servedCountries
	t.Cleanup(func() { servedCountries = was })
	servedCountries = append(slices.Clone(was), "RU", "BY")
	for _, c := range []string{"RU", "BY"} {
		if err := CheckServedCountry(c); !errors.Is(err, ErrCountryNotServed) {
			t.Errorf("%s with the list holding it = %v; want ErrCountryNotServed", c, err)
		}
		if slices.Contains(ServedCountries(), c) {
			t.Errorf("the served list offers %s", c)
		}
	}
	if n := len(was); n != 44 {
		t.Fatalf("the list starts as the 44 countries the apps offered; it holds %d", n)
	}
}
