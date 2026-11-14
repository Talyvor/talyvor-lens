package api

// B48.9: POST /v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/quote, POST and GET …/crypto-sends, GET
// …/crypto-sends/{sendID} and POST …/crypto-sends/{sendID}/approve and /decline (registered in one loop, parsed as one)
// — an agent's USDC sends from its smart account — undocumented until the app's send screen settles their shape.
func init() { undocumentedRoutes["B48.9"] = 5 }
