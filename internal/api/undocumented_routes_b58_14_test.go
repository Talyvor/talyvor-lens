package api

func init() {
	// B58.14: GET /v1/pooling — whether cross-workspace pooling is on for this deployment and why
	// not, read by the nightly's pool-isolation check.
	undocumentedRoutes["B58.14"] = 1
}
