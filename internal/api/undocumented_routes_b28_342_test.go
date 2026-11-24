package api

func init() {
	// B28.342: GET, PUT and DELETE /v1/workspaces/{wsID}/roles, the owner's roles for the people in a workspace, and
	// POST /v1/provision/roles, a role session for the BFF on the provisioning secret beside /v1/provision.
	undocumentedRoutes["B28.342"] = 4
}
