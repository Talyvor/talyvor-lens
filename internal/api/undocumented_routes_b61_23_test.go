package api

func init() {
	// B61.23: POST /v1/auth/passcode/check and /change, the passcode for a session of the person — for the suite BFF
	// alone on the provision secret, undocumented as the phone door beside them is.
	undocumentedRoutes["B61.23"] = 2
}
