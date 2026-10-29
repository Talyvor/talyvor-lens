package api

// openapi_wallets.go — the Agent Wallets half of the served OpenAPI document (B28.12): an agent, its
// balance and key, its rules, the approvals they file, its statement, its transfers and its card.
// The handlers are cmd/lens/agent_accounts_handler.go, agent_transfers_handler.go and
// agent_cards_handler.go; each path below is one they register.

// walletTag groups every wallet operation, and is listed first so a generated client and a rendered
// reference open on it.
const walletTag = "Agent Wallets"

// boostableAgentRules are the limits a boost may raise (B28.308): economy.BoostableRules().
var boostableAgentRules = []string{"approval_above_ulxc", "daily_limit_ulxc", "hourly_limit_ulxc", "max_per_request_ulxc",
	"monthly_limit_ulxc", "requests_per_minute", "weekly_limit_ulxc"}

func openAPIWalletTags() []map[string]any {
	return []map[string]any{{
		"name": walletTag,
		"description": "Every AI agent gets a wallet — a balance, spending rules, approvals, a card and a live statement. " +
			"The agent calls its models through Lens with a key of its own, and Lens judges each request and payment " +
			"against its wallet before the provider or the payee sees it. Amounts are in µLXC: " +
			"1 LXC = 1,000,000 µLXC = $0.10, so $1 is 10000000.",
	}}
}

