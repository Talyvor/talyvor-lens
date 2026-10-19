package mcp

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/learner"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/reqtrack"
	"github.com/talyvor/lens/internal/rooms"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/session"
	"github.com/talyvor/lens/internal/workspace"
)

// supportedVersions are the MCP protocol versions /mcp speaks, newest first (B40.1). 2026-07-28 is stateless: each
// request names its version in _meta and there is no initialize. The others open with initialize, which answers the
// version the client asks for when it is one of them, else the newest; 2024-11-05 stays for the agents built on it.
var (
	supportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}
	modernVersions    = supportedVersions[:1]
	handshakeVersions = supportedVersions[1:]
)

const (
	legacyVersion   = "2024-11-05" // what a client that names no version gets
	serverName      = "talyvor-lens"
	ssePingInterval = 30 * time.Second

	rpcErrParse        = -32700
	rpcErrInvalidReq   = -32600
	rpcErrMethodNotFnd = -32601
	rpcErrInvalidParam = -32602
	rpcErrInternal     = -32603
	rpcErrHeader       = -32020 // 2026-07-28 HeaderMismatch
	rpcErrVersion      = -32022 // 2026-07-28 UnsupportedProtocolVersion

	metaVersion = "io.modelcontextprotocol/protocolVersion"
)

// pgxDB is the subset of *pgxpool.Pool the MCP tools need. nil pool is
// supported — tools that depend on the DB return a "database not
// configured" error to the caller.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type Server struct {
	pool           pgxDB
	learner        *learner.Learner
	alertManager   *alerts.AlertManager
	wsManager      *workspace.Manager
	sessionTracker *session.SessionTracker
	router         *router.Router
	version        string
	agentBank      AgentBank        // B19.9: the agent tools; nil leaves them out
	credentials    CredentialIssuer // B30.5: wallet_credential; nil refuses it
	market         *market.Store    // B32.23: the market tools; nil leaves them out
	marketDeps     MarketDeps
	rooms          *rooms.Store // B32.36: the room tools; nil leaves them out
	roomDeps       RoomDeps

	sseMu sync.Mutex
	sse   map[string]*sseSession // the open 2024-11-05 HTTP+SSE streams, by session id
}

// sseSession is one 2024-11-05 HTTP+SSE stream: answers to the POSTs that name it are sent down it.
type sseSession struct {
	keyID string
	out   chan []byte
}

func New(
	pool *pgxpool.Pool,
	lrn *learner.Learner,
	alertManager *alerts.AlertManager,
	wsManager *workspace.Manager,
	sessionTracker *session.SessionTracker,
	version string,
) *Server {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newServer(db, lrn, alertManager, wsManager, sessionTracker, version)
}

func newServer(
	pool pgxDB,
	lrn *learner.Learner,
	alertManager *alerts.AlertManager,
	wsManager *workspace.Manager,
	sessionTracker *session.SessionTracker,
	version string,
) *Server {
	return &Server{
		pool:           pool,
		learner:        lrn,
		alertManager:   alertManager,
		wsManager:      wsManager,
		sessionTracker: sessionTracker,
		router:         router.New(),
		version:        version,
		sse:            map[string]*sseSession{},
	}
}

// JSON-RPC 2.0 envelopes ----------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// metaVersion is the version a 2026-07-28 request names in params._meta; "" for a request of the handshake era.
func (r rpcRequest) metaVersion() string {
	var p struct {
		Meta map[string]any `json:"_meta"`
	}
	_ = json.Unmarshal(r.Params, &p)
	v, _ := p.Meta[metaVersion].(string)
	return v
}

func (r rpcRequest) toolName() string {
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(r.Params, &p)
	return p.Name
}

// headerValue undoes the =?base64?…?= form a 2026-07-28 client sends a value that is not plain ASCII in.
func headerValue(h string) string {
	if v, ok := strings.CutPrefix(h, "=?base64?"); ok && strings.HasSuffix(v, "?=") {
		if b, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(v, "?=")); err == nil {
			return string(b)
		}
	}
	return h
}

func rpcOK(id json.RawMessage, r any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: r}
}

