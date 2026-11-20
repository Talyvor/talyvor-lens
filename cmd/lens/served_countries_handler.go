package main

import (
	"net/http"

	"github.com/talyvor/lens/internal/economy"
)

// B61.53 — the countries Talyvor serves (internal/economy/served_countries.go), for the web's and the phone's sign-up
// and identity check, so the apps offer exactly the countries Lens takes.
//
//	GET /v1/public/countries   {countries: ["AE", "AT", …]}, ISO 3166-1 alpha-2 codes in code order
func publicCountriesHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSONOK(w, http.StatusOK, map[string]any{"countries": economy.ServedCountries()})
}

// writeCountryNotServed answers a sign-up or an identity check in a country Talyvor does not serve.
func writeCountryNotServed(w http.ResponseWriter) {
	writeJSONOK(w, http.StatusBadRequest, map[string]any{"error": economy.CountryNotServedSentence,
		"code": economy.CountryNotServedCode, "field": "country"})
}
