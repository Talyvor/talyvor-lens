package api

// B56.23: POST /v1/workspaces/{wsID}/agents/{agentID}/money/convert/quote and …/money/convert — an agent's currency
// conversions at its plan's FX margin, undocumented as the rest of the money routes are.
func init() { undocumentedRoutes["B56.23"] = 2 }