func rpcErr(id json.RawMessage, code int, msg string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// HandleRPC answers one JSON-RPC message POSTed to /mcp: Streamable HTTP (2025-03-26 on), and the POST half of the
// 2024-11-05 HTTP+SSE transport when it names a stream with ?sessionId=, whose answer then goes down that stream.
func (s *Server) HandleRPC(w http.ResponseWriter, r *http.Request) {
	var stream *sseSession
	if sid := r.URL.Query().Get("sessionId"); sid != "" {
		s.sseMu.Lock()
		stream = s.sse[sid]
		s.sseMu.Unlock()
		if stream == nil || stream.keyID != callerKeyID(r.Context()) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
	}
	status := http.StatusOK
	var req rpcRequest
	var resp rpcResponse
	err := json.NewDecoder(r.Body).Decode(&req)
	header, modern := r.Header.Get("MCP-Protocol-Version"), req.metaVersion()
	switch {
	case err != nil:
		resp = rpcErr(nil, rpcErrParse, "Parse error")
	case header != "" && !slices.Contains(supportedVersions, header) || modern != "" && !slices.Contains(modernVersions, modern):
		status, resp = http.StatusBadRequest, rpcErr(req.ID, rpcErrVersion, "Unsupported protocol version")
		resp.Error.Data = map[string]any{"supported": supportedVersions, "requested": cmp.Or(modern, header)}
	case modern != "" && (header != modern || r.Header.Get("Mcp-Method") != req.Method ||
		req.Method == "tools/call" && headerValue(r.Header.Get("Mcp-Name")) != req.toolName()):
		status, resp = http.StatusBadRequest, rpcErr(req.ID, rpcErrHeader, "Header mismatch: MCP-Protocol-Version, Mcp-Method and Mcp-Name must match the body")
	case len(req.ID) == 0 || string(req.ID) == "null":
		// A notification (notifications/initialized, …) or a client's answer: nothing to send back.
		w.WriteHeader(http.StatusAccepted)
		return
	default:
		resp = s.dispatch(context.WithValue(r.Context(), callerRequestKey{}, r), req)
		if modern != "" {
			status = s.stateless(&resp, req.Method)
		}
	}
	body, _ := json.Marshal(resp)
	if stream != nil {
		select {
		case stream.out <- body:
			w.WriteHeader(http.StatusAccepted)
		case <-r.Context().Done():
		}
		return
	}
	if req.Method == "initialize" && resp.Error == nil {
		// Every request is authenticated by its own key, so the id is issued for clients that want one and never
		// looked up.
		w.Header().Set("Mcp-Session-Id", newSessionID())
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// stateless finishes a 2026-07-28 answer: every result says it is complete and who answered, a list says how long
// it may be cached, and an unknown method is a 404.
func (s *Server) stateless(resp *rpcResponse, method string) int {
	if resp.Error != nil {
		if resp.Error.Code == rpcErrMethodNotFnd && !strings.HasPrefix(resp.Error.Message, "unknown tool") {
			return http.StatusNotFound
		}
		return http.StatusOK
	}
	if m, ok := resp.Result.(map[string]any); ok {
		m["resultType"] = "complete"
		m["_meta"] = map[string]any{"io.modelcontextprotocol/serverInfo": s.serverInfo()}
		if method == "tools/list" || method == "server/discover" {
			m["ttlMs"], m["cacheScope"] = 300_000, "private"
		}
	}
	return http.StatusOK
}

func (s *Server) serverInfo() map[string]any {
	return map[string]any{"name": serverName, "version": s.version}
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest) rpcResponse {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := legacyVersion
		if p.ProtocolVersion != "" {
			version = handshakeVersions[0]
			if slices.Contains(handshakeVersions, p.ProtocolVersion) {
				version = p.ProtocolVersion
			}
		}
		return rpcOK(req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      s.serverInfo(),
		})
	case "server/discover":
		return rpcOK(req.ID, map[string]any{"supportedVersions": supportedVersions, "capabilities": map[string]any{"tools": map[string]any{}}})
	case "ping":
		return rpcOK(req.ID, map[string]any{})
	case "tools/list":
		return rpcOK(req.ID, map[string]any{"tools": s.tools()})
	case "tools/call":
		return s.handleToolsCall(ctx, req.ID, req.Params)
	default:
		return rpcErr(req.ID, rpcErrMethodNotFnd, "method not found: "+req.Method)
	}
}

func callerKeyID(ctx context.Context) string {
	if a := auth.GetAuthContext(ctx); a != nil {
		return a.APIKeyID
	}
	return ""
}

// HandleSSE is the stream of the 2024-11-05 HTTP+SSE transport: its endpoint event is the URI to POST to, and the
// answers to those POSTs, and a ping every 30 s, come down it.
func (s *Server) HandleSSE(w http.ResponseWriter, r *http.Request) {
	// Meant to stay open, so it is never /healthz's slowest request nor logged as slow (B27.11).
	reqtrack.MarkLongLived(r.Context())
	id, sess := newSessionID(), &sseSession{keyID: callerKeyID(r.Context()), out: make(chan []byte, 16)}
	s.sseMu.Lock()
	s.sse[id] = sess
	s.sseMu.Unlock()
	defer func() {
		s.sseMu.Lock()
		delete(s.sse, id)
		s.sseMu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Through the wrapping middleware, which hides http.Flusher: a type assertion left the stream unsent (B17.131).
	rc := http.NewResponseController(w)
	_, _ = fmt.Fprintf(w, "event: endpoint\ndata: /mcp?sessionId=%s\n\n", id)
	_ = rc.Flush()

	ticker := time.NewTicker(ssePingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-sess.out:
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			_ = rc.Flush()
		case <-ticker.C:
			_, _ = fmt.Fprintf(w, "event: ping\ndata: {}\n\n")
			_ = rc.Flush()
		}
	}
}

// Tool catalogue ------------------------------------------------------------

// tools lists every tool this server answers: the agent tools too when agent wallets are set.
func (s *Server) tools() []map[string]any {
	if s.agentBank == nil {
		return annotate(toolDefinitions())
	}
	all := append(append(toolDefinitions(), agentToolDefinitions()...), walletToolDefinitions()...)
	if s.market != nil {
		all = append(all, marketToolDefinitions()...)
	}
	if s.rooms != nil {
		all = append(all, roomToolDefinitions()...)
	}
	return annotate(all)
}

func toolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name":        "get_spend_summary",
			"description": "Get total spend, request count, and cache hit rate for a workspace over a time period.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workspace_id": map[string]any{"type": "string", "default": "default"},
					"days":         map[string]any{"type": "integer", "default": 30},
				},
				"required": []string{},
			},
		},
		{
			"name":        "get_cache_stats",
			"description": "Get cache hit rate, entry count, and estimated USD savings from caching.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workspace_id": map[string]any{"type": "string", "default": "default"},
				},
				"required": []string{},
			},
		},
		{
			"name":        "get_model_recommendations",
			"description": "Returns the most-repeated prompt patterns the learner suggests caching, with estimated monthly savings.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workspace_id": map[string]any{"type": "string", "default": "default"},
				},
				"required": []string{},
			},
		},
		{
			"name":        "check_circuit_breakers",
			"description": "List which (team, feature) circuit breakers are currently tripped due to spend caps.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
				"required":   []string{},
			},
		},
		{
			"name":        "get_session_summary",
			"description": "Summarise an agent session by ID — turn count, total cost, cache hit rate, duration.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": map[string]any{"type": "string"},
				},
				"required": []string{"session_id"},
			},
		},
		{
			"name":        "route_model",
			"description": "Ask Lens which model to use for a given prompt and provider, plus the estimated cost per 1k tokens.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"provider":        map[string]any{"type": "string", "enum": []string{"openai", "anthropic", "google"}},
					"prompt":          map[string]any{"type": "string"},
					"requested_model": map[string]any{"type": "string"},
				},
				"required": []string{"provider", "prompt"},
			},
		},
	}
}

