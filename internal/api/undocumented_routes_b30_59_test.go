package api

// B30.59: PUT and GET /v1/money/accounts/{id}/treasury-sweep and POST /v1/money/accounts/{id}/treasury-sweep/withdraw,
// a company account's sweep into a money-market fund — undocumented, as the /v1/money/accounts routes are.
func init() { undocumentedRoutes["B30.59"] = 3 }
