package api

func init() {
	// B28.338: GET and POST /v1/money/alerts/channels, DELETE /v1/money/alerts/channels/{id} and
	// GET /v1/money/alerts/deliveries — where the owner's alerts go, read by the BFF alone.
	undocumentedRoutes["B28.338"] = 4
}
