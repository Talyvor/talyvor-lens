package api

// B17.584: POST /v1/synthetic/workspaces/{wsID}/role-session — a tester signs a test workspace's role person in,
// undocumented beside the other synthetic-key routes, registered only when LENS_SYNTHETIC_KEY is set.
func init() { undocumentedRoutes["B17.584"] = 1 }
