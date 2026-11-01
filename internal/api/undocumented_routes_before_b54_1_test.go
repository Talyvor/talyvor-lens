package api

// The undocumented routes items added before B54.1, each on its own entry. A new item does not edit this file:
// it adds internal/api/undocumented_routes_<item>_test.go of its own (see undocumentedRoutes).
func init() {
	// 272 → 276 (B30.19): POST and GET /v1/money/mandates, POST /v1/money/mandates/{id}/revoke and /pull — counted on a
	// line of their own, so a branch adding routes beside another's adds a line rather than rewriting the other's.
	undocumentedRoutes["B30.19"] = 4

	// 276 → 283 (B30.20): POST and GET /v1/money/invoices, POST /v1/money/invoices/{id}/send and /void, and the pay link's
	// GET /v1/pay/{token}, POST /v1/pay/{token}/card and /agent — on a line of their own, as B30.19's are.
	undocumentedRoutes["B30.20"] = 7

	// 283 → 287 (B30.21): POST and GET /v1/money/payouts, GET /v1/money/payouts/{id} and POST .../{id}/approve — on a line
	// of their own, as B30.19's and B30.20's are.
	undocumentedRoutes["B30.21"] = 4

	// 292 → 293 (B32.99): POST /v1/synthetic/workspaces/{wsID}/marketplace/payouts/run — on a line of its own, as B30.19's are.
	undocumentedRoutes["B32.99"] = 1

	// 293 → 294 (B32.102): POST /v1/synthetic/workspaces/{wsID}/card-link — on a line of its own, as B30.19's are.
	undocumentedRoutes["B32.102"] = 1

	// 294 → 297 (B28.125): POST /v1/workspaces/{wsID}/agents/{agentID}/prompt-schedules, GET …/agents/prompt-schedules and
	// DELETE …/agents/prompt-schedules/{scheduleID} — on a line of their own, as B30.19's are.
	undocumentedRoutes["B28.125"] = 3

	// 297 → 298 (B17.189): GET /v1/workspaces/{wsID}/marketplace/invoices — on a line of its own, as B30.19's are.
	undocumentedRoutes["B17.189"] = 1

	// 298 → 303 (B40.2): POST /v1/money/permission-mandates/prepare, POST and GET /v1/money/permission-mandates, and POST
	// …/{id}/revoke and …/{id}/evaluate — on a line of their own, as B30.19's are.
	undocumentedRoutes["B40.2"] = 5

	// 303 → 306 (B40.3): POST /v1/money/payment-requests, GET …/{id} and POST …/{id}/cancel — on a line of their own.
	undocumentedRoutes["B40.3"] = 3

	// 306 → 310 (B40.4): POST /v1/money/devices/challenge, POST and GET /v1/money/devices and POST …/{id}/revoke, and
	// POST …/{id}/push-token (B40.15) — on a line of their own.
	undocumentedRoutes["B40.4"] = 5

	// 310 → 314 (B40.5): POST /v1/money/payment-requests/{id}/approval, …/approve and …/pay, and GET …/record — on a line
	// of their own.
	undocumentedRoutes["B40.5"] = 4

	// 314 → 315 (B44.6): POST /v1/waitlist, the BFF's waitlist write on the provision secret — on a line of its own.
	undocumentedRoutes["B44.6"] = 1

	// 316 → 317 (B40.38): GET /v1/money/payment-requests?status=pending_approval — on a line of its own.
	undocumentedRoutes["B40.38"] = 1

	// 317 → 321 (B45.23): GET /v1/a2a/agents/{agentID}/agent-card.json and …/jwks.json, POST /v1/a2a/verify, and POST
	// /v1/workspaces/{wsID}/agents/{id}/agent-card/rotate — on a line of their own.
	undocumentedRoutes["B45.23"] = 4

	// B41.101: POST and GET /v1/money/bills and GET /v1/money/bills/{id} — on a line of their own, as B30.19's are.
	undocumentedRoutes["B41.101"] = 3

	// B41.102: POST /v1/money/bills/pay — on a line of its own, as B41.101's are.
	undocumentedRoutes["B41.102"] = 1

	// B46.3: POST /v1/money/webhooks/{partner}, each money partner's signed webhooks — on a line of its own.
	undocumentedRoutes["B46.3"] = 1

	// 321 → 323 (B45.24): GET /v1/a2a/signature-check and POST /v1/a2a/signature-check/money — on a line of their own.
	undocumentedRoutes["B45.24"] = 2

	// B17.255: POST /v1/money/accounts/{id}/test-money, the owner's test money into a company account — on a line of its own.
	undocumentedRoutes["B17.255"] = 1

	// 332 (B48.1): GET …/agents/{agentID}/wallets, PUT …/agents/{agentID}/wallet-kind and GET …/agents/owner-wallets — an
	// agent's wallets of each kind it is, and its owner's — on a line of their own.
	undocumentedRoutes["B48.1"] = 3

	// 4 (B46.11): PUT and GET /v1/money/payout-details, GET /v1/money/earnings-payouts and POST /v1/rooms/{roomID}/payouts —
	// earnings paid out on the money layer.
	undocumentedRoutes["B46.11"] = 4

	// B46.10: POST and GET /v1/money/cards, GET /v1/money/cards/{id} and POST …/{id}/approve — on a line of their own.
	undocumentedRoutes["B46.10"] = 4

	// B51.2: GET /v1/pricing, whether a card can be charged — on a line of its own.
	undocumentedRoutes["B51.2"] = 1

	// B41.105: GET /v1/workspaces/{wsID}/agents/safe-to-spend — on a line of its own.
	undocumentedRoutes["B41.105"] = 1

	// B41.107: GET /v1/money/search — on a line of its own.
	undocumentedRoutes["B41.107"] = 1

	// B41.111: POST /v1/money/payees/{id}/called/challenge and POST /v1/money/payees/{id}/called — on a line of their own.
	undocumentedRoutes["B41.111"] = 2

	// B48.2: POST and GET …/agents/{agentID}/smart-account — a crypto agent's own smart account, owned by the person — and
	// B50.25: POST …/smart-account/challenge, the challenge its owner key signs — on a
	// line of their own.
	undocumentedRoutes["B48.2"] = 3

	// B48.3: POST …/spend-permission/prepare, PUT and GET …/spend-permission and POST …/freeze — the owner's limits on a
	// crypto agent's key and an instant freeze — on a line of their own.
	undocumentedRoutes["B48.3"] = 4
}
