package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// ─── CacheNodeConfig.Validate ────────────────────

func TestCacheNodeConfigValidate(t *testing.T) {
	if err := (CacheNodeConfig{}).Validate(); err == nil {
		t.Fatal("expected error for empty config")
	}
	good := CacheNodeConfig{
		LensURL: "http://l", LensAPIKey: "k", WorkspaceID: "ws",
		NodeURL: "http://node", RedisURL: "redis://x",
		MaxCacheGB: 10, Port: 9091,
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("expected pass with full config, got %v", err)
	}
}

func TestCacheNodeConfigValidate_RejectsBadGB(t *testing.T) {
	cfg := CacheNodeConfig{
		LensURL: "http://l", LensAPIKey: "k", WorkspaceID: "ws",
		NodeURL: "http://node", RedisURL: "redis://x",
		MaxCacheGB: 0, Port: 9091,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for non-positive max_cache_gb")
	}
}

// ─── CacheStorage.Get / Set / Delete ─────────────

func TestCacheStorage_GetMissReturnsEmpty(t *testing.T) {
	storage := NewCacheStorage(newRedis(t), 1)
	val, owner, err := storage.Get(context.Background(), "absent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "" || owner != "" {
		t.Fatalf("expected ('','',nil), got (%q,%q)", val, owner)
	}
}

func TestCacheStorage_SetStoresWithOwner(t *testing.T) {
	storage := NewCacheStorage(newRedis(t), 1)
	ctx := context.Background()
	if err := storage.Set(ctx, "k", "value-bytes", "ws_owner", time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	val, owner, err := storage.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "value-bytes" || owner != "ws_owner" {
		t.Fatalf("round-trip mismatch: val=%q owner=%q", val, owner)
	}
}

func TestCacheStorage_Delete(t *testing.T) {
	storage := NewCacheStorage(newRedis(t), 1)
	ctx := context.Background()
	_ = storage.Set(ctx, "k", "v", "ws", time.Minute)
	if err := storage.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	val, _, _ := storage.Get(ctx, "k")
	if val != "" {
		t.Fatal("expected empty after delete")
	}
}

func TestCacheStorage_Stats_ReportsSize(t *testing.T) {
	storage := NewCacheStorage(newRedis(t), 1)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		_ = storage.Set(ctx, "k"+string(rune('0'+i)), strings.Repeat("x", 1024), "ws", time.Minute)
	}
	stats, err := storage.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.TotalEntries != 10 {
		t.Fatalf("expected 10 entries, got %d", stats.TotalEntries)
	}
	if stats.SizeMB <= 0 {
		t.Fatalf("expected positive size, got %f", stats.SizeMB)
	}
	if stats.MaxSizeMB != 1024 {
		t.Fatalf("expected MaxSizeMB 1024 (1GB), got %f", stats.MaxSizeMB)
	}
}

func TestCacheStorage_HitMissCounters(t *testing.T) {
	storage := NewCacheStorage(newRedis(t), 1)
	ctx := context.Background()
	_ = storage.Set(ctx, "k", "v", "ws", time.Minute)
	_, _, _ = storage.Get(ctx, "k")     // hit
	_, _, _ = storage.Get(ctx, "miss1") // miss
	_, _, _ = storage.Get(ctx, "miss2") // miss
	stats, _ := storage.Stats(ctx)
	if stats.HitsTotal != 1 || stats.MissTotal != 2 {
		t.Fatalf("expected 1 hit + 2 misses, got %d / %d", stats.HitsTotal, stats.MissTotal)
	}
	if stats.HitRate < 0.33 || stats.HitRate > 0.34 {
		t.Fatalf("expected ~0.333 hit rate, got %f", stats.HitRate)
	}
}

// ─── CacheServer ─────────────────────────────────

const testSecret = "real-secret"

func newServer(t *testing.T, cfg CacheNodeConfig) (*httptest.Server, *CacheStorage) {
	storage := NewCacheStorage(newRedis(t), 1)
	srv := httptest.NewServer(NewCacheServer(storage, testSecret, cfg).Handler())
	t.Cleanup(srv.Close)
	return srv, storage
}

