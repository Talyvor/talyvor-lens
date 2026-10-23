package api

// openapi.go — hand-written OpenAPI 3.0 spec served at GET /openapi.json: Agent Wallets and the
// gateway that enforces them. Lens has too many routes to spec every single one, so this file covers
// a subset. Returns a valid JSON document when marshalled.
//
// COVERED: agent wallets (agents, keys, funding, rules, approvals, statements, transfers and cards —
// openapi_wallets.go, listed first under the "Agent Wallets" tag), proxy endpoints, key management,
// workspaces, tenant config, and the local-endpoint registry. Every path is registered by the binary;
// that is guarded, not assumed (openapi_route_contract_test.go).
//
// ⚠ NOT COVERED, AND THIS PARAGRAPH IS WHY W6.29 EXISTS. This header used to name `attribution`
// and `A/B` among the surfaces it covers. Measured against the served document: ZERO published
// paths for either, against 8 registered attribution routes and 6 experiment / eval-A-B routes
// outside /v1/admin. A client generating from this document gets no attribution API at all while
// the header says it is there. Also not covered: 5 of the 7 registered proxy providers — only
// openai and anthropic are published; bedrock, google, groq, mistral and vllm are not.
//
// ⚠ 122 NON-ADMIN /v1 ROUTES ARE OUTSIDE THIS DOCUMENT and that is a position, not a bug — see the
// count and its reasoning in openapi_route_contract_test.go, which pins the number so it cannot
// drift by fifty in silence.
//
// ⚠ AND ONE PUBLISHED SURFACE DOES NOTHING. components.schemas.WorkspaceConfig declares
// spending_cap_usd, monthly_budget, rate_limit_rpm, rate_limit_tpm, allowed_models,
// allowed_providers, log_level and retention_days, and PUT /v1/workspaces/{wsID}/config stores all
// of them. W6.27 (#495) measured that NOTHING READS ANY OF IT — the allowlist the proxy enforces
// belongs to a different table. Publishing a settings contract is a stronger promise than storing
// one, so this note stays until that is decided.

import (
	"encoding/json"
	"net/http"
)

