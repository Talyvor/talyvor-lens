package api

// B54.7: POST /v1/negotiations/domains — the domain a workspace trades as in Counter's negotiations, verified by that
// domain's own agent — undocumented with the rest of Counter's routes until Counter in the app (B52.15) settles them.
func init() { undocumentedRoutes["B54.7"] = 1 }
