package api

func init() {
	// B59.24: GET and POST /v1/money/card-disputes, POST /v1/money/card-disputes/{id}/sandbox-outcome, GET and POST
	// /v1/money/card-refunds and GET /v1/money/card-exports — card disputes, refunds and the card export — undocumented
	// like the money card routes beside them.
	undocumentedRoutes["B59.24"] = 6
}