// tools/call dispatch -------------------------------------------------------

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) handleToolsCall(ctx context.Context, id, paramsRaw json.RawMessage) rpcResponse {
	var params toolCallParams
	if err := json.Unmarshal(paramsRaw, &params); err != nil {
		return rpcErr(id, rpcErrInvalidParam, "Invalid params: "+err.Error())
	}

	var (
		result any
		err    error
	)
	switch params.Name {
	case "agent_balance", "agent_request_approval", "agent_pay", "agent_receipt":
		if s.agentBank == nil {
			return rpcErr(id, rpcErrMethodNotFnd, "unknown tool: "+params.Name)
		}
		result, err = s.callAgentTool(ctx, params.Name, params.Arguments)
	case "get_spend_summary":
		result, err = s.toolGetSpendSummary(ctx, params.Arguments)
	case "get_cache_stats":
		result, err = s.toolGetCacheStats(ctx, params.Arguments)
	case "get_model_recommendations":
		result, err = s.toolGetModelRecommendations(ctx, params.Arguments)
	case "check_circuit_breakers":
		result, err = s.toolCheckCircuitBreakers(ctx)
	case "get_session_summary":
		result, err = s.toolGetSessionSummary(ctx, params.Arguments)
	case "route_model":
		result, err = s.toolRouteModel(ctx, params.Arguments)
	default:
		known := isWalletTool(params.Name) || (isMarketTool(params.Name) && s.market != nil) || (isRoomTool(params.Name) && s.rooms != nil)
		if !known || s.agentBank == nil {
			return rpcErr(id, rpcErrMethodNotFnd, "unknown tool: "+params.Name)
		}
		result, err = s.callAgentTool(ctx, params.Name, params.Arguments) // B22.11, B32.23, B32.36: run and logged as the agent tools are
	}
	var refusal *toolRefusal
	if errors.As(err, &refusal) {
		return rpcOK(id, map[string]any{"content": []map[string]any{{"type": "text", "text": refusal.msg}}, "isError": true})
	}
	if err != nil {
		return rpcErr(id, rpcErrInternal, err.Error())
	}
	// MCP wraps tool results in a content array of typed text blocks.
	body, mErr := json.Marshal(result)
	if mErr != nil {
		return rpcErr(id, rpcErrInternal, "result marshal: "+mErr.Error())
	}
	return rpcOK(id, map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": string(body),
		}},
	})
}

