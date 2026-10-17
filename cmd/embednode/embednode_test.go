package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ─── EmbedNodeConfig.Validate ────────────────────

func TestEmbedNodeConfig_Validate(t *testing.T) {
	// Empty config → error.
	if err := (EmbedNodeConfig{}).Validate(); err == nil {
		t.Fatal("expected error for empty config")
	}
	// Full config → pass.
	good := EmbedNodeConfig{
		LensURL: "http://l", LensAPIKey: "k", WorkspaceID: "ws",
		NodeURL: "http://node", Model: "nomic-embed-text",
		Dimensions: 768, MaxBatch: 100, Port: 9092,
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("expected pass, got %v", err)
	}
}

func TestEmbedNodeConfig_RejectsUnknownModel(t *testing.T) {
	cfg := EmbedNodeConfig{
		LensURL: "http://l", LensAPIKey: "k", WorkspaceID: "ws",
		NodeURL: "http://node", Model: "fake-embed",
		Dimensions: 768, MaxBatch: 100, Port: 9092,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestEmbedNodeConfig_RejectsInvalidDimensions(t *testing.T) {
	cfg := EmbedNodeConfig{
		LensURL: "http://l", LensAPIKey: "k", WorkspaceID: "ws",
		NodeURL: "http://node", Model: "nomic-embed-text",
		Dimensions: 512, MaxBatch: 100, Port: 9092,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for non-allowlisted dimensions")
	}
}

// ─── Backend adapter ─────────────────────────────

func TestOpenAICompatBackend_HitsCorrectEndpoint(t *testing.T) {
	var hitPath string
	var receivedTexts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		var body struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		receivedTexts = body.Input
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]},{"embedding":[0.4,0.5,0.6]}]}`))
	}))
	defer srv.Close()
	backend := NewOpenAICompatBackend(srv.URL)
	embeddings, err := backend.Embed(context.Background(), "test-model",
		[]string{"hello", "world"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if hitPath != "/v1/embeddings" {
		t.Fatalf("expected /v1/embeddings hit, got %q", hitPath)
	}
	if len(receivedTexts) != 2 || receivedTexts[0] != "hello" {
		t.Fatalf("unexpected payload texts: %v", receivedTexts)
	}
	if len(embeddings) != 2 {
		t.Fatalf("expected 2 embeddings, got %d", len(embeddings))
	}
}

func TestOllamaBackend_LoopsPerText(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"embedding":[0.1,0.2]}`))
	}))
	defer srv.Close()
	backend := NewOllamaBackend(srv.URL)
	embeddings, err := backend.Embed(context.Background(), "m",
		[]string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 backend calls (one per text), got %d", calls)
	}
	if len(embeddings) != 3 {
		t.Fatalf("expected 3 embeddings, got %d", len(embeddings))
	}
}

