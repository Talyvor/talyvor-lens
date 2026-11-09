package api

// B41.142: GET and PUT /v1/rooms/{roomID}/rate-card and PUT /v1/rooms/{roomID}/contributions/{contributionID}/size —
// a room's rate card and a contribution's claimed size — undocumented, like shared ownership, until room_equity is cleared.
func init() { undocumentedRoutes["B41.142"] = 3 }
