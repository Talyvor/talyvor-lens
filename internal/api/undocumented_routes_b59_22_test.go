package api

func init() {
	// B59.22: GET and POST /v1/money/card-challenges — a workspace's 3-D Secure challenges, and the Sandbox challenging
	// a payment on one of its agents' cards — undocumented like the money card routes beside them.
	undocumentedRoutes["B59.22"] = 2
}
