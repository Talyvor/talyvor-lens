package api

// B48.7: POST /v1/money/payees/{id}/address-proof and POST /v1/money/payout-details/address-proof — the owner proves
// they hold a self-hosted address's key — undocumented like the payee and payout-details routes beside them.
func init() { undocumentedRoutes["B48.7"] = 2 }
