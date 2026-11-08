package api

// B41.152: GET and POST /v1/rooms/{roomID}/tasks and POST and DELETE /v1/rooms/{roomID}/tasks/{taskID}/claim — a
// room's open tasks board — undocumented, like the rate card, until room_equity is cleared.
func init() { undocumentedRoutes["B41.152"] = 4 }
