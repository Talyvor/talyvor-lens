package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/auth"
)

// embedded NATS for the alerts manager (it needs a connection even if
// tests never publish/subscribe).
func runNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := &natsserver.Options{
		Host:     "127.0.0.1",
		Port:     -1,
		StoreDir: t.TempDir(),
		NoLog:    true,
		NoSigs:   true,
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("natsserver.NewServer: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		srv.Shutdown()
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(func() {
		nc.Close()
		srv.Shutdown()
		srv.WaitForShutdown()
	})
	return nc
}

func newTestServer(t *testing.T, pool pgxmock.PgxPoolIface) *Server {
	t.Helper()
	return newServer(pool, nil, nil, nil, nil, "test-version")
}

// rpcCall fires a JSON-RPC request through HandleRPC and returns the
// parsed response.
func rpcCall(t *testing.T, s *Server, method string, params any) rpcResponse {
	t.Helper()
	bodyMap := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
	}
	if params != nil {
		bodyMap["params"] = params
	}
	body, _ := json.Marshal(bodyMap)

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	// Mirror production: /mcp is mounted behind AuthMiddleware, so every tool call carries a verified
	// workspace in context. Tests stamp a non-admin "default" workspace (the tools force the acted-on
	// workspace to this verified value via effectiveWorkspace).
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: "default"}))
	w := httptest.NewRecorder()
	s.HandleRPC(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp rpcResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode JSON-RPC: %v; body=%s", err, w.Body.String())
	}
	return resp
}

// rpcCallString is rpcCall but returns the raw JSON-RPC body (string)
// for assertions that don't fit into a parsed struct.
func rpcCallString(t *testing.T, s *Server, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.HandleRPC(w, req)
	return w.Code, w.Body.String()
}

// toolResult parses a tools/call response into the inner JSON result
// the spec wraps in `content[0].text`.
func toolResult(t *testing.T, resp rpcResponse) map[string]any {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("expected no error, got %+v", resp.Error)
	}
	res, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result not an object: %T %v", resp.Result, resp.Result)
	}
	content, ok := res["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content missing or empty: %v", res)
	}
	first := content[0].(map[string]any)
	if first["type"] != "text" {
		t.Fatalf("content[0].type = %v, want text", first["type"])
	}
	textStr, _ := first["text"].(string)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(textStr), &parsed); err != nil {
		t.Fatalf("decode content text: %v; text=%s", err, textStr)
	}
	return parsed
}

func TestMCP_InitializeReturnsCorrectProtocolVersion(t *testing.T) {
	pool, _ := pgxmock.NewPool()
	t.Cleanup(pool.Close)
	s := newTestServer(t, pool)

	resp := rpcCall(t, s, "initialize", nil)
	if resp.Error != nil {
		t.Fatalf("initialize errored: %+v", resp.Error)
	}
	res := resp.Result.(map[string]any)
	if res["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v, want 2024-11-05", res["protocolVersion"])
	}
	info := res["serverInfo"].(map[string]any)
	if info["name"] != "talyvor-lens" {
		t.Errorf("serverInfo.name = %v, want talyvor-lens", info["name"])
	}
	if info["version"] != "test-version" {
		t.Errorf("serverInfo.version = %v, want test-version", info["version"])
	}
}

func TestMCP_ToolsListReturnsAllSixTools(t *testing.T) {
	pool, _ := pgxmock.NewPool()
	t.Cleanup(pool.Close)
	s := newTestServer(t, pool)

	resp := rpcCall(t, s, "tools/list", nil)
	res := resp.Result.(map[string]any)
	tools := res["tools"].([]any)
	if len(tools) != 6 {
		t.Fatalf("got %d tools, want 6", len(tools))
	}
	wantNames := map[string]bool{
		"get_spend_summary":         true,
		"get_cache_stats":           true,
		"get_model_recommendations": true,
		"check_circuit_breakers":    true,
		"get_session_summary":       true,
		"route_model":               true,
	}
	for _, tl := range tools {
		entry := tl.(map[string]any)
		name, _ := entry["name"].(string)
		if !wantNames[name] {
			t.Errorf("unexpected tool: %q", name)
		}
		delete(wantNames, name)
	}
	if len(wantNames) > 0 {
		t.Errorf("missing tools: %v", wantNames)
	}
}

