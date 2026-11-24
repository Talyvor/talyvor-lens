package api

func init() {
	// B30.24: POST /v1/money/accounts/{id}/freeze, /unfreeze/challenge, /unfreeze, /close/challenge and /close — the owner's freeze,
	// unfreeze and close of a currency account, drawn by the Money area (B30.95).
	undocumentedRoutes["B30.24"] = 5
}