func openAPIWalletSchemas() map[string]any {
	str := map[string]any{"type": "string"}
	ulxc := map[string]any{"type": "integer", "format": "int64", "description": "µLXC (1 LXC = 1,000,000 µLXC = $0.10)"}
	at := map[string]any{"type": "string", "format": "date-time"}
	strs := map[string]any{"type": "array", "items": str}
	return map[string]any{
		"Agent": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":            str,
				"name":          str,
				"balance_ulxc":  ulxc,
				"spent_ulxc":    ulxc,
				"pots_ulxc":     ulxc,
				"keys":          map[string]any{"type": "array", "items": str, "description": "ids of the agent's own proxy keys"},
				"created_at":    at,
				"paused_at":     map[string]any{"type": "string", "format": "date-time", "nullable": true},
				"paused_reason": str,
				"owner_user_id": map[string]any{"type": "string", "description": "the person answerable for what it spends"},
				"verified":      map[string]any{"type": "boolean"},
				"handle":        map[string]any{"type": "string", "description": "its @handle, once its owner picks one"},
				"description":   map[string]any{"type": "string", "description": "what the agent is for, in its owner's words"},
				"archived_at":   map[string]any{"type": "string", "format": "date-time", "nullable": true, "description": "set once the agent is archived: swept to zero, its keys revoked"},
			},
		},
		"AgentBook": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"workspace_balance_ulxc": ulxc,
				"allocated_ulxc":         ulxc,
				"unallocated_ulxc":       ulxc,
				"spent_ulxc":             ulxc,
				"agents":                 map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Agent"}},
				"all_paused_at":          map[string]any{"type": "string", "format": "date-time", "nullable": true},
				"all_paused_reason":      str,
			},
		},
		"AgentRules": map[string]any{
			"type":                 "object",
			"description":          "Zero or empty means no limit. A field this schema does not name is refused (400), so a misspelt rule is never silently no rule.",
			"additionalProperties": false,
			"properties": map[string]any{
				"max_per_request_ulxc": ulxc,
				"hourly_limit_ulxc":    map[string]any{"type": "integer", "format": "int64", "description": "the clock hour, in timezone; absent keeps the stored cap"},
				"daily_limit_ulxc":     ulxc,
				"weekly_limit_ulxc":    map[string]any{"type": "integer", "format": "int64", "description": "the week from Monday, in timezone; absent keeps the stored cap"},
				"monthly_limit_ulxc":   ulxc,
				"model_daily_limits_ulxc": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "integer", "format": "int64"},
					"description": "a model, named as the agent asks for it, to what the agent may spend on it in a day, in timezone; absent keeps the stored caps"},
				"payee_daily_limits_ulxc": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "integer", "format": "int64"},
					"description": "µLXC it may pay one payee in a day, by the payee's id as the payee lists name it (a company's counts its agents and listings); a payment past it is refused and posts nothing. Absent keeps the stored caps; saved, it replaces them whole"},
				"payee_monthly_limits_ulxc": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "integer", "format": "int64"},
					"description": "payee_daily_limits_ulxc over the month, in timezone. Absent keeps the stored caps; saved, it replaces them whole"},
				"payee_allowances": map[string]any{"type": "object", "additionalProperties": map[string]any{"$ref": "#/components/schemas/PayeeAllowance"},
					"description": "the owner's \"always allow\": by payee id, what the agent may pay that payee in a day or a month without approval; past it, a payment above approval_above_ulxc waits for approval again. Absent keeps the stored allowances; saved, it replaces them whole"},
				"requests_per_minute":    map[string]any{"type": "integer", "description": "the requests to models it may make in any sixty seconds; one more is refused 429 and holds nothing. Absent keeps the stored cap"},
				"approval_above_ulxc":    map[string]any{"type": "integer", "format": "int64", "description": "a request that could cost more waits for a person's approval"},
				"allowed_models":         strs,
				"allowed_providers":      strs,
				"allowed_listings":       map[string]any{"type": "array", "items": str, "description": "marketplace listings it may use; empty allows any"},
				"allowed_payees":         map[string]any{"type": "array", "items": str, "description": "the ids of the agents, listings, companies (a company names its agents and listings) and card merchants it may pay; once it names any, a payment to another is refused. Absent keeps the stored list"},
				"blocked_payees":         map[string]any{"type": "array", "items": str, "description": "the ids of the payees it may not pay; a payment to one is refused and posts nothing. Absent keeps the stored list"},
				"active_from":            map[string]any{"type": "string", "example": "09:00", "description": "HH:MM in timezone; with active_until, the hours it may spend"},
				"active_until":           map[string]any{"type": "string", "example": "18:00", "description": "exclusive; earlier than active_from crosses midnight"},
				"timezone":               map[string]any{"type": "string", "example": "Europe/Berlin", "description": "IANA name; UTC when empty"},
				"pause_on_unusual_spend": map[string]any{"type": "boolean"},
			},
		},
		"PayeeAllowance": map[string]any{"type": "object", "required": []string{"amount_ulxc", "per"}, "properties": map[string]any{
			"amount_ulxc": map[string]any{"type": "integer", "format": "int64", "minimum": 1},
			"per":         map[string]any{"type": "string", "enum": []string{"day", "month"}},
		}},
		"RuleTemplate": map[string]any{
			"type":        "object",
			"description": "Rules an agent can start from. They name every rule, so applying them replaces the agent's rules whole.",
			"properties": map[string]any{
				"id":      map[string]any{"type": "string", "example": "support-bot"},
				"name":    str,
				"summary": map[string]any{"type": "string", "description": "the agent it suits and what its rules do"},
				"rules":   map[string]any{"$ref": "#/components/schemas/AgentRules"},
			},
		},
		"AgentApprover": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":                str,
				"agent_id":          str,
				"approver_agent_id": str,
				"limit_ulxc":        ulxc,
				"mandate":           map[string]any{"type": "string", "description": "the owner's AP2 v0.2 open Payment Mandate, an SD-JWT"},
				"signed_by":         str,
				"created_at":        map[string]any{"type": "string", "format": "date-time"},
			},
		},
		"AgentApproval": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":                   str,
				"agent_id":             str,
				"amount_ulxc":          ulxc,
				"model":                str,
				"reason":               str,
				"approved_by_agent_id": map[string]any{"type": "string", "description": "the approver agent that approved it within its own limits (B45.28); absent when the owner did"},
				"payee": map[string]any{
					"type":     "object",
					"nullable": true,
					"properties": map[string]any{
						"kind": map[string]any{"type": "string", "enum": []string{"agent", "listing", "company", "merchant"}},
						"id":   str,
						"name": str,
					},
				},
				"memo":       str,
				"status":     map[string]any{"type": "string", "enum": []string{"pending", "approved", "denied", "used", "expired"}},
				"created_at": at,
				"decided_at": map[string]any{"type": "string", "format": "date-time", "nullable": true},
				"expires_at": map[string]any{"type": "string", "format": "date-time", "description": "When it stops being usable: 24 hours after it was filed while pending, 24 hours after the tap once approved."},
			},
		},
		"AgentRulesVersion": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"version":    map[string]any{"type": "integer", "minimum": 1},
				"rules":      map[string]any{"$ref": "#/components/schemas/AgentRules"},
				"changed_by": map[string]any{"type": "string", "description": "the credential that changed them: operator, or its method, user and key (jwt:user:…); empty when unknown"},
				"change":     map[string]any{"type": "string", "description": "set, template <id>, rollback to <n>, or before history for rules set before versions were kept"},
				"created_at": at,
			},
		},
		"AgentRuleBoost": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"rule":        map[string]any{"type": "string", "enum": boostableAgentRules, "description": "the limit raised, as AgentRules names it"},
				"raised_from": map[string]any{"type": "integer", "format": "int64", "minimum": 1, "description": "the limit as the rules set it when the boost was set; once the rules change it, the boost no longer applies"},
				"value":       map[string]any{"type": "integer", "format": "int64", "minimum": 1, "description": "what it is raised to: µLXC, or requests a minute for requests_per_minute"},
				"until":       map[string]any{"type": "string", "format": "date-time", "description": "from this time on the boost is not read and the limit is the rules' again"},
				"created_by":  map[string]any{"type": "string", "description": "the credential that set it; empty when unknown"},
				"created_at":  at,
			},
		},
		"AgentStatementLine": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"entry_id":           str,
				"kind":               map[string]any{"type": "string", "enum": []string{"fund", "withdraw", "spend", "hold", "settle", "release", "pay", "platform_fee"}},
				"amount_ulxc":        ulxc,
				"counterparty":       map[string]any{"type": "string", "description": "workspace | spend | agent:<id>"},
				"ref":                map[string]any{"type": "string", "description": "a payment's memo, or the request it paid for"},
				"balance_after_ulxc": ulxc,
				"at":                 at,
				"label":              map[string]any{"type": "string", "description": "on a platform_fee line, the fee and its rate: \"Platform fee 3%\""},
			},
		},
		"AgentTransfer": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":                str,
				"entry_id":          str,
				"from_workspace_id": str,
				"from_agent_id":     str,
				"to_workspace_id":   str,
				"to_agent_id":       str,
				"amount_ulxc":       ulxc,
				"memo":              str,
				"class":             str,
				"request_id":        str,
				"schedule_id":       str,
				"refund_of":         str,
				"created_at":        at,
			},
		},
	}
}

