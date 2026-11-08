package api

// B56.18: POST /v1/sso/sign-in — the single sign-on connection that covers an email, with its client secret. It is
// the suite BFF's alone, on LENS_PROVISION_SECRET like /v1/provision, so it is not in the tenant contract.
func init() { undocumentedRoutes["B56.18"] = 1 }
