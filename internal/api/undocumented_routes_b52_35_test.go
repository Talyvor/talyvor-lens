package api

func init() {
	// B52.35: POST, GET /v1/workspaces/{wsID}/member-invites, DELETE …/member-invites/{inviteID}, GET …/members and DELETE
	// …/members/{memberID}, the owner's; POST /v1/provision/invites/accept and /v1/provision/workspaces, the BFF's on the
	// provision secret, as /v1/provision itself is undocumented.
	undocumentedRoutes["B52.35"] = 7
}
