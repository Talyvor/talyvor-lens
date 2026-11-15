package api

func init() {
	// B61.22: POST /v1/auth/phone/start, /finish and /person, the front door by phone number — for the suite BFF
	// alone on the provision secret, undocumented as /v1/provision beside them is.
	undocumentedRoutes["B61.22"] = 3
}
