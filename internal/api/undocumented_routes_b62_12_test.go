package api

// B62.12: POST /v1/synthetic/workspaces/{wsID}/agents/approvals/{approvalID}/payee — a tester re-points a test workspace's
// approved payment at another payee, undocumented beside the other synthetic-key routes, registered only when
// LENS_SYNTHETIC_KEY is set.
func init() { undocumentedRoutes["B62.12"] = 1 }
