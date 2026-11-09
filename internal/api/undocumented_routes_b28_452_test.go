package api

func init() {
	// B28.452: POST and GET /v1/workspaces/{wsID}/chat-shares, DELETE …/chat-shares/{id} and the signed-out
	// GET /v1/public/chat-shares/{token} — Chat's share links, read by the BFF alone.
	undocumentedRoutes["B28.452"] = 4
}
