package api

// B55.48: the seven key recovery routes under /v1/workspaces/{wsID}/keys/ — a new recovery code and its challenge,
// recovering with the code, and starting, reading, cancelling and completing a recovery without it — undocumented like
// the passkey routes beside them.
func init() { undocumentedRoutes["B55.48"] = 7 }
