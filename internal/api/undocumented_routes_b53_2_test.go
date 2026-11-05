package api

// +3 (B53.2): POST and GET /v1/money/reusable-cards and PATCH /v1/money/reusable-cards/{id} — undocumented while they
// stay unmounted (cmd/lens reusableCardsOn) until a live card issuer serves cards.
func init() { undocumentedRoutes["B53.2"] = 3 }
