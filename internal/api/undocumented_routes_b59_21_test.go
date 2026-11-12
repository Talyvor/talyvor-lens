package api

func init() {
	// B59.21: POST and GET /v1/money/card-spends and GET /v1/money/card-spends/{id} — an agent asks before it pays by
	// card on a preapproved card programme — undocumented like the money card routes beside them.
	undocumentedRoutes["B59.21"] = 3
}
