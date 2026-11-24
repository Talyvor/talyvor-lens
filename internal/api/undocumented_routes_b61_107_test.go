package api

func init() {
	// B61.107: POST /v1/auth/passcode/replace, a forgotten passcode set again with the number's code and a key — for
	// the suite BFF alone on the provision secret, undocumented as the phone door beside it is.
	undocumentedRoutes["B61.107"] = 1
}
