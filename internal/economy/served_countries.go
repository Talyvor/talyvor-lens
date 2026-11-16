package economy

import (
	"errors"
	"slices"
	"strings"
)

// served_countries.go — B61.53: THE COUNTRIES TALYVOR SERVES, AND THE ONLY ONES AN ACCOUNT IS OPENED OR CHECKED IN.
//
// Sign-up (the provision call's profile) and the identity check (VerificationRequest's country and its home address's)
// refuse any other with ErrCountryNotServed, which the routes answer with CountryNotServedCode and
// CountryNotServedSentence. GET /v1/public/countries serves the list, so the web and the phone offer exactly these.
// The list is Mihai's to change. Russia and Belarus are refused by name whatever it holds (all three founders: no
// Russia or Belarus flows).
//
// An account made before B61.53 keeps its country: the check runs when an account is made and when a check starts,
// never on a sign-in to an account that already exists, and nothing already recorded is rewritten.

// servedCountries started as the 44 the apps offered at sign-up on 15 Nov 2026, as ISO 3166-1 alpha-2 codes.
var servedCountries = []string{
	"AE", "AT", "AU", "BE", "BG", "BR", "CA", "CH", "CY", "CZ", "DE", "DK", "EE", "ES", "FI", "FR", "GB", "GR", "HR",
	"HU", "IE", "IL", "IN", "IS", "IT", "JP", "LI", "LT", "LU", "LV", "MT", "MX", "NL", "NO", "NZ", "PL", "PT", "RO",
	"SE", "SG", "SI", "SK", "US", "ZA",
}

// neverServed are refused even when servedCountries names them.
var neverServed = []string{"BY", "RU"}

const (
	// CountryNotServedCode is the code a refused country is answered with, for the apps to read.
	CountryNotServedCode = "country_not_served"
	// CountryNotServedSentence is the sentence a refused country is answered with.
	CountryNotServedSentence = "Talyvor is not available in that country yet."
)

// ErrCountryNotServed: a country Talyvor does not serve, at sign-up or at the identity check.
var ErrCountryNotServed = errors.New("economy: " + CountryNotServedSentence)

// ServedCountries is every country Talyvor serves, in code order.
func ServedCountries() []string {
	out := []string{}
	for _, c := range servedCountries {
		if !slices.Contains(neverServed, c) {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return out
}

// CheckServedCountry refuses a country Talyvor does not serve. An empty one names no country and passes; the caller
// checks the code is one first.
func CheckServedCountry(country string) error {
	country = strings.ToUpper(strings.TrimSpace(country))
	if country == "" {
		return nil
	}
	if !slices.Contains(ServedCountries(), country) {
		return ErrCountryNotServed
	}
	return nil
}
