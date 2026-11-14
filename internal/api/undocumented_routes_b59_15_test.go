package api

func init() {
	// B59.15: POST /v1/workspaces/{wsID}/agents/{agentID}/money/instant-transfers (the send) and GET
	// /v1/workspaces/{wsID}/money/instant-transfers/{id} (its receipt) — undocumented, as the quote beside them is, until
	// the screens that send one (B59.19, B59.20) document the three together.
	undocumentedRoutes["B59.15"] = 2
}
