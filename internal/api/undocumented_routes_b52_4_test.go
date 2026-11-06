package api

// B52.4: POST /v1/auth/connect-token — the BFF mints an assistant's Connect token with the mint credential —
// undocumented like /v1/auth/token beside it, which no tenant calls either.
func init() { undocumentedRoutes["B52.4"] = 1 }