// OpenAPISpec returns the spec as a generic `map[string]any` so
// the package doesn't grow a dependency on a typed OpenAPI
// schema library. The shape is intentionally hand-rolled.
func OpenAPISpec() map[string]any {
	spec := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":   "Talyvor Lens API",
			"version": APIVersion,
			"description": "Agent Wallets and the gateway that enforces them. Every AI agent gets a wallet — a balance, " +
				"spending rules, approvals, a card and a live statement — and Lens judges each model call and payment " +
				"against that wallet before it reaches the provider or the payee. Underneath: multi-provider routing " +
				"with cost tracking, quality scoring and tenant isolation.",
			"contact": map[string]any{
				"name": "Talyvor",
				"url":  "https://talyvor.com",
			},
		},
		"servers": []map[string]any{
			{"url": "/", "description": "current deployment"},
		},
		"tags": openAPIWalletTags(),
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"ApiKeyAuth": map[string]any{
					"type":        "http",
					"scheme":      "bearer",
					"description": "Lens API key — Authorization: Bearer tlv_...",
				},
			},
			"schemas": map[string]any{
				// ⚠ NO RESPONSE THIS BINARY SENDS SATISFIES THIS SCHEMA — measured on real driven
				// middleware, not read (W6.30, internal/apicontract/error_contract_test.go). The two
				// operations that $ref it are the openai proxy's 401 and 429, and they emit
				// {"error":…} and {"error":…,"limit_type":…,"retry_after_seconds":…} respectively:
				// neither carries `code` or `message`, both of which are required here. The nine
				// ErrCode* constants in middleware.go are emitted nowhere and appear only as the
				// `example` below. ⚠ BOTH REPAIRS ARE DECISIONS — moving the wire to this schema
				// breaks every client parsing `error`; moving this schema to the wire discards the
				// designed contract — so W6.30 pins the gap rather than choosing.
				"APIError": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"code":       map[string]any{"type": "string", "example": ErrCodeInvalidRequest},
						"message":    map[string]any{"type": "string"},
						"details":    map[string]any{"type": "object", "nullable": true},
						"request_id": map[string]any{"type": "string"},
					},
					"required": []string{"code", "message"},
				},
				"PaginatedResponse": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"data":        map[string]any{"type": "array", "items": map[string]any{}},
						"page":        map[string]any{"type": "integer", "example": 1},
						"page_size":   map[string]any{"type": "integer", "example": 20},
						"total":       map[string]any{"type": "integer"},
						"total_pages": map[string]any{"type": "integer"},
						"has_next":    map[string]any{"type": "boolean"},
						"has_prev":    map[string]any{"type": "boolean"},
						"next_cursor": map[string]any{"type": "string", "nullable": true},
					},
				},
				"HealthStatus": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"status":         map[string]any{"type": "string", "enum": []string{"healthy", "degraded", "unhealthy"}},
						"version":        map[string]any{"type": "string"},
						"uptime_seconds": map[string]any{"type": "integer"},
						"checks":         map[string]any{"type": "object", "additionalProperties": true},
						// B27.11: state, not pass/fail — what tells a hang from a restart from a blip.
						"database_pool": map[string]any{
							"type":        "object",
							"description": "admin key only",
							"properties": map[string]any{
								"in_use":         map[string]any{"type": "integer"},
								"idle":           map[string]any{"type": "integer"},
								"max":            map[string]any{"type": "integer"},
								"wait_count":     map[string]any{"type": "integer", "description": "acquires that found no idle connection and waited"},
								"wait_ms":        map[string]any{"type": "integer", "description": "total time those acquires waited"},
								"canceled_waits": map[string]any{"type": "integer", "description": "acquires that gave up waiting"},
							},
						},
						"requests": map[string]any{
							"type":        "object",
							"description": "admin key only",
							"properties": map[string]any{
								"in_flight":  map[string]any{"type": "integer", "description": "requests being served, not counting this probe"},
								"long_lived": map[string]any{"type": "integer", "description": "of those, subscriptions meant to stay open (SSE)"},
								"slowest_5m": map[string]any{
									"type":        "object",
									"nullable":    true,
									"description": "the slowest request of the last five minutes, finished or still running; route template only, never a path",
									"properties": map[string]any{
										"route":   map[string]any{"type": "string"},
										"ms":      map[string]any{"type": "integer"},
										"phase":   map[string]any{"type": "string", "enum": []string{"handler", "db_acquire", "db_query", "redis", "upstream"}},
										"running": map[string]any{"type": "boolean"},
									},
								},
							},
						},
					},
				},
				"WorkspaceConfig": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":                map[string]any{"type": "string"},
						"name":              map[string]any{"type": "string"},
						"spending_cap_usd":  map[string]any{"type": "number"},
						"monthly_budget":    map[string]any{"type": "number"},
						"rate_limit_rpm":    map[string]any{"type": "integer"},
						"rate_limit_tpm":    map[string]any{"type": "integer"},
						"allowed_models":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"allowed_providers": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"log_level":         map[string]any{"type": "string", "enum": []string{"all", "errors", "none"}},
						"retention_days":    map[string]any{"type": "integer"},
					},
				},
				"WorkspaceAPIKey": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":           map[string]any{"type": "string"},
						"workspace_id": map[string]any{"type": "string"},
						"key_prefix":   map[string]any{"type": "string"},
						"name":         map[string]any{"type": "string"},
						"scopes":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"expires_at":   map[string]any{"type": "string", "format": "date-time", "nullable": true},
						"created_at":   map[string]any{"type": "string", "format": "date-time"},
					},
				},
				"LocalEndpoint": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":             map[string]any{"type": "string"},
						"url":            map[string]any{"type": "string"},
						"provider":       map[string]any{"type": "string", "enum": []string{"ollama", "vllm", "llamacpp"}},
						"models":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"priority":       map[string]any{"type": "integer"},
						"max_concurrent": map[string]any{"type": "integer"},
						"active":         map[string]any{"type": "boolean"},
						"healthy":        map[string]any{"type": "boolean"},
						"avg_latency_ms": map[string]any{"type": "integer"},
						"error_rate":     map[string]any{"type": "number"},
					},
				},
			},
		},
		"security": []map[string]any{{"ApiKeyAuth": []string{}}},
		"paths":    openAPIPaths(),
	}
	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
	for name, schema := range openAPIWalletSchemas() {
		schemas[name] = schema
	}
	paths := spec["paths"].(map[string]any)
	for p, item := range openAPIWalletPaths() {
		paths[p] = item
	}
	return spec
}

