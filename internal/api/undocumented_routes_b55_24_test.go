package api

// B55.24: POST /v1/money/mandates/challenge, POST /v1/money/mandates/{id}/approve and /decline — undocumented, as the
// other /v1/money/mandates routes are.
func init() { undocumentedRoutes["B55.24"] = 3 }