// Tool 1: get_spend_summary -------------------------------------------------

// ⚠ CACHE HITS ARE COUNTED FROM serve_source, NOT THE `cached` BOOLEAN — the same definition
// internal/api/server.go uses, and deliberately worded the same way so a reader comparing the two
// surfaces can see at a glance that they agree.
//
// `token_events.cached` is BOOLEAN NOT NULL DEFAULT false and NOTHING WRITES IT: neither
// insertTokenEventSQL nor insertCacheServeSQL names the column, and no UPDATE touches it. So
// `FILTER (WHERE cached)` matched zero rows on every deployment that has ever existed, and this
// tool reported a hit rate of 0 as though it had measured one.
//
// That was already known. Migration 0100 added serve_source for exactly this reason and the REST
// endpoint moved to it; these tools were not moved, so the defect survived in the surface nobody
// re-checked — an assistant asking Lens what it saved got $0.00. The guard against a third
// divergence is TestMCPAndRESTAgreeOnTheHitRateDefinition, which fails if either side is edited
// alone.
const spendSummarySQL = `SELECT COALESCE(SUM(cost_usd), 0),
  COALESCE(SUM(input_tokens), 0),
  COALESCE(SUM(output_tokens), 0),
  COUNT(*),
  COUNT(*) FILTER (WHERE serve_source LIKE 'cache_hit%')
FROM token_events
WHERE workspace_id = $1
  AND created_at > NOW() - INTERVAL '1 day' * $2`

