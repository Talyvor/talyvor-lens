package api

// B54.45: POST /v1/synthetic/dkim-keys — a tester publishes a .test domain's DKIM key, undocumented beside the other
// synthetic-key routes, registered only when LENS_SYNTHETIC_KEY is set.
func init() { undocumentedRoutes["B54.45"] = 1 }
