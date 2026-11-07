package api

// B52.40: POST /v1/synthetic/workspaces/{wsID}/scan/reports, …/scan/reports/{id}/approve and …/{id}/outcome, and GET
// …/scan/track-record — a Scan report rehearsed on a test workspace for the nightly, on the synthetic key, undocumented
// like every synthetic route.
func init() { undocumentedRoutes["B52.40"] = 4 }
