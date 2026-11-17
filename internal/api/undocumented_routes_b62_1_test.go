package api

// B62.1: GET /v1/workspaces/{wsID}/agents/payment-proofs/{id}, the record of one payment an agent's rules let through —
// what the owner was shown and signed, and what was sent. Undocumented like the approvals routes beside it.
func init() { undocumentedRoutes["B62.1"] = 1 }
