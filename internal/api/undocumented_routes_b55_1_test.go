package api

// B55.1: GET /v1/workspaces/{wsID}/agents/keys/enrolments and …/{id}, and POST …/{id}/challenge, …/{id}/approve and
// …/{id}/decline — a new key let in from another device — undocumented like the passkey routes beside them.
func init() { undocumentedRoutes["B55.1"] = 5 }
