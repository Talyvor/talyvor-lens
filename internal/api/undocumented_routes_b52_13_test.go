package api

// B52.13: POST /v1/negotiations/{id}/close, …/deal/approval, …/deal/approve, …/deal/decline and …/deal/pay — Counter closing
// inside the mandate and the owner signing the rest — undocumented until Counter in the app (B52.15) settles their shape.
func init() { undocumentedRoutes["B52.13"] = 5 }
