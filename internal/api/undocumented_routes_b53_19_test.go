package api

// +5 (B53.19): GET and POST /v1/money/physical-cards, POST /v1/money/physical-cards/{id}/activate and /replace, and the
// synthetic-key POST /v1/synthetic/workspaces/{wsID}/money/physical-cards/{cardID}/delivery — undocumented while
// LENS_PHYSICAL_CARDS stays off until a live card partner serves cards.
func init() { undocumentedRoutes["B53.19"] = 5 }
