package api

func init() {
	// B52.45: POST /v1/provision/invites/preview, the BFF's on the provision secret, as /v1/provision/invites/accept is
	// undocumented.
	undocumentedRoutes["B52.45"] = 1
}