func openAPIPaths() map[string]any {
	jsonResp := func(schemaRef string) map[string]any {
		return map[string]any{
			"content": map[string]any{
				"application/json": map[string]any{
					"schema": map[string]any{"$ref": "#/components/schemas/" + schemaRef},
				},
			},
		}
	}
	errResp := func(status string) map[string]any {
		return map[string]any{
			"description": "error",
			"content": map[string]any{
				"application/json": map[string]any{
					"schema": map[string]any{"$ref": "#/components/schemas/APIError"},
				},
			},
		}
	}
	// ⚠ `_ = errResp` STOOD HERE. It is used twice below (the openai proxy's 401 and 429), so the
	// blank assignment read as "this helper is unused" while it was the only error contract the
	// document has. W6.30.
	return map[string]any{
		"/healthz": map[string]any{
			"get": map[string]any{
				"summary":     "Liveness + dependency probe",
				"description": "Public callers get status, version, uptime_seconds and each check's status and latency_ms. A check's detail and the database_pool and requests sections are shown only with the admin key, the same credential /metrics needs. 503 when a dependency is down.",
				"security":    []map[string]any{},
				"responses": map[string]any{
					"200": map[string]any{
						"description": "service health",
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{"$ref": "#/components/schemas/HealthStatus"},
							},
						},
					},
				},
			},
		},
		"/v1/proxy/openai/{path}": map[string]any{
			"post": map[string]any{
				"summary":     "Proxy to OpenAI",
				"description": "Routes OpenAI-shape chat / completions / embeddings through Lens.",
				"parameters": []map[string]any{
					{"name": "path", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "proxied response"},
					"401": errResp("401"),
					"429": errResp("429"),
				},
			},
		},
		// B18.13 — a document too large to carry inline (the proxy caps a request body at 4 MiB).
		"/v1/documents": map[string]any{
			"post": map[string]any{
				"summary": "Upload a document for chats to reference by id",
				"description": "The request body is the file (up to 25 MB) and its Content-Type the document's media type: " +
					"PDF, Word, Excel, PowerPoint, HTML, CSV, JSON, XML, Markdown or plain text; ?filename= optionally names it. " +
					"A chat request then references the returned id instead of carrying the file — " +
					`{"type":"document","source":{"type":"file","file_id":"tdoc_…"}} (Anthropic) or ` +
					`{"type":"file","file":{"file_id":"tdoc_…"}} (OpenAI) — and Lens converts it to text on the way to the model, ` +
					"reporting the saving in X-Talyvor-Distill-Tokens-Saved and X-Talyvor-Distill-Bytes-Saved. " +
					"Only the uploading workspace can reference a document. A document over 25 MB is refused with 413, " +
					"a Content-Type Lens cannot read with 415.",
				"parameters": []map[string]any{
					{"name": "filename", "in": "query", "required": false, "schema": map[string]any{"type": "string"}},
				},
				"requestBody": map[string]any{"required": true, "content": map[string]any{
					"application/octet-stream": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}},
				}},
				"responses": map[string]any{
					"201": map[string]any{"description": "the stored document: id, media_type, filename, size_bytes, uploaded_at"},
				},
			},
		},
		// B40.10 — speech, OpenAI-compatible, priced per minute or per character and charged like a model call.
		"/v1/audio/transcriptions": map[string]any{
			"post": map[string]any{
				"summary": "Transcribe audio to text",
				"description": "multipart/form-data: file (the audio, up to 25 MB), model (nova-3, the default, or whisper-1), " +
					"and optionally language and response_format (json, the default, or text). Charged per minute heard at the " +
					"model's catalog price, from the agent's wallet for an agent's key. Lens keeps no audio. 503 when speech is " +
					"not configured; 402 when the wallet or budget does not cover the clip.",
				"requestBody": map[string]any{"required": true, "content": map[string]any{
					"multipart/form-data": map[string]any{"schema": map[string]any{"type": "object", "required": []string{"file"},
						"properties": map[string]any{
							"file":            map[string]any{"type": "string", "format": "binary"},
							"model":           map[string]any{"type": "string"},
							"language":        map[string]any{"type": "string"},
							"response_format": map[string]any{"type": "string", "enum": []string{"json", "text"}},
						}}},
				}},
				"responses": map[string]any{
					"200": map[string]any{"description": `{"text": "…"}, or the text alone for response_format=text`},
					"402": map[string]any{"description": "the wallet, budget or an agent's rules do not cover it"},
					"503": map[string]any{"description": "speech is not configured"},
				},
			},
		},
		"/v1/audio/speech": map[string]any{
			"post": map[string]any{
				"summary": "Speak text aloud",
				"description": `JSON {"input": "…" (up to 4,096 characters), "model" (aura-2, the default, tts-1 or tts-1-hd), ` +
					`"voice", "response_format" (mp3, the default, opus, aac, flac or wav)}. Answers the audio. Charged per ` +
					"character at the model's catalog price. 503 when speech is not configured.",
				"requestBody": map[string]any{"required": true, "content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"type": "object", "required": []string{"input"},
						"properties": map[string]any{
							"input":           map[string]any{"type": "string"},
							"model":           map[string]any{"type": "string"},
							"voice":           map[string]any{"type": "string"},
							"response_format": map[string]any{"type": "string"},
						}}},
				}},
				"responses": map[string]any{
					"200": map[string]any{"description": "the spoken audio"},
					"402": map[string]any{"description": "the wallet, budget or an agent's rules do not cover it"},
					"503": map[string]any{"description": "speech is not configured"},
				},
			},
		},
		"/v1/proxy/anthropic/{path}": map[string]any{
			"post": map[string]any{
				"summary": "Proxy to Anthropic",
				"parameters": []map[string]any{
					{"name": "path", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "proxied response"},
				},
			},
		},
		"/v1/api/keys": map[string]any{
			"post": map[string]any{
				"summary": "Create a Lens admin API key",
				"responses": map[string]any{
					"201": map[string]any{"description": "key created (raw key shown once)"},
				},
			},
		},
		"/v1/api/keys/{keyID}": map[string]any{
			"delete": map[string]any{
				"summary": "Revoke a Lens admin API key",
				"parameters": []map[string]any{
					{"name": "keyID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{"200": map[string]any{"description": "revoked"}},
			},
		},
		"/v1/workspaces/{wsID}/config": map[string]any{
			"get": map[string]any{
				"summary": "Get workspace tenant config",
				"parameters": []map[string]any{
					{"name": "wsID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{"200": jsonResp("WorkspaceConfig")},
			},
			"put": map[string]any{
				"summary": "Upsert workspace tenant config",
				"parameters": []map[string]any{
					{"name": "wsID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"requestBody": map[string]any{
					"required": true,
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/WorkspaceConfig"},
						},
					},
				},
				"responses": map[string]any{"200": jsonResp("WorkspaceConfig")},
			},
		},
		"/v1/workspaces/{wsID}/api-keys": map[string]any{
			"get": map[string]any{
				"summary": "List workspace-scoped API keys",
				"parameters": []map[string]any{
					{"name": "wsID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{
					"200": map[string]any{
						"description": "list of keys",
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type":  "array",
									"items": map[string]any{"$ref": "#/components/schemas/WorkspaceAPIKey"},
								},
							},
						},
					},
				},
			},
			"post": map[string]any{
				"summary": "Issue a workspace-scoped API key",
				"parameters": []map[string]any{
					{"name": "wsID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{"201": map[string]any{"description": "key created — raw key shown exactly once"}},
			},
		},
		"/v1/workspaces/{wsID}/api-keys/{keyID}": map[string]any{
			"delete": map[string]any{
				"summary":   "Revoke a workspace-scoped API key",
				"responses": map[string]any{"200": map[string]any{"description": "revoked"}},
			},
		},
		"/v1/workspaces/{wsID}/provider-keys": map[string]any{
			"get": map[string]any{
				"summary":     "List this workspace's own provider keys (BYOK) — the last four characters only",
				"description": `{"byok": on the BYOK plan, "providers": the providers a key can be added for, "keys": [{"provider","last4","updated_at"}]}. The key itself is never returned. Present only when the deployment holds custody (LENS_PROVIDER_SECRET_KEK).`,
				"parameters": []map[string]any{
					{"name": "wsID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{"200": map[string]any{"description": "byok, providers, keys"}},
			},
		},
		"/v1/workspaces/{wsID}/provider-keys/{provider}": map[string]any{
			"put": map[string]any{
				"summary":     "Add or replace this workspace's own key for a provider (BYOK)",
				"description": `Body {"key":"…"}. Requests to that provider are then sent on this key and charged no tokens. Refused (402) unless the workspace is on the BYOK plan; an unknown provider or a malformed key is refused (400). Answers with the provider and last four characters only.`,
				"parameters": []map[string]any{
					{"name": "wsID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "provider", "in": "path", "required": true, "schema": map[string]any{"type": "string", "enum": []string{"anthropic", "google", "groq", "mistral", "openai"}}},
				},
				"responses": map[string]any{"200": map[string]any{"description": "provider, last4, updated_at"}},
			},
			"delete": map[string]any{
				"summary":     "Remove this workspace's own key for a provider (BYOK)",
				"description": "404 when no key is stored for that provider.",
				"parameters": []map[string]any{
					{"name": "wsID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "provider", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{"204": map[string]any{"description": "removed"}},
			},
		},
		"/v1/public/fees": map[string]any{
			"get": map[string]any{
				"summary":     "Get every fee Talyvor charges",
				"description": "No key needed. Basis points (100 = 1%) unless named minor units: market_take_bps (listing sales; the seller keeps the rest), services_take_bps, compute_take_bps, lending_fee_bps, merchant_fee_bps, merchant_a2a_fee_bps, and by plan platform_fee_bps and fx_margin_bps; intl_payment_fee_minor by currency.",
				"security":    []map[string]any{},
				"responses":   map[string]any{"200": map[string]any{"description": "every fee setting"}},
			},
		},
		"/v1/public/plan-gates": map[string]any{
			"get": map[string]any{
				"summary":     "Get what each plan unlocks",
				"description": "No key needed. order (free, team, business, enterprise) and, by plan, agents and seats (-1 unlimited), own_provider_keys (none, add_on — only with the BYOK add-on — or included), live_money, slack_teams_approvals, sso, audit_export and edge. plus, pro and max take free's; byok takes team's, with own keys.",
				"security":    []map[string]any{},
				"responses":   map[string]any{"200": map[string]any{"description": "order and plans"}},
			},
		},
		"/v1/workspaces/{wsID}/plan": map[string]any{
			"get": map[string]any{
				"summary": "Get this workspace's plan and what it unlocks",
				"parameters": []map[string]any{
					{"name": "wsID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{"200": map[string]any{"description": "plan, gated_as, byok_add_on, gates, own_provider_keys_allowed, agents_used"}},
			},
		},
		"/v1/workspaces/{wsID}/plan/seats": map[string]any{
			"get": map[string]any{
				"summary":     "Check whether this workspace's plan takes a number of members",
				"description": "Asked before a member is added, with the count the workspace would have. 402 names LENS_PLAN_GATES, the plan and the plan that would allow it.",
				"parameters": []map[string]any{
					{"name": "wsID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
					{"name": "members", "in": "query", "required": true, "schema": map[string]any{"type": "integer"}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "plan, seats, members"},
					"402": map[string]any{"description": "error, plan, gate, limit, allows"},
				},
			},
		},
		"/v1/workspaces/{wsID}/spend/current-month": map[string]any{
			"get": map[string]any{
				"summary":   "Get current-month spend for a workspace",
				"responses": map[string]any{"200": map[string]any{"description": "spend snapshot"}},
			},
		},
		"/v1/workspaces/{wsID}/savings/current-month": map[string]any{
			"get": map[string]any{
				"summary":     "Get this month's measured saving for a workspace",
				"description": "Summed over the workspace's own requests this calendar month (UTC): what each would have cost at the model it asked for with no Talyvor cache (list_usd), minus what it was charged (charged_usd). Requests recorded without a measurement are counted in unmeasured_requests and left out of the sums.",
				"parameters": []map[string]any{
					{"name": "wsID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{"200": map[string]any{"description": "month_start, saved_usd, list_usd, charged_usd, requests, unmeasured_requests"}},
			},
		},
		"/v1/workspaces/{wsID}/stored-answers": map[string]any{
			"get": map[string]any{
				"summary": "Count this workspace's stored answers and document conversions, by scope",
				"parameters": []map[string]any{
					{"name": "wsID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{"200": map[string]any{"description": "shared_answers, private_answers, shared_conversions, private_conversions, cached_copies"}},
			},
			"delete": map[string]any{
				"summary":     "Delete this workspace's stored answers — irreversible, owner or admin only",
				"description": `Body {"scope":"shared"|"all","confirm":"<workspace>"}. "shared" deletes every answer and document conversion this workspace shared; "all" also deletes its private ones. Earnings already final are kept. A confirm that does not match the workspace is refused (400); a caller that is not the owner or an admin is refused (403).`,
				"parameters": []map[string]any{
					{"name": "wsID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{"200": map[string]any{"description": "what was deleted, as counts"}},
			},
		},
		"/v1/workspaces/{wsID}/deletion-requests": map[string]any{
			"post": map[string]any{
				"summary":     "Ask Talyvor to delete everything it holds for this workspace",
				"description": `Body {"note":"…"} (optional). An operator completes the request; billing and ledger records are kept, as the law requires.`,
				"parameters": []map[string]any{
					{"name": "wsID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{"201": map[string]any{"description": "the request, status requested"}},
			},
			"get": map[string]any{
				"summary": "List this workspace's deletion requests and their status",
				"parameters": []map[string]any{
					{"name": "wsID", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
				},
				"responses": map[string]any{"200": map[string]any{"description": "requests: id, status (requested|done), requested_at, completed_at"}},
			},
		},
		"/v1/local/endpoints": map[string]any{
			"get": map[string]any{
				"summary":     "List registered local-model endpoints",
				"description": "admin key only",
				"responses": map[string]any{
					"401": map[string]any{"description": "admin credentials required"},
					"200": map[string]any{
						"description": "list of endpoints",
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type":  "array",
									"items": map[string]any{"$ref": "#/components/schemas/LocalEndpoint"},
								},
							},
						},
					},
				},
			},
			"post": map[string]any{
				"summary": "Register a local-model endpoint",
				"requestBody": map[string]any{
					"required": true,
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": "#/components/schemas/LocalEndpoint"},
						},
					},
				},
				"responses": map[string]any{"201": jsonResp("LocalEndpoint")},
			},
		},
		"/v1/local/endpoints/{id}": map[string]any{
			"delete": map[string]any{
				"summary":   "Remove a local endpoint",
				"responses": map[string]any{"200": map[string]any{"description": "removed"}},
			},
		},
		"/v1/local/endpoints/{id}/check": map[string]any{
			"post": map[string]any{
				"summary":   "Trigger an immediate health probe for one endpoint",
				"responses": map[string]any{"200": jsonResp("LocalEndpoint")},
			},
		},
	}
}

// ServeOpenAPI is the http.HandlerFunc the router mounts at
// GET /openapi.json. We marshal on every request rather than
// cache the result — it's cheap, and it means a redeploy with a
// changed version string is reflected immediately.
func ServeOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(OpenAPISpec())
}