func TestMCP_ToolsCall_GetSpendSummary(t *testing.T) {
	pool, _ := pgxmock.NewPool()
	t.Cleanup(pool.Close)
	pool.ExpectQuery(`SUM\(cost_usd\)`).
		WithArgs("default", 30).
		WillReturnRows(
			pgxmock.NewRows([]string{
				"total_cost_usd", "total_input_tokens", "total_output_tokens",
				"total_requests", "cached_requests",
			}).AddRow(float64(123.45), int64(50000), int64(25000), int64(500), int64(200)),
		)

	s := newTestServer(t, pool)
	resp := rpcCall(t, s, "tools/call", map[string]any{
		"name":      "get_spend_summary",
		"arguments": map[string]any{"workspace_id": "default", "days": 30},
	})
	data := toolResult(t, resp)

	if data["total_cost_usd"].(float64) != 123.45 {
		t.Errorf("total_cost_usd = %v, want 123.45", data["total_cost_usd"])
	}
	if int(data["total_requests"].(float64)) != 500 {
		t.Errorf("total_requests = %v, want 500", data["total_requests"])
	}
	if data["cache_hit_rate"].(float64) != 0.4 {
		t.Errorf("cache_hit_rate = %v, want 0.4", data["cache_hit_rate"])
	}
	if int(data["period_days"].(float64)) != 30 {
		t.Errorf("period_days = %v, want 30", data["period_days"])
	}
}

func TestMCP_ToolsCall_GetCacheStats(t *testing.T) {
	pool, _ := pgxmock.NewPool()
	t.Cleanup(pool.Close)
	pool.ExpectQuery(`COUNT\(\*\)`).
		WithArgs("default").
		WillReturnRows(
			pgxmock.NewRows([]string{"total", "cached", "uncached_cost"}).
				AddRow(int64(100), int64(60), float64(50.0)),
		)
	pool.ExpectQuery(`COUNT\(\*\) FROM prompt_embeddings`).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(15000)))

	s := newTestServer(t, pool)
	resp := rpcCall(t, s, "tools/call", map[string]any{
		"name":      "get_cache_stats",
		"arguments": map[string]any{"workspace_id": "default"},
	})
	data := toolResult(t, resp)

	if data["total_hit_rate"].(float64) != 0.6 {
		t.Errorf("total_hit_rate = %v, want 0.6", data["total_hit_rate"])
	}
	if int(data["entries_count"].(float64)) != 15000 {
		t.Errorf("entries_count = %v, want 15000", data["entries_count"])
	}
	if data["estimated_savings_usd"].(float64) != 30.0 {
		t.Errorf("estimated_savings_usd = %v, want 30.0", data["estimated_savings_usd"])
	}
}

func TestMCP_ToolsCall_CheckCircuitBreakers(t *testing.T) {
	nc := runNATS(t)
	am := alerts.New(nil, nc, nil)
	am.OpenCircuit("default", "team-a", "search")
	am.OpenCircuit("default", "team-b", "summarize")
	am.OpenCircuit("ws-other", "team-a", "search") // B42.10: another workspace's circuit is not the caller's to see

	s := newServer(nil, nil, am, nil, nil, "test-version")
	resp := rpcCall(t, s, "tools/call", map[string]any{
		"name":      "check_circuit_breakers",
		"arguments": map[string]any{},
	})
	data := toolResult(t, resp)

	circuits, ok := data["circuits"].(map[string]any)
	if !ok {
		t.Fatalf("circuits not an object: %v", data)
	}
	if circuits["team-a:search"] != "open" {
		t.Errorf("team-a:search = %v, want open", circuits["team-a:search"])
	}
	if int(data["open_count"].(float64)) != 2 {
		t.Errorf("open_count = %v, want 2", data["open_count"])
	}
}

func TestMCP_ToolsCall_RouteModel(t *testing.T) {
	pool, _ := pgxmock.NewPool()
	t.Cleanup(pool.Close)
	s := newTestServer(t, pool)

	resp := rpcCall(t, s, "tools/call", map[string]any{
		"name": "route_model",
		"arguments": map[string]any{
			"provider":        "openai",
			"prompt":          "hello world",
			"requested_model": "gpt-4o",
		},
	})
	data := toolResult(t, resp)

	model, _ := data["recommended_model"].(string)
	if model == "" {
		t.Errorf("recommended_model empty: %v", data)
	}
	tier, _ := data["cost_tier"].(string)
	if tier != "cheap" {
		t.Errorf("cost_tier = %v, want cheap (simple prompt)", tier)
	}
	if data["reason"].(string) == "" {
		t.Error("reason missing")
	}
}

