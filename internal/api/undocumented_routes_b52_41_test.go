package api

// B52.41: POST /v1/workspaces/{wsID}/agents/{agentID}/rules/proposals, …/rules/proposals/{proposalID}/challenge and
// …/rules/proposals/{proposalID}/approve — a raise filed for the owner's passkey, beside …/rules/challenge, which is
// undocumented too.
func init() { undocumentedRoutes["B52.41"] = 3 }