// call sends method path as workspace ws with the given secret ("" sends none).
func call(t *testing.T, srv *httptest.Server, method, path, secret, ws, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if secret != "" {
		req.Header.Set("X-Node-Secret", secret)
	}
	if ws != "" {
		req.Header.Set("X-Workspace-ID", ws)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestCacheServer_GetReturns404OnMiss(t *testing.T) {
	srv, _ := newServer(t, CacheNodeConfig{})
	if resp := call(t, srv, http.MethodGet, "/cache/missing", testSecret, "ws_a", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestCacheServer_PostStoresAndGetRetrieves(t *testing.T) {
	srv, _ := newServer(t, CacheNodeConfig{})
	postBody := `{"value":"hello","ttl_seconds":300,"owner_workspace":"ws_a"}`
	if resp := call(t, srv, http.MethodPost, "/cache/mykey", testSecret, "ws_a", postBody); resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	resp := call(t, srv, http.MethodGet, "/cache/mykey", testSecret, "ws_a", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body struct {
		Value          string `json:"value"`
		OwnerWorkspace string `json:"owner_workspace"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Value != "hello" || body.OwnerWorkspace != "ws_a" {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestCacheServer_RejectsWrongSecret(t *testing.T) {
	srv, _ := newServer(t, CacheNodeConfig{})
	if resp := call(t, srv, http.MethodGet, "/cache/anything", "wrong", "ws_a", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestCacheServer_AcceptsRightSecret(t *testing.T) {
	srv, storage := newServer(t, CacheNodeConfig{})
	_ = storage.Set(context.Background(), "k", "v", "ws", time.Minute)
	if resp := call(t, srv, http.MethodGet, "/cache/k", testSecret, "ws", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// B42.3: every route but /health answers 401 without the secret.
func TestCacheServer_EveryRouteButHealthNeedsSecret(t *testing.T) {
	srv, _ := newServer(t, CacheNodeConfig{})
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/cache/k"}, {http.MethodPost, "/cache/k"}, {http.MethodDelete, "/cache/k"}, {http.MethodGet, "/stats"},
	} {
		if resp := call(t, srv, c.method, c.path, "", "ws_a", `{"value":"v"}`); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without secret: got %d, want 401", c.method, c.path, resp.StatusCode)
		}
	}
	if resp := call(t, srv, http.MethodGet, "/health", "", "", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("/health without secret: got %d, want 200", resp.StatusCode)
	}
}

// B42.3: an empty secret admits nobody, unless the node runs in development mode.
func TestCacheServer_EmptySecretAdmitsNobodyOutsideDev(t *testing.T) {
	storage := NewCacheStorage(newRedis(t), 1)
	locked := httptest.NewServer(NewCacheServer(storage, "", CacheNodeConfig{}).Handler())
	t.Cleanup(locked.Close)
	if resp := call(t, locked, http.MethodGet, "/stats", "", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no secret, no dev flag: got %d, want 401", resp.StatusCode)
	}
	dev := httptest.NewServer(NewCacheServer(storage, "", CacheNodeConfig{DevNoSecret: true}).Handler())
	t.Cleanup(dev.Close)
	if resp := call(t, dev, http.MethodGet, "/stats", "", "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("dev flag: got %d, want 200", resp.StatusCode)
	}
}

// B42.3: workspace B can neither read, overwrite nor delete workspace A's key.
func TestCacheServer_OwnerCheck(t *testing.T) {
	srv, storage := newServer(t, CacheNodeConfig{})
	ctx := context.Background()
	_ = storage.Set(ctx, "k", "a-secret-answer", "ws_a", time.Minute)

	if resp := call(t, srv, http.MethodGet, "/cache/k", testSecret, "ws_b", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("B reads A's key: got %d, want 404", resp.StatusCode)
	}
	if resp := call(t, srv, http.MethodPost, "/cache/k", testSecret, "ws_b", `{"value":"b"}`); resp.StatusCode != http.StatusForbidden {
		t.Errorf("B overwrites A's key: got %d, want 403", resp.StatusCode)
	}
	if resp := call(t, srv, http.MethodPost, "/cache/k2", testSecret, "ws_b", `{"value":"b","owner_workspace":"ws_a"}`); resp.StatusCode != http.StatusForbidden {
		t.Errorf("B writes as A: got %d, want 403", resp.StatusCode)
	}
	if resp := call(t, srv, http.MethodDelete, "/cache/k", testSecret, "ws_b", ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("B deletes A's key: got %d, want 403", resp.StatusCode)
	}
	if resp := call(t, srv, http.MethodGet, "/cache/k", testSecret, "", ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("no X-Workspace-ID: got %d, want 400", resp.StatusCode)
	}
	if val, owner, _ := storage.Get(ctx, "k"); val != "a-secret-answer" || owner != "ws_a" {
		t.Fatalf("A's entry changed: %q owned by %q", val, owner)
	}
	if resp := call(t, srv, http.MethodDelete, "/cache/k", testSecret, "ws_a", ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("A deletes its own key: got %d, want 204", resp.StatusCode)
	}

	shared, sharedStorage := newServer(t, CacheNodeConfig{ShareEnabled: true})
	_ = sharedStorage.Set(ctx, "k", "v", "ws_a", time.Minute)
	if resp := call(t, shared, http.MethodGet, "/cache/k", testSecret, "ws_b", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("CACHE_NODE_SHARE: B reads A's key: got %d, want 200", resp.StatusCode)
	}
}

func TestCheckNodeSecret(t *testing.T) {
	if checkNodeSecret("", false) == nil {
		t.Fatal("no secret and no dev flag must refuse to start")
	}
	if err := checkNodeSecret("", true); err != nil {
		t.Fatalf("dev flag must start: %v", err)
	}
	if err := checkNodeSecret("s", false); err != nil {
		t.Fatalf("a secret must start: %v", err)
	}
	t.Setenv("TALYVOR_NODE_DEV_NO_SECRET", "true")
	if !LoadConfig().DevNoSecret {
		t.Fatal("TALYVOR_NODE_DEV_NO_SECRET=true must set DevNoSecret")
	}
}

func TestCacheServer_Health(t *testing.T) {
	srv, _ := newServer(t, CacheNodeConfig{})
	resp := call(t, srv, http.MethodGet, "/health", "", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["status"] != "healthy" {
		t.Fatalf("expected healthy, got %v", out["status"])
	}
}

func TestCacheServer_Stats(t *testing.T) {
	srv, storage := newServer(t, CacheNodeConfig{})
	_ = storage.Set(context.Background(), "k1", strings.Repeat("a", 512), "ws", time.Minute)
	resp := call(t, srv, http.MethodGet, "/stats", testSecret, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out CacheStats
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.TotalEntries != 1 {
		t.Fatalf("expected 1 entry, got %d", out.TotalEntries)
	}
}

// ─── NodeState round-trip ───────────────────────

func TestCacheNodeState_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TALYVOR_CACHENODE_STATE", tmp+"/state.json")
	in := CacheNodeState{
		NodeID: "cn1", NodeSecret: "secret", WorkspaceID: "ws",
		LensURL: "http://l", NodeURL: "http://node",
		MaxCacheGB: 10, RegisteredAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := SaveState(in); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	out, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if out.NodeID != in.NodeID || out.NodeSecret != in.NodeSecret {
		t.Fatalf("round-trip mismatch: %+v vs %+v", in, out)
	}
	if err := ClearState(); err != nil {
		t.Fatalf("ClearState: %v", err)
	}
	out, _ = LoadState()
	if out.NodeID != "" {
		t.Fatal("expected empty state after clear")
	}
}

// ─── TLS config loading ──────────────────────────

func TestCacheNodeConfig_LoadsTLSEnvVars(t *testing.T) {
	t.Setenv("CACHE_NODE_TLS_CERT", "/certs/cache.pem")
	t.Setenv("CACHE_NODE_TLS_KEY", "/certs/cache.key")
	cfg := LoadConfig()
	if cfg.TLSCertFile != "/certs/cache.pem" {
		t.Fatalf("expected TLSCertFile, got %q", cfg.TLSCertFile)
	}
	if cfg.TLSKeyFile != "/certs/cache.key" {
		t.Fatalf("expected TLSKeyFile, got %q", cfg.TLSKeyFile)
	}
}

func TestCacheNodeConfig_TLSDefaultsEmpty(t *testing.T) {
	t.Setenv("CACHE_NODE_TLS_CERT", "")
	t.Setenv("CACHE_NODE_TLS_KEY", "")
	cfg := LoadConfig()
	if cfg.TLSCertFile != "" || cfg.TLSKeyFile != "" {
		t.Fatal("expected empty TLS fields when env vars are absent")
	}
}

// ─── CacheServer TLS ─────────────────────────────

func TestCacheServer_ServesOverTLS(t *testing.T) {
	storage := NewCacheStorage(newRedis(t), 1)
	srv := NewCacheServer(storage, testSecret, CacheNodeConfig{})
	ts := httptest.NewTLSServer(srv.Handler())
	defer ts.Close()

	client := ts.Client()
	resp, err := client.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("HTTPS GET /health: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}
