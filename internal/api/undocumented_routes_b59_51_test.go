package api

func init() {
	// B59.51: the owner's Wallets — POST /v1/money/accounts/{id}/withdraw and its /challenge, to the owner's own bank,
	// and POST /v1/money/convert/quote, /v1/money/convert/challenge and /v1/money/convert, between the two sides —
	// undocumented, as B56.23's conversion and B59.14's Instant transfer quote beside them are.
	undocumentedRoutes["B59.51"] = 5
}
