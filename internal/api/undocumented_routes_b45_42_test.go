package api

// B45.42: POST /v1/synthetic/workspaces/{wsID}/money/schedules/{scheduleID}/due — synthetic-key only, undocumented like
// B53.18's money card charge.
func init() { undocumentedRoutes["B45.42"] = 1 }
