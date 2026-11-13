package api

// B55.15: DELETE /v1/workspaces/{wsID}/agents/passkeys/{credentialID} and GET /v1/workspaces/{wsID}/agents/keys —
// removing a passkey and listing every active key — undocumented like the passkey routes beside them.
func init() { undocumentedRoutes["B55.15"] = 2 }
