package api

// B41.147: POST /v1/rooms/{roomID}/leaderboard/freeze, GET and POST /v1/rooms/{roomID}/objections and PUT
// /v1/rooms/{roomID}/objections/{objectionID}/vote — objections to a room's frozen leaderboard — undocumented, like
// the leaderboard, until room_equity is cleared.
func init() { undocumentedRoutes["B41.147"] = 4 }
