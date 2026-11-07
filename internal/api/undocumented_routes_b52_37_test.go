package api

// B52.37: POST /v1/account/delete — called by the suite BFF on the provision secret, like POST /v1/provision,
// not by a workspace; undocumented like it.
func init() { undocumentedRoutes["B52.37"] = 1 }
