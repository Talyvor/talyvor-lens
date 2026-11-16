package api

func init() {
	// B61.53: GET /v1/public/countries, the countries Talyvor serves, beside /v1/public/plans, which is undocumented too.
	undocumentedRoutes["B61.53"] = 1
}
