package api

func init() {
	// B61.24: POST /v1/auth/passkey/challenge, /finish, /register/challenge, /register, /keys and /keys/remove, a
	// person's sign-in keys — for the suite BFF alone on the provision secret, undocumented as /v1/provision beside
	// them is.
	undocumentedRoutes["B61.24"] = 6
}
