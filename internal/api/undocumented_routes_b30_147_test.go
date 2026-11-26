package api

// B30.147: GET /v1/money/payments/{id}/entry, a payment's ledger entry — undocumented beside the payments routes
// it reads, which are not in the published contract either.
func init() { undocumentedRoutes["B30.147"] = 1 }
