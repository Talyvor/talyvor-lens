package api

func init() {
	// B28.344: GET and POST /v1/money/event-webhooks, POST /v1/money/event-webhooks/{id}/rotate, DELETE /v1/money/event-webhooks/{id} and
	// GET /v1/money/event-webhooks/events — the owner's wallet-event webhooks, read by the BFF alone.
	undocumentedRoutes["B28.344"] = 5
}
