package api

// B41.148: POST /v1/rooms/{roomID}/owner, GET/POST/DELETE /v1/rooms/{roomID}/takeover, PUT .../takeover/vote and
// POST /v1/rooms/{roomID}/fork — a room's owner changes by hand-over or a members' vote — undocumented like the rest of
// the rooms routes.
func init() { undocumentedRoutes["B41.148"] = 6 }
