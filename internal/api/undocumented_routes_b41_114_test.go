package api

// B41.114: GET and PUT /v1/money/assistant-limits, the daily limit on what each assistant may prepare — undocumented
// like the payment-request routes they guard, until the suite's limits screen (B41.14) settles their shape.
func init() { undocumentedRoutes["B41.114"] = 2 }