func TestMCP_ToolsCall_UnknownToolReturnsError(t *testing.T) {
	pool, _ := pgxmock.NewPool()
	t.Cleanup(pool.Close)
	s := newTestServer(t, pool)

	resp := rpcCall(t, s, "tools/call", map[string]any{
		"name":      "no_such_tool",
		"arguments": map[string]any{},
	})
	if resp.Error == nil {
		t.Fatal("expected JSON-RPC error for unknown tool")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("error code = %d, want -32601", resp.Error.Code)
	}
}

func TestMCP_SSEReturnsEndpointEvent(t *testing.T) {
	pool, _ := pgxmock.NewPool()
	t.Cleanup(pool.Close)
	s := newTestServer(t, pool)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/mcp/sse", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		s.HandleSSE(w, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := w.Body.String()
	if !strings.Contains(body, "event: endpoint") {
		t.Errorf("missing endpoint event in body:\n%s", body)
	}
	if !strings.Contains(body, "data: /mcp?sessionId=") {
		t.Errorf(`missing endpoint URI in body:\n%s`, body)
	}
}

// B17.131: through Lens's own middleware, whose writer hides http.Flusher, a client still gets the endpoint event at once.
func TestMCP_SSEEndpointEventReachesClientThroughMiddleware(t *testing.T) {
	pool, _ := pgxmock.NewPool()
	t.Cleanup(pool.Close)
	s := newTestServer(t, pool)
	srv := httptest.NewServer(api.RateLimitHeadersMiddleware(http.HandlerFunc(s.HandleSSE)))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/mcp/sse", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /mcp/sse sent nothing in 3 s, not even its headers: %v", err)
	}
	defer resp.Body.Close()
	var got string
	for buf := make([]byte, 256); !strings.Contains(got, "\n\n"); {
		n, err := resp.Body.Read(buf)
		got += string(buf[:n])
		if err != nil {
			t.Fatalf("stream ended after %q: %v", got, err)
		}
	}
	if !strings.HasPrefix(got, "event: endpoint\ndata: /mcp?sessionId=") {
		t.Errorf("first event = %q, want the endpoint URI", got)
	}
}

func TestMCP_InvalidJSONReturnsParseError(t *testing.T) {
	pool, _ := pgxmock.NewPool()
	t.Cleanup(pool.Close)
	s := newTestServer(t, pool)

	code, body := rpcCallString(t, s, "{not valid json")
	if code != http.StatusOK {
		t.Errorf("status = %d, want 200 (JSON-RPC errors still travel as 200)", code)
	}
	var resp rpcResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("body should be valid JSON-RPC error response; got: %s", body)
	}
	if resp.Error == nil || resp.Error.Code != -32700 {
		t.Errorf("error = %+v, want code -32700", resp.Error)
	}
}