func TestDetectBackend_PrefersOllama(t *testing.T) {
	var ollamaTagsHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			ollamaTagsHit = true
			_, _ = w.Write([]byte(`{"models":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	backend, err := DetectBackend(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("DetectBackend: %v", err)
	}
	if !ollamaTagsHit {
		t.Fatal("expected /api/tags probe to be hit first")
	}
	if backend.Name() != "ollama" {
		t.Fatalf("expected ollama backend, got %q", backend.Name())
	}
}

func TestDetectBackend_FallsBackToOpenAICompat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	backend, err := DetectBackend(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("DetectBackend: %v", err)
	}
	if backend.Name() != "openai-compat" {
		t.Fatalf("expected openai-compat backend, got %q", backend.Name())
	}
}

// ─── EmbedServer ─────────────────────────────────

type stubBackend struct {
	dim     int
	calls   int
	failErr error
}

func (s *stubBackend) Name() string { return "stub" }
func (s *stubBackend) Embed(_ context.Context, _ string, texts []string) ([][]float64, error) {
	s.calls++
	if s.failErr != nil {
		return nil, s.failErr
	}
	out := make([][]float64, len(texts))
	for i := range texts {
		out[i] = make([]float64, s.dim)
		for j := 0; j < s.dim; j++ {
			out[i][j] = float64(i*10 + j)
		}
	}
	return out, nil
}

func TestEmbedServer_RejectsWrongSecret(t *testing.T) {
	srv := NewEmbedServer(&stubBackend{dim: 4}, "real-secret", EmbedNodeConfig{
		Model: "nomic-embed-text", Dimensions: 768, MaxBatch: 10,
	}, 100)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/embed",
		strings.NewReader(`{"texts":["hi"]}`))
	req.Header.Set("X-Node-Secret", "wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

// B42.3: /embed and /benchmark answer 401 without the secret — also when the node has no secret and no
// dev flag — while /health stays open; with the secret they serve.
func TestEmbedServer_EveryRouteButHealthNeedsSecret(t *testing.T) {
	cfg := EmbedNodeConfig{Model: "nomic-embed-text", Dimensions: 768, MaxBatch: 10}
	do := func(ts *httptest.Server, method, path, secret string) int {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(`{"texts":["hi"]}`))
		if secret != "" {
			req.Header.Set("X-Node-Secret", secret)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, secret := range []string{"real-secret", ""} {
		ts := httptest.NewServer(NewEmbedServer(&stubBackend{dim: 4}, secret, cfg, 100).Handler())
		if got := do(ts, http.MethodPost, "/embed", ""); got != http.StatusUnauthorized {
			t.Errorf("node secret %q: /embed without secret got %d, want 401", secret, got)
		}
		if got := do(ts, http.MethodGet, "/benchmark", ""); got != http.StatusUnauthorized {
			t.Errorf("node secret %q: /benchmark without secret got %d, want 401", secret, got)
		}
		if got := do(ts, http.MethodGet, "/health", ""); got != http.StatusOK {
			t.Errorf("node secret %q: /health got %d, want 200", secret, got)
		}
		ts.Close()
	}
	ts := httptest.NewServer(NewEmbedServer(&stubBackend{dim: 4}, "real-secret", cfg, 100).Handler())
	defer ts.Close()
	if got := do(ts, http.MethodPost, "/embed", "real-secret"); got != http.StatusOK {
		t.Errorf("/embed with secret got %d, want 200", got)
	}
	if got := do(ts, http.MethodGet, "/benchmark", "real-secret"); got != http.StatusOK {
		t.Errorf("/benchmark with secret got %d, want 200", got)
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

func TestEmbedServer_ProcessesBatch(t *testing.T) {
	srv := NewEmbedServer(&stubBackend{dim: 4}, "", EmbedNodeConfig{DevNoSecret: true,
		Model: "nomic-embed-text", Dimensions: 768, MaxBatch: 10,
	}, 100)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	body := `{"texts":["a","b","c"]}`
	resp, err := http.Post(ts.URL+"/embed", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out struct {
		Embeddings [][]float64 `json:"embeddings"`
		Model      string      `json:"model"`
		Dimensions int         `json:"dimensions"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Embeddings) != 3 {
		t.Fatalf("expected 3 embeddings, got %d", len(out.Embeddings))
	}
	if out.Model != "nomic-embed-text" {
		t.Fatalf("expected echoed model, got %q", out.Model)
	}
}

func TestEmbedServer_RejectsOversizeBatch(t *testing.T) {
	srv := NewEmbedServer(&stubBackend{dim: 4}, "", EmbedNodeConfig{DevNoSecret: true,
		Model: "nomic-embed-text", Dimensions: 768, MaxBatch: 2,
	}, 100)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/embed", "application/json",
		strings.NewReader(`{"texts":["a","b","c"]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestEmbedServer_HealthShape(t *testing.T) {
	srv := NewEmbedServer(&stubBackend{dim: 4}, "", EmbedNodeConfig{DevNoSecret: true,
		Model: "nomic-embed-text", Dimensions: 768, MaxBatch: 10,
	}, 42)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["model"] != "nomic-embed-text" {
		t.Fatalf("expected model echoed, got %v", out["model"])
	}
	if int(out["dimensions"].(float64)) != 768 {
		t.Fatalf("expected dimensions 768, got %v", out["dimensions"])
	}
}

// ─── Benchmark ───────────────────────────────────

func TestRunBenchmark_ReturnsPositive(t *testing.T) {
	tps, err := RunBenchmark(context.Background(), &stubBackend{dim: 4}, "m")
	if err != nil {
		t.Fatalf("RunBenchmark: %v", err)
	}
	if tps <= 0 {
		t.Fatalf("expected positive tps, got %d", tps)
	}
}

// ─── NodeState round-trip ───────────────────────

func TestEmbedNodeState_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TALYVOR_EMBEDNODE_STATE", tmp+"/state.json")
	in := EmbedNodeState{
		NodeID: "en1", NodeSecret: "secret", WorkspaceID: "ws",
		LensURL: "http://l", NodeURL: "http://node",
		Model: "nomic-embed-text", Dimensions: 768, MaxBatch: 100,
		SpeedTPS: 240, RegisteredAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := SaveState(in); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	out, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if out.NodeID != in.NodeID || out.SpeedTPS != in.SpeedTPS {
		t.Fatalf("round-trip mismatch: %+v vs %+v", in, out)
	}
	_ = ClearState()
	out, _ = LoadState()
	if out.NodeID != "" {
		t.Fatal("expected empty state after clear")
	}
}

// ─── TLS config loading ──────────────────────────

func TestEmbedNodeConfig_LoadsTLSEnvVars(t *testing.T) {
	t.Setenv("EMBED_NODE_TLS_CERT", "/certs/embed.pem")
	t.Setenv("EMBED_NODE_TLS_KEY", "/certs/embed.key")
	cfg := LoadConfig()
	if cfg.TLSCertFile != "/certs/embed.pem" {
		t.Fatalf("expected TLSCertFile, got %q", cfg.TLSCertFile)
	}
	if cfg.TLSKeyFile != "/certs/embed.key" {
		t.Fatalf("expected TLSKeyFile, got %q", cfg.TLSKeyFile)
	}
}

func TestEmbedNodeConfig_TLSDefaultsEmpty(t *testing.T) {
	t.Setenv("EMBED_NODE_TLS_CERT", "")
	t.Setenv("EMBED_NODE_TLS_KEY", "")
	cfg := LoadConfig()
	if cfg.TLSCertFile != "" || cfg.TLSKeyFile != "" {
		t.Fatal("expected empty TLS fields when env vars are absent")
	}
}

// ─── EmbedServer TLS ─────────────────────────────

func TestEmbedServer_ServesOverTLS(t *testing.T) {
	srv := NewEmbedServer(&stubBackend{dim: 4}, "", EmbedNodeConfig{DevNoSecret: true,
		Model: "nomic-embed-text", Dimensions: 768, MaxBatch: 10,
	}, 100)
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
