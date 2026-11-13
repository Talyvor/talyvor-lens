package api

// B54.8: POST /v1/inbound/negotiations/deal — a counterparty's DKIM-signed email confirming a negotiation's deal,
// undocumented like the agent's confirmation route beside it.
func init() { undocumentedRoutes["B54.8"] = 1 }
