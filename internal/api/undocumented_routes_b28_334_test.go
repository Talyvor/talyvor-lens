package api

// B28.334: GET and PUT /v1/money/low-balance, DELETE /v1/money/low-balance/{id} and GET /v1/money/low-balance/alerts —
// an agent's low-balance threshold and the alerts it raises, beside B53.4's budgets, which are outside the contract too.
func init() {
	undocumentedRoutes["B28.334"] = 4
}