// effectiveWorkspace derives the workspace a tool may act on from the VERIFIED credential (the auth
// context stamped by AuthMiddleware), never the caller-supplied argument. A non-admin is forced to its
// own workspace — the arg is ignored, so it cannot read another tenant's financials by naming it. The
// global admin may target any workspace via the arg (or its own / "default" when unset). An
// unauthenticated context yields "" (fails closed); /mcp is mounted behind AuthMiddleware, so an
// unauthenticated request never reaches a tool.
func effectiveWorkspace(ctx context.Context, argWorkspace string) string {
	verified, isAdmin := auth.WorkspaceIdentity(ctx)
	if !isAdmin {
		return verified
	}
	if argWorkspace != "" {
		return argWorkspace
	}
	if verified != "" {
		return verified
	}
	return "default"
}

func (s *Server) toolGetSpendSummary(ctx context.Context, args json.RawMessage) (any, error) {
	if s.pool == nil {
		return nil, fmt.Errorf("database not configured")
	}
	var in struct {
		WorkspaceID string `json:"workspace_id"`
		Days        int    `json:"days"`
	}
	_ = json.Unmarshal(args, &in)
	in.WorkspaceID = effectiveWorkspace(ctx, in.WorkspaceID)
	if in.Days <= 0 {
		in.Days = 30
	}

	var (
		totalCost                              float64
		totalIn, totalOut, totalReq, cachedReq int64
	)
	if err := s.pool.QueryRow(ctx, spendSummarySQL, in.WorkspaceID, in.Days).
		Scan(&totalCost, &totalIn, &totalOut, &totalReq, &cachedReq); err != nil {
		return nil, err
	}
	hitRate := 0.0
	avg := 0.0
	if totalReq > 0 {
		hitRate = float64(cachedReq) / float64(totalReq)
		avg = totalCost / float64(totalReq)
	}
	return map[string]any{
		"total_cost_usd":       totalCost,
		"total_input_tokens":   totalIn,
		"total_output_tokens":  totalOut,
		"total_requests":       totalReq,
		"cached_requests":      cachedReq,
		"cache_hit_rate":       hitRate,
		"avg_cost_per_request": avg,
		"period_days":          in.Days,
	}, nil
}

// Tool 2: get_cache_stats ---------------------------------------------------

// Same correction as spendSummarySQL above. Note the second aggregate in particular: with `cached`
// universally false, `FILTER (WHERE NOT cached)` selected EVERY row, so the savings estimate was
// (total spend x 0) — zero by construction rather than by measurement. The complement of a
// cache hit is "served by something other than the cache", which serve_source states directly.
// serve_source is NOT NULL DEFAULT 'upstream' (migration 0100), so NOT LIKE has no NULL hole.
const cacheStatsSQL = `SELECT COUNT(*),
  COUNT(*) FILTER (WHERE serve_source LIKE 'cache_hit%'),
  COALESCE(SUM(cost_usd) FILTER (WHERE serve_source NOT LIKE 'cache_hit%'), 0)
FROM token_events
WHERE workspace_id = $1`

const cacheEntriesSQL = `SELECT COUNT(*) FROM prompt_embeddings`

func (s *Server) toolGetCacheStats(ctx context.Context, args json.RawMessage) (any, error) {
	if s.pool == nil {
		return nil, fmt.Errorf("database not configured")
	}
	var in struct {
		WorkspaceID string `json:"workspace_id"`
	}
	_ = json.Unmarshal(args, &in)
	in.WorkspaceID = effectiveWorkspace(ctx, in.WorkspaceID)

	var total, cached int64
	var uncachedCost float64
	if err := s.pool.QueryRow(ctx, cacheStatsSQL, in.WorkspaceID).Scan(&total, &cached, &uncachedCost); err != nil {
		return nil, err
	}
	var entries int64
	if err := s.pool.QueryRow(ctx, cacheEntriesSQL).Scan(&entries); err != nil {
		return nil, err
	}
	rate := 0.0
	savings := 0.0
	if total > 0 {
		rate = float64(cached) / float64(total)
		savings = uncachedCost * (float64(cached) / float64(total))
	}
	return map[string]any{
		"total_hit_rate":        rate,
		"entries_count":         entries,
		"estimated_savings_usd": savings,
	}, nil
}

// Tool 3: get_model_recommendations -----------------------------------------

