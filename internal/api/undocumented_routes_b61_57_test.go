package api

func init() {
	// B61.57: GET /v1/public/plans, both plan ladders with their prices, beside /v1/public/fees and
	// /v1/public/plan-gates, which are undocumented too; and PUT /v1/workspaces/{wsID}/plan, the owner's plan change,
	// beside the GET on the same path, also undocumented.
	undocumentedRoutes["B61.57"] = 2
}
