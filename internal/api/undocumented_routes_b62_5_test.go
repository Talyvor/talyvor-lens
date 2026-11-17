package api

// B62.5: GET /v1/partner/agent-activity, /v1/partner/agent-activity/export and /v1/partner/agent-activity/summary, the
// account partner's read-only view of every agent payment. Read on a partner key only, never a tenant's credential, so
// they are not in the tenant API's published contract.
func init() { undocumentedRoutes["B62.5"] = 3 }
