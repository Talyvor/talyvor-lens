package api

// B48.14: GET /v1/workspaces/{wsID}/agents/{agentID}/wallet/activity, /wallet/activity, /agents/{agentID}/wallet/statement,
// /wallet/statement and /wallet/tax — undocumented like the agent wallet routes they sit beside (B46.4), until the
// wallet screens (B48.18, B48.19) settle their shape.
func init() { undocumentedRoutes["B48.14"] = 5 }
