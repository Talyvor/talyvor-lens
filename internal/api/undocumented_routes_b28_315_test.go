package api

func init() {
	// B28.315: GET and PUT /v1/workspaces/{wsID}/second-approver and POST .../second-approver/challenge, a Business's
	// second-approver line — undocumented, as the approval routes it decides beside are.
	undocumentedRoutes["B28.315"] = 3
}
