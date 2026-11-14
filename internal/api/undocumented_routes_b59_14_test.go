package api

func init() {
	// B59.14: POST /v1/workspaces/{wsID}/money/instant-transfers/quote, the Instant transfer quote locked for 30
	// seconds — undocumented, as B56.23's conversion quote beside it is, until its send (B59.15) joins it.
	undocumentedRoutes["B59.14"] = 1
}
