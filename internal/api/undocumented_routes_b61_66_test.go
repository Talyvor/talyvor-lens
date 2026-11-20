package api

// B61.66: POST /v1/money/accounts/{id}/funding — the owner or an admin moves money now between the company's account
// and an agent's in its currency. It sits beside /v1/money/accounts/{id}/test-money and the rest of
// /v1/money/accounts, which are outside the contract too.
func init() {
	undocumentedRoutes["B61.66"] = 1
}
