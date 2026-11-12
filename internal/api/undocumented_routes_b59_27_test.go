package api

func init() {
	// B59.27: POST and GET /v1/workspaces/{wsID}/requests and GET /v1/workspaces/{wsID}/requests/{id}/export —
	// support and complaints, closing the account and the workspace's data, filed and read by the web app and the
	// phone for the person signed in.
	undocumentedRoutes["B59.27"] = 3
}
