package api

// B52.11: POST /v1/negotiations/{id}/plan, POST …/{id}/move and POST …/{id}/replies — Counter's deal engine: its plan,
// its next move with a counterparty and a counterparty's reply read into terms — undocumented until Counter in the app
// (B52.15) settles their shape.
func init() { undocumentedRoutes["B52.11"] = 3 }