// B40.1: initialize answers the version asked for (or the newest), ping answers, a notification is accepted with
// no body, initialize issues an Mcp-Session-Id, and an unknown MCP-Protocol-Version header is refused.
func TestMCP_StreamableHTTPLifecycle(t *testing.T) {
	s := newServer(nil, nil, nil, nil, nil, "test")
	post := func(body string, header ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		w := httptest.NewRecorder()
		s.HandleRPC(w, req)
		return w
	}
	for asked, want := range map[string]string{"2024-11-05": "2024-11-05", "2025-06-18": "2025-06-18", "2099-01-01": handshakeVersions[0]} {
		w := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + asked + `","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
		if !strings.Contains(w.Body.String(), `"protocolVersion":"`+want+`"`) || w.Header().Get("Mcp-Session-Id") == "" {
			t.Errorf("initialize asking %s = %s (session %q), want %s and a session id", asked, w.Body.String(), w.Header().Get("Mcp-Session-Id"), want)
		}
	}
	if w := post(`{"jsonrpc":"2.0","id":2,"method":"ping"}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"result":{}`) {
		t.Errorf("ping = %d %s", w.Code, w.Body.String())
	}
	if w := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Errorf("notifications/initialized = %d %q, want 202 and no body", w.Code, w.Body.String())
	}
	if w := post(`{"jsonrpc":"2.0","id":3,"method":"ping"}`, "MCP-Protocol-Version", "1999-01-01"); w.Code != http.StatusBadRequest {
		t.Errorf("an unsupported MCP-Protocol-Version = %d, want 400", w.Code)
	}
}

// B40.1: a 2026-07-28 request names its version in _meta and needs no initialize: server/discover lists the versions,
// tools/list answers with resultType and a cache lifetime, a header that disagrees with the body is a 400 HeaderMismatch
// and an unknown version a 400 naming the supported ones.
func TestMCP_StatelessRevision(t *testing.T) {
	s := newServer(nil, nil, nil, nil, nil, "test")
	post := func(method, version, name string) (int, map[string]any) {
		body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"` + version + `"}}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.Header.Set("MCP-Protocol-Version", version)
		req.Header.Set("Mcp-Method", name)
		w := httptest.NewRecorder()
		s.HandleRPC(w, req)
		var resp map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if w.Header().Get("Mcp-Session-Id") != "" {
			t.Errorf("%s minted a session id under 2026-07-28", method)
		}
		return w.Code, resp
	}
	code, resp := post("server/discover", "2026-07-28", "server/discover")
	if res, _ := resp["result"].(map[string]any); code != 200 || res["resultType"] != "complete" || !strings.Contains(fmt.Sprint(res["supportedVersions"]), "2024-11-05") {
		t.Errorf("server/discover = %d %v", code, resp)
	}
	code, resp = post("tools/list", "2026-07-28", "tools/list")
	if res, _ := resp["result"].(map[string]any); code != 200 || res["resultType"] != "complete" || res["cacheScope"] != "private" || res["tools"] == nil {
		t.Errorf("tools/list = %d %v", code, resp)
	}
	if code, resp = post("tools/list", "2026-07-28", "tools/call"); code != 400 || fmt.Sprint(resp["error"].(map[string]any)["code"]) != "-32020" {
		t.Errorf("a mismatched Mcp-Method = %d %v, want 400 -32020", code, resp)
	}
	if code, resp = post("tools/list", "2031-01-01", "tools/list"); code != 400 || fmt.Sprint(resp["error"].(map[string]any)["code"]) != "-32022" {
		t.Errorf("an unknown version = %d %v, want 400 -32022", code, resp)
	}
}

// B40.1: every tool has a title and annotations, and a name of at most 64 characters.
func TestMCP_EveryToolIsAnnotated(t *testing.T) {
	var all []map[string]any
	for _, defs := range [][]map[string]any{toolDefinitions(), agentToolDefinitions(), walletToolDefinitions(), marketToolDefinitions(), roomToolDefinitions()} {
		all = append(all, annotate(defs)...)
	}
	for _, tool := range all {
		name := tool["name"].(string)
		if _, ok := toolAnnotations[name]; !ok || len(name) > 64 || tool["title"] == "" {
			t.Errorf("tool %q has no annotations in toolAnnotations, or a name over 64 characters", name)
		}
	}
	if len(all) != len(toolAnnotations) {
		t.Errorf("%d tools, %d annotated: toolAnnotations names a tool that does not exist", len(all), len(toolAnnotations))
	}
}

// B40.1: a 2024-11-05 stream answers only the credential that opened it; another key naming it is a 404.
func TestMCP_SSEStreamIsBoundToItsCredential(t *testing.T) {
	s := newServer(nil, nil, nil, nil, nil, "test")
	as := func(key string, r *http.Request) *http.Request {
		return r.WithContext(auth.WithAuthContext(r.Context(), &auth.AuthContext{APIKeyID: key, WorkspaceID: "ws"}))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sse := httptest.NewRecorder()
	go s.HandleSSE(sse, as("key-a", httptest.NewRequest(http.MethodGet, "/mcp/sse", nil).WithContext(ctx)))
	var sid string
	for range 100 {
		s.sseMu.Lock()
		for id := range s.sse {
			sid = id
		}
		s.sseMu.Unlock()
		if sid != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	post := func(key string) int {
		w := httptest.NewRecorder()
		s.HandleRPC(w, as(key, httptest.NewRequest(http.MethodPost, "/mcp?sessionId="+sid, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))))
		return w.Code
	}
	if code := post("key-b"); code != http.StatusNotFound {
		t.Errorf("another key posting to the stream = %d, want 404", code)
	}
	if code := post("key-a"); code != http.StatusAccepted {
		t.Errorf("the stream's own key = %d, want 202", code)
	}
}
