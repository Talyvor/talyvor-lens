package api

// B53.5: GET /v1/money/spending/unusual, GET /v1/money/spending/subscriptions — undocumented like B53.3's spending
// routes they sit beside, until the Spending screen (B53.8) settles their shape.
func init() { undocumentedRoutes["B53.5"] = 2 }
