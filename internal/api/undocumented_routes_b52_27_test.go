package api

// B52.27: POST /v1/workspaces/{wsID}/agents/{agentID}/scan-watches, GET /v1/workspaces/{wsID}/scan-watches and
// DELETE /v1/workspaces/{wsID}/scan-watches/{watchID} — undocumented like the prompt schedules they sit beside, until
// Scan in the app (B52.28) settles their shape.
func init() { undocumentedRoutes["B52.27"] = 3 }