// blendedPerTokenCost is the gpt-4o-mini-shaped per-token approximation
// used to translate "tokens saved" into "money saved". Matches the api
// package's estimate so dashboards agree across surfaces.
const blendedPerTokenCost = 0.000000375

func (s *Server) toolGetModelRecommendations(ctx context.Context, _ json.RawMessage) (any, error) {
	if s.learner == nil {
		return []any{}, nil
	}
	insights, err := s.learner.Analyse(ctx)
	if err != nil {
		return nil, err
	}
	if len(insights) > 10 {
		insights = insights[:10]
	}
	out := make([]map[string]any, 0, len(insights))
	for _, ins := range insights {
		est := float64(ins.HitCount) * float64(ins.AvgTokensSaved) * blendedPerTokenCost * 30
		out = append(out, map[string]any{
			"pattern_hash":                  ins.PromptPattern,
			"hit_count":                     ins.HitCount,
			"recommendation":                ins.Recommendation,
			"estimated_monthly_savings_usd": est,
		})
	}
	// Wrap to a single-key object so the MCP content text is parseable
	// as JSON object (the test helper expects that shape).
	return map[string]any{"recommendations": out}, nil
}

// Tool 4: check_circuit_breakers --------------------------------------------

// toolCheckCircuitBreakers shows the caller its own workspace's circuits, keyed "team:feature"; only
// the global admin sees every workspace's, keyed "workspace:team:feature" (B42.10).
func (s *Server) toolCheckCircuitBreakers(ctx context.Context) (any, error) {
	if s.alertManager == nil {
		return map[string]any{"circuits": map[string]string{}, "open_count": 0}, nil
	}
	ws, isAdmin := auth.WorkspaceIdentity(ctx)
	states := s.alertManager.WorkspaceCircuitStates(ws)
	if isAdmin {
		states = s.alertManager.CircuitStates()
	}
	openCount := 0
	for _, v := range states {
		if v == "open" {
			openCount++
		}
	}
	return map[string]any{
		"circuits":   states,
		"open_count": openCount,
	}, nil
}

// Tool 5: get_session_summary -----------------------------------------------

func (s *Server) toolGetSessionSummary(ctx context.Context, args json.RawMessage) (any, error) {
	if s.sessionTracker == nil {
		return nil, fmt.Errorf("session tracker not configured")
	}
	var in struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.SessionID == "" {
		return nil, fmt.Errorf("session_id required")
	}
	// B42.2: another workspace's session reads exactly like one that does not exist.
	sess, ok := s.sessionTracker.GetSession(in.SessionID)
	if caller, isAdmin := auth.WorkspaceIdentity(ctx); !ok || (!isAdmin && (caller == "" || sess.WorkspaceID != caller)) {
		return nil, fmt.Errorf("session not found")
	}
	sum := s.sessionTracker.SummariseSession(ctx, in.SessionID)
	return map[string]any{
		"session_id":       sum.SessionID,
		"agent_name":       sum.AgentName,
		"turn_count":       sum.TurnCount,
		"total_cost_usd":   sum.TotalCostUSD,
		"cache_hit_rate":   sum.CacheHitRate,
		"duration_seconds": sum.Duration.Seconds(),
	}, nil
}

// Tool 6: route_model -------------------------------------------------------

func (s *Server) toolRouteModel(ctx context.Context, args json.RawMessage) (any, error) {
	var in struct {
		Provider       string `json:"provider"`
		Prompt         string `json:"prompt"`
		RequestedModel string `json:"requested_model"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.Provider == "" || in.Prompt == "" {
		return nil, fmt.Errorf("provider and prompt required")
	}
	decision := s.router.Route(ctx, in.Provider, in.RequestedModel, in.Prompt)

	// Estimated cost per 1k tokens: blended 500-in / 500-out for the
	// recommended model. Lookup is via alerts to keep the price table
	// in one place.
	est := alerts.CostUSD(decision.Model, 500, 500)
	return map[string]any{
		"recommended_model":            decision.Model,
		"cost_tier":                    decision.CostTier,
		"reason":                       decision.Reason,
		"estimated_cost_per_1k_tokens": est,
	}, nil
}
