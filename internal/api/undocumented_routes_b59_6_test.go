package api

func init() {
	// B59.6: GET /v1/workspaces/{wsID}/account, the workspace's type and kind of money, and PUT
	// /v1/workspaces/{wsID}/account/money-kind, its change as launch pack 3.5 allows — undocumented, as B48.1's agent
	// wallet-kind routes beside them are.
	undocumentedRoutes["B59.6"] = 2
}
