package api

// B62.6: POST /v1/kya/verify/tap and POST /v1/workspaces/{wsID}/agents/{id}/credential/tap — an agent's Know Your Agent
// credential in the form Visa's Trusted Agent Protocol publishes, and its check. docs/kya.md is their format, as it is
// for /v1/kya/verify and …/credential beside them, which are outside the contract too.
func init() {
	undocumentedRoutes["B62.6"] = 2
}
