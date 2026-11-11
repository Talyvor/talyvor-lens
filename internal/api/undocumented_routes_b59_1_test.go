package api

func init() {
	// B59.1: GET /v1/workspaces/{wsID}/areas — the areas the workspace has, read by the web app and the phone
	// to show an area only once the operator has switched it on for the workspace.
	undocumentedRoutes["B59.1"] = 1
}
