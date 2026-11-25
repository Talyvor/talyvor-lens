package api

// B48.43: GET /v1/workspaces/{wsID}/agents/{agentID}/wallet-moves/{moveID} — one top-up or cash-out by id, for the
// owner's app to show the top-up an agent asked for; undocumented like the B48.8 routes it sits beside.
func init() { undocumentedRoutes["B48.43"] = 1 }
