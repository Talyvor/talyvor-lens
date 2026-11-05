package api

// B41.204: POST /v1/workspaces/{wsID}/agents/approvals/challenge and …/approvals/decide — one passkey assertion over
// several approvals — undocumented like the single-approval challenge and decide routes beside them.
func init() { undocumentedRoutes["B41.204"] = 2 }
