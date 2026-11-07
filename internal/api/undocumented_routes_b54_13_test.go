package api

// B54.13: POST /v1/auth/connect-revocations — the BFF records that a person ended an assistant's connection — undocumented
// like /v1/auth/connect-token beside it, which no tenant calls either.
func init() { undocumentedRoutes["B54.13"] = 1 }
