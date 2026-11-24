package api

// B48.8: POST /v1/workspaces/{wsID}/agents/{agentID}/wallet-moves/quote, POST …/wallet-moves/{moveID}/confirm, POST
// …/wallet-moves/{moveID}/return, GET …/{agentID}/wallet-moves and GET …/agents/owner-wallets/moves — undocumented like
// the B48.1 wallet routes they sit beside, until the wallet screen (B48.18) settles their shape.
func init() { undocumentedRoutes["B48.8"] = 5 }
