package api

func init() {
	// B61.104: POST /v1/auth/phone/change-number, a person changing their own number with the new number's code and
	// their passcode — for the suite BFF alone on the provision secret, undocumented as the phone door beside it is.
	undocumentedRoutes["B61.104"] = 1
}
