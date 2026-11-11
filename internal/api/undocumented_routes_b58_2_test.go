package api

func init() {
	// B58.2: POST /v1/workspaces/{wsID}/agents/{agentID}/pay is documented now, with its 202 for a payment held for
	// the owner, so it leaves the undocumented count.
	undocumentedRoutes["B58.2"] = -1
}
