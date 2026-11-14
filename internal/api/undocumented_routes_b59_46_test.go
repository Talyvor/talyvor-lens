package api

func init() {
	// B59.46: POST /v1/workspaces/{wsID}/account/money-kind/challenge, the challenge a change of the kind of money is
	// signed over — undocumented, as B59.6's PUT it signs for is.
	undocumentedRoutes["B59.46"] = 1
}