func openAPIWalletPaths() map[string]any {
	ref := func(schema string) map[string]any {
		return map[string]any{"$ref": "#/components/schemas/" + schema}
	}
	ok := func(desc string, schema map[string]any) map[string]any {
		return map[string]any{
			"description": desc,
			"content":     map[string]any{"application/json": map[string]any{"schema": schema}},
		}
	}
	body := func(schema map[string]any) map[string]any {
		return map[string]any{
			"required": true,
			"content":  map[string]any{"application/json": map[string]any{"schema": schema}},
		}
	}
	optional := func(schema map[string]any) map[string]any {
		b := body(schema)
		b["required"] = false
		return b
	}
	obj := func(required []string, props map[string]any) map[string]any {
		o := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			o["required"] = required
		}
		return o
	}
	listOf := func(field, schema string) map[string]any {
		return obj(nil, map[string]any{field: map[string]any{"type": "array", "items": ref(schema)}})
	}
	param := func(name, in string) map[string]any {
		return map[string]any{"name": name, "in": in, "required": in == "path", "schema": map[string]any{"type": "string"}}
	}
	ws, agent, approval := param("wsID", "path"), param("agentID", "path"), param("approvalID", "path")
	str := map[string]any{"type": "string"}
	ulxc := map[string]any{"type": "integer", "format": "int64", "minimum": 1}
	balance := ok("the agent's balance after the move", obj(nil, map[string]any{"agent_id": str, "balance_ulxc": ulxc}))
	idempotency := map[string]any{"name": "Idempotency-Key", "in": "header", "required": false,
		"description": "sent again with the same key, the move lands once (at most 128 characters)",
		"schema":      map[string]any{"type": "string", "maxLength": 128}}
	// op builds one wallet operation; every one carries the wallet tag.
	op := func(summary, description string, params []map[string]any, responses map[string]any) map[string]any {
		o := map[string]any{"tags": []string{walletTag}, "summary": summary, "parameters": params, "responses": responses}
		if description != "" {
			o["description"] = description
		}
		return o
	}
	withBody := func(o, b map[string]any) map[string]any {
		o["requestBody"] = b
		return o
	}
	periodParams := []map[string]any{
		ws,
		{"name": "from", "in": "query", "required": false, "description": "RFC 3339 time or YYYY-MM-DD (midnight UTC); defaults to the start of this month", "schema": str},
		{"name": "to", "in": "query", "required": false, "description": "exclusive; RFC 3339 time or YYYY-MM-DD; defaults to now", "schema": str},
		{"name": "format", "in": "query", "required": false, "schema": map[string]any{"type": "string", "enum": []string{"json", "csv"}}},
	}

	return map[string]any{
		"/v1/workspaces/{wsID}/agents": map[string]any{
			"get": op("List the workspace's agents and their wallets",
				"Each agent's balance and spend, reconciled with the workspace's own balance.",
				[]map[string]any{ws},
				map[string]any{"200": ok("the agents and the workspace's totals", ref("AgentBook"))}),
			"post": withBody(op("Create an agent",
				"Owner or admin. The signed-in person owns the agent; an admin credential names the owner in owner_user_id. With template, the agent starts with exactly that rule template's rules, and the answer carries template and rules too.",
				[]map[string]any{ws},
				map[string]any{"201": ok("the new agent, balance zero", ref("Agent")), "400": map[string]any{"description": "no name, no owner, or no such template"}}),
				body(obj([]string{"name"}, map[string]any{"name": str, "owner_user_id": str,
					"template": map[string]any{"type": "string", "example": "support-bot", "description": "the id of a rule template to start from"}}))),
		},
		"/v1/workspaces/{wsID}/agents/rule-templates": map[string]any{
			"get": op("List the rule templates an agent can start from",
				"Support bot, Researcher and Coder: each names every rule, so applying one replaces an agent's rules whole.",
				[]map[string]any{ws},
				map[string]any{"200": ok("the templates", listOf("templates", "RuleTemplate"))}),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/rules/template": map[string]any{
			"post": withBody(op("Apply a rule template to the agent",
				"Owner or admin. Replaces every one of the agent's rules with the template's: the agent then holds exactly the template, nothing kept from before.",
				[]map[string]any{ws, agent},
				map[string]any{"200": ok("the rules as stored", ref("AgentRules")), "400": map[string]any{"description": "no such template"}, "404": map[string]any{"description": "no such agent"}}),
				body(obj([]string{"template"}, map[string]any{"template": map[string]any{"type": "string", "example": "researcher"}}))),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/rules/simulate": map[string]any{
			"post": withBody(op("Ask whether the agent's rules would let a request through",
				"Moves nothing: the rules judge the request exactly as a real one, against what the agent has really spent, and everything the judgement would write is rolled back — no posting, no approval filed. With payee it is a payment to that payee; without, a question to model through provider. at (RFC 3339) asks about another time, for the active hours and the periods; now when absent. always_allow, with payee, judges the payment as though that allowance for its payee were saved: the preview of approving with always allow.",
				[]map[string]any{ws, agent},
				map[string]any{"200": ok("the rules' answer", obj([]string{"verdict", "reason", "amount_ulxc", "at", "balance_ulxc"}, map[string]any{
					"verdict":      map[string]any{"type": "string", "enum": []string{"allowed", "refused", "approval_required"}},
					"reason":       map[string]any{"type": "string", "description": "why it is refused or needs approval; empty when allowed"},
					"amount_ulxc":  map[string]any{"type": "integer", "format": "int64"},
					"at":           map[string]any{"type": "string", "format": "date-time"},
					"balance_ulxc": map[string]any{"type": "integer", "format": "int64", "description": "what the agent holds now, which the rules do not judge"},
				})), "400": map[string]any{"description": "an unknown field, a negative amount or a payee of no known kind"}, "404": map[string]any{"description": "no such agent"}}),
				body(obj([]string{"amount_ulxc"}, map[string]any{
					"amount_ulxc": map[string]any{"type": "integer", "format": "int64", "minimum": 0},
					"model":       str, "provider": str,
					"payee": obj([]string{"kind", "id"}, map[string]any{
						"kind": map[string]any{"type": "string", "enum": []string{"agent", "listing", "company", "merchant"}}, "id": str}),
					"at":           map[string]any{"type": "string", "format": "date-time"},
					"always_allow": ref("PayeeAllowance"),
				}))),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}": map[string]any{
			"patch": withBody(op("Rename or describe the agent",
				"Owner or admin. Either field or both; a name cannot be blank and a description is at most 500 characters.",
				[]map[string]any{ws, agent},
				map[string]any{"200": ok("the agent", ref("Agent")), "400": map[string]any{"description": "a blank name, a description too long, or an unknown field"}, "404": map[string]any{"description": "no such agent"}}),
				body(obj(nil, map[string]any{"name": str, "description": str}))),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/archive": map[string]any{
			"post": op("Archive the agent",
				"Owner or admin. In one step: the agent's whole balance goes back to the workspace as one withdraw, its proxy keys are revoked, its top-up and schedules stop. It stays listed, with archived_at, and its statement is kept; it can no longer be funded or move money of its own.",
				[]map[string]any{ws, agent},
				map[string]any{
					"200": ok("what archiving did", obj(nil, map[string]any{"agent_id": str, "swept_ulxc": map[string]any{"type": "integer", "format": "int64"},
						"revoked_keys": map[string]any{"type": "array", "items": str}, "archived_at": map[string]any{"type": "string", "format": "date-time"}})),
					"404": map[string]any{"description": "no such agent"},
					"409": map[string]any{"description": "already archived, or LXC is still kept in its pots"},
				}),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/keys": map[string]any{
			"post": withBody(op("Issue the agent a proxy key of its own",
				"Owner or admin. The agent calls its models through /v1/proxy/… with this key, and every request on it is judged against the agent's wallet. The raw key is shown once.",
				[]map[string]any{ws, agent},
				map[string]any{
					"201": ok("the key, shown once", obj(nil, map[string]any{"agent_id": str, "key": str, "id": str, "prefix": str, "warning": str})),
					"404": map[string]any{"description": "no such agent in this workspace"},
				}),
				optional(obj(nil, map[string]any{"name": map[string]any{"type": "string", "description": "defaults to the agent's id"}}))),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/fund": map[string]any{
			"post": withBody(op("Fund the agent from the workspace's credit",
				"Owner or admin.",
				[]map[string]any{ws, agent, idempotency},
				map[string]any{"200": balance, "404": map[string]any{"description": "no such agent"}, "409": map[string]any{"description": "the workspace cannot cover it, or the agent has no owner"}}),
				body(obj([]string{"amount_ulxc"}, map[string]any{"amount_ulxc": ulxc}))),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/withdraw": map[string]any{
			"post": withBody(op("Move credit from the agent back to the workspace",
				"Owner or admin.",
				[]map[string]any{ws, agent, idempotency},
				map[string]any{"200": balance, "404": map[string]any{"description": "no such agent"}, "409": map[string]any{"description": "the agent's balance cannot cover it"}}),
				body(obj([]string{"amount_ulxc"}, map[string]any{"amount_ulxc": ulxc}))),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/rules": map[string]any{
			"get": op("Read the agent's spending rules", "",
				[]map[string]any{ws, agent},
				map[string]any{"200": ok("the rules", ref("AgentRules")), "404": map[string]any{"description": "no such agent"}}),
			"put": withBody(op("Replace the agent's spending rules",
				"Owner or admin. Lens judges every model call, payment and card purchase of the agent against these before it happens: a request a rule refuses is answered 403 naming the rule and never reaches the provider.",
				[]map[string]any{ws, agent},
				map[string]any{"200": ok("the rules as stored", ref("AgentRules")), "400": map[string]any{"description": "an unknown field or an invalid rule"}, "404": map[string]any{"description": "no such agent"}}),
				body(ref("AgentRules"))),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/rules/history": map[string]any{
			"get": op("List the versions of the agent's rules",
				"Newest first: the first is the rules in force. Every change that alters the rules — a save, a template, a rollback — is a version, with who made it.",
				[]map[string]any{ws, agent},
				map[string]any{"200": ok("the versions", listOf("versions", "AgentRulesVersion")), "404": map[string]any{"description": "no such agent"}}),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/rules/rollback": map[string]any{
			"post": withBody(op("Roll the agent's rules back to an earlier version",
				"Owner or admin. The rules become exactly what they were at version — a rule added since keeps its value — and the rollback is itself a new version, recording who made it.",
				[]map[string]any{ws, agent},
				map[string]any{"200": ok("the rules now in force", ref("AgentRules")), "400": map[string]any{"description": "no version given"}, "404": map[string]any{"description": "no such agent, or no such version"}}),
				body(obj([]string{"version"}, map[string]any{"version": map[string]any{"type": "integer", "minimum": 1}}))),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/rules/boosts": map[string]any{
			"get": op("List the agent's temporary limit boosts in force",
				"The limits raised for now, the soonest to end first. A boost that has ended is not listed: the limit is the rules' again.",
				[]map[string]any{ws, agent},
				map[string]any{"200": ok("the boosts in force", listOf("boosts", "AgentRuleBoost")), "404": map[string]any{"description": "no such agent"}}),
			"post": withBody(op("Raise one of the agent's limits until a time",
				"Owner or admin. Until the time, the limit is the boost's value; from then on the rules judge it as the rules set it, with nothing to undo. The limit must be set and the value above it, and the boost applies only while the rules leave the limit as it was. A boost replaces the one the limit had.",
				[]map[string]any{ws, agent},
				map[string]any{"201": ok("the boost", ref("AgentRuleBoost")), "400": map[string]any{"description": "an unknown limit, a limit not set, a value not above it, or a time not in the future"}, "404": map[string]any{"description": "no such agent"}}),
				body(obj([]string{"rule", "value", "until"}, map[string]any{
					"rule":  map[string]any{"type": "string", "enum": boostableAgentRules},
					"value": map[string]any{"type": "integer", "format": "int64", "minimum": 1},
					"until": map[string]any{"type": "string", "format": "date-time"},
				}))),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/rules/boosts/{rule}": map[string]any{
			"delete": op("End a limit boost before its time",
				"Owner or admin. The limit is the rules' again at once.",
				[]map[string]any{ws, agent, param("rule", "path")},
				map[string]any{"204": map[string]any{"description": "ended"}, "404": map[string]any{"description": "no such agent, or no boost in force on that limit"}}),
		},
		"/v1/workspaces/{wsID}/agents/approvals": map[string]any{
			"get": op("List the requests the agents' rules sent to a person",
				"Newest first. A payment's approval names its payee and memo.",
				[]map[string]any{ws},
				map[string]any{"200": ok("the approvals", listOf("approvals", "AgentApproval"))}),
		},
		"/v1/workspaces/{wsID}/agents/approvals/{approvalID}/approve": map[string]any{
			"post": withBody(op("Approve a request, once — or approve it and always allow its payee",
				`Owner or admin. The agent's retry of the same request then goes through. Once the workspace has a passkey, the body carries {"assertion": {credential_id, client_data_json, authenticator_data, signature}} over the approval's challenge. With always_allow, a payment's approval also saves that allowance for its payee on the agent's rules (payee_allowances), and the answer adds the rules as saved.`,
				[]map[string]any{ws, approval},
				map[string]any{"200": ok("the approval, approved", ref("AgentApproval")), "400": map[string]any{"description": "always_allow without an amount above 0 and per day or month"},
					"403": map[string]any{"description": "a passkey assertion is required, or it is invalid"}, "404": map[string]any{"description": "no such approval"},
					"409": map[string]any{"description": "approved once, but always_allow was not saved: the approval names no payee"}}),
				body(obj(nil, map[string]any{"assertion": map[string]any{"type": "object"}, "always_allow": ref("PayeeAllowance")}))),
		},
		"/v1/workspaces/{wsID}/agents/approvals/{approvalID}/deny": map[string]any{
			"post": op("Deny a request",
				"Owner or admin; signed with a passkey the same way as approve.",
				[]map[string]any{ws, approval},
				map[string]any{"200": ok("the approval, denied", ref("AgentApproval")), "403": map[string]any{"description": "a passkey assertion is required, or it is invalid"}, "404": map[string]any{"description": "no such approval"}}),
		},
		"/v1/workspaces/{wsID}/agents/approvals/{approvalID}/evidence": map[string]any{
			"get": op("Read what proves an approval",
				"The owner's signed approval as an AP2 v0.2 closed Payment Mandate (an SD-JWT, owner_mandate), or an approver agent's approval as Talyvor-signed evidence naming both agents (agent_evidence, a JWS) with the open mandate it ran under (approver_mandate). Both verify with the key at jwks_url; docs/ap2-mandates.md is the format.",
				[]map[string]any{ws, approval},
				map[string]any{"200": ok("the evidence", obj(nil, map[string]any{"approval_id": str, "owner_mandate": str, "approved_by_agent_id": str,
					"agent_evidence": str, "approver_mandate": str, "jwks_url": str})), "404": map[string]any{"description": "no such approval"}}),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/approver/challenge": map[string]any{
			"post": withBody(op("Get the challenge to sign to let an agent approve this agent's requests",
				"Owner or admin. Also the limit the approver would approve up to: its own, the lower of its max_per_request_ulxc and approval_above_ulxc. An approver with neither may approve nothing.",
				[]map[string]any{ws, agent},
				map[string]any{"200": ok("the challenge", obj(nil, map[string]any{"challenge": str, "limit_ulxc": ulxc, "user_verification": str})),
					"400": map[string]any{"description": "the agent itself, an archived agent, or one with no limit of its own"}, "404": map[string]any{"description": "no such agent"}}),
				body(obj([]string{"approver_agent_id"}, map[string]any{"approver_agent_id": str}))),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/approver": map[string]any{
			"get": op("Read the agent that approves this agent's requests",
				"The approver in force, its limit and the owner's AP2 v0.2 open Payment Mandate that set it.",
				[]map[string]any{ws, agent},
				map[string]any{"200": ok("the approver", ref("AgentApprover")), "404": map[string]any{"description": "no approver: its requests go to the owner"}}),
			"put": withBody(op("Let an agent approve this agent's requests, within its own limits",
				`Owner or admin, signed on the Talyvor app: {"approver_agent_id", "assertion"} over the challenge (a passkey or a device key). Kept as an AP2 v0.2 open Payment Mandate bound to the approver's Agent Card key; it replaces the approver in force. A request above the approver's limits still goes to the owner.`,
				[]map[string]any{ws, agent},
				map[string]any{"201": ok("the approver", ref("AgentApprover")), "400": map[string]any{"description": "the agent itself, an archived agent, or one with no limit of its own"},
					"403": map[string]any{"description": "no signature, or one that does not verify"}, "404": map[string]any{"description": "no such agent"}}),
				body(obj([]string{"approver_agent_id", "assertion"}, map[string]any{"approver_agent_id": str, "assertion": map[string]any{"type": "object"}}))),
			"delete": op("Take the approver away",
				"Owner or admin, at once and unsigned. From then on the agent's requests go to the owner.",
				[]map[string]any{ws, agent},
				map[string]any{"204": map[string]any{"description": "removed"}, "404": map[string]any{"description": "no approver"}}),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/approvals/{approvalID}/approve": map[string]any{
			"post": op("Approve another agent's request, as its approver",
				"By the approver agent's own key only. Approved when the amount is within the lower of the approver's limit when the owner signed and its limit now, and within its daily limit over what it approved today; above them it is 409 and the request waits for the owner.",
				[]map[string]any{ws, agent, approval},
				map[string]any{"200": ok("the approval, approved", ref("AgentApproval")), "403": map[string]any{"description": "not the approver's own key, not the approver in force, or the approver is paused"},
					"404": map[string]any{"description": "no such pending approval"}, "409": map[string]any{"description": "above the approver's own limits: it waits for the owner"}}),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/statement": map[string]any{
			"get": op("Read the agent's statement",
				"With none of from, to or format: the agent's account, newest first (limit, default and at most 1000). With any of them: the statement for the period [from, to), as JSON or CSV.",
				append([]map[string]any{agent, {"name": "limit", "in": "query", "required": false, "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}}}, periodParams...),
				map[string]any{
					"200": ok("the agent's lines", obj(nil, map[string]any{"agent_id": str, "lines": map[string]any{"type": "array", "items": ref("AgentStatementLine")}})),
					"404": map[string]any{"description": "no such agent"},
				}),
		},
		"/v1/workspaces/{wsID}/agents/statement": map[string]any{
			"get": op("Read the statement of every agent wallet in the workspace",
				"For the period [from, to), as JSON or CSV — the statement an auditor can open.",
				periodParams,
				map[string]any{"200": map[string]any{"description": "the period statement"}}),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/pause": map[string]any{
			"post": withBody(op("Pause the agent",
				"Owner or admin. Every movement of the agent is refused until it is resumed, effective on its next request, buffered or streamed.",
				[]map[string]any{ws, agent},
				map[string]any{"200": map[string]any{"description": "paused"}}),
				optional(obj(nil, map[string]any{"reason": str}))),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/resume": map[string]any{
			"post": withBody(op("Resume a paused agent",
				"Owner or admin. Once the workspace has a passkey or device key, it takes an assertion over the resume challenge.",
				[]map[string]any{ws, agent},
				map[string]any{"200": map[string]any{"description": "resumed"}, "403": map[string]any{"description": "no assertion, or one Lens could not verify: the agent stays paused"}}),
				optional(obj(nil, map[string]any{"assertion": map[string]any{"type": "object"}}))),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/resume/challenge": map[string]any{
			"post": op("Get the challenge resuming the agent is signed over",
				"Owner or admin. Single use, for five minutes.",
				[]map[string]any{ws, agent},
				map[string]any{"200": map[string]any{"description": "the challenge and the passkeys that may sign it"}, "404": map[string]any{"description": "no such agent"}}),
		},
		"/v1/workspaces/{wsID}/agents/resume-all": map[string]any{
			"post": withBody(op("Resume every agent in the workspace",
				"Owner or admin. Lifts pause-all; an agent paused on its own stays paused. Once the workspace has a passkey or device key, it takes an assertion over the resume-all challenge.",
				[]map[string]any{ws},
				map[string]any{"200": map[string]any{"description": "resumed"}, "403": map[string]any{"description": "no assertion, or one Lens could not verify: every agent stays paused"}}),
				optional(obj(nil, map[string]any{"assertion": map[string]any{"type": "object"}}))),
		},
		"/v1/workspaces/{wsID}/agents/resume-all/challenge": map[string]any{
			"post": op("Get the challenge resuming every agent is signed over",
				"Owner or admin. Single use, for five minutes.",
				[]map[string]any{ws},
				map[string]any{"200": map[string]any{"description": "the challenge and the passkeys that may sign it"}}),
		},
		"/v1/workspaces/{wsID}/agents/pause-all": map[string]any{
			"post": withBody(op("Pause every agent in the workspace",
				"Owner or admin. Those created later included, until resume-all.",
				[]map[string]any{ws},
				map[string]any{"200": map[string]any{"description": "paused"}}),
				optional(obj(nil, map[string]any{"reason": str}))),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/send": map[string]any{
			"post": withBody(op("Send credits to any agent on Talyvor",
				"By the agent's own key, the workspace's owner or an admin. The agent's rules judge it like any other spend.",
				[]map[string]any{ws, agent},
				map[string]any{"200": ok("the transfer", ref("AgentTransfer")), "403": map[string]any{"description": "not the agent's key, the owner or an admin — or a rule refused it"}, "409": map[string]any{"description": "the agent's balance cannot cover it"}}),
				body(obj([]string{"to", "amount_ulxc"}, map[string]any{"to": map[string]any{"type": "string", "description": "a wallet id or @handle"}, "amount_ulxc": ulxc, "memo": str}))),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/transfers": map[string]any{
			"get": op("List what the agent sent and received", "",
				[]map[string]any{ws, agent},
				map[string]any{"200": ok("the transfers", listOf("transfers", "AgentTransfer"))}),
		},
		"/v1/wallets/{address}": map[string]any{
			"get": op("Look up a wallet before sending to it",
				"Who a wallet id or @handle is.",
				[]map[string]any{param("address", "path")},
				map[string]any{"200": map[string]any{"description": "the wallet's agent and company"}, "404": map[string]any{"description": "no such wallet"}}),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/card": map[string]any{
			"get": op("Read the agent's card and its authorisations",
				"Newest first, approved and declined; each authorisation was decided by the agent's rules and balance.",
				[]map[string]any{ws, agent},
				map[string]any{"200": map[string]any{"description": "the card and its authorisations"}}),
			"post": withBody(op("Issue the agent a virtual card",
				"Owner or admin; the agent's owner is the cardholder. With a live Stripe key it is 409 and nothing is issued: cards take no real money until a licensed partner issues them.",
				[]map[string]any{ws, agent},
				map[string]any{"201": map[string]any{"description": "the card"}, "409": map[string]any{"description": "a live Stripe key, or the agent already has a card — no card is issued"}}),
				body(obj(nil, map[string]any{
					"first_name": str, "last_name": str, "email": str, "phone_number": str,
					"line1": str, "line2": str, "city": str, "postal_code": str, "country": str,
				}))),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/card/freeze": map[string]any{
			"post": op("Freeze the agent's card",
				"Owner or admin. Every purchase on a frozen card is declined and nothing leaves the agent.",
				[]map[string]any{ws, agent},
				map[string]any{"200": map[string]any{"description": "the card, frozen"}, "404": map[string]any{"description": "no such agent, or it holds no card"}}),
		},
		"/v1/workspaces/{wsID}/agents/{agentID}/card/unfreeze": map[string]any{
			"post": op("Unfreeze the agent's card",
				"Owner or admin. Purchases are judged by the agent's rules and balance again.",
				[]map[string]any{ws, agent},
				map[string]any{"200": map[string]any{"description": "the card, unfrozen"}, "404": map[string]any{"description": "no such agent, or it holds no card"}}),
		},
	}
}
