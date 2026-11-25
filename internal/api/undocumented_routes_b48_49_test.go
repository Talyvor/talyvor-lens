package api

// B48.49: POST /v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/challenge and …/crypto-sends/{sendID}/approve/challenge
// — the challenges the owner signs their own send and their yes to the agent's over — and GET and POST /v1/workspaces/{wsID}/crypto-addresses and DELETE …/crypto-addresses/{addressID}, the
// owner's saved crypto addresses, undocumented until the app's send screen settles their shape.
func init() { undocumentedRoutes["B48.49"] = 5 }
