package main

// server.go — HTTP server the embedding-node binary spins up.
// Lens routes embedding calls here when it picks a network node;
// every route but /health needs the X-Node-Secret (nothing in Lens calls /embed today).

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

// EmbedServer is the HTTP layer in front of the backend. Owns
// the shared secret + a small atomic in-flight counter for the
// health endpoint.
type EmbedServer struct {
	backend  Backend
	secret   string
	cfg      EmbedNodeConfig
	speedTPS int64 // observed embeddings/sec from the benchmark

	startedAt time.Time
	inflight  int64
}

func NewEmbedServer(backend Backend, secret string, cfg EmbedNodeConfig, speedTPS int64) *EmbedServer {
	return &EmbedServer{
		backend:   backend,
		secret:    secret,
		cfg:       cfg,
		speedTPS:  speedTPS,
		startedAt: time.Now(),
	}
}

// Handler wires the routes onto a fresh ServeMux.
func (s *EmbedServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/embed", s.handleEmbed)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/benchmark", s.handleBenchmark)
	return mux
}

// ─── /embed ──────────────────────────────────────

func (s *EmbedServer) handleEmbed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !nodeSecretOK(r, s.secret, s.cfg.DevNoSecret) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var in struct {
		Texts []string `json:"texts"`
		Model string   `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(in.Texts) == 0 {
		http.Error(w, "texts required", http.StatusBadRequest)
		return
	}
	if len(in.Texts) > s.cfg.MaxBatch {
		http.Error(w, fmt.Sprintf("batch size %d exceeds max %d", len(in.Texts), s.cfg.MaxBatch),
			http.StatusBadRequest)
		return
	}
	model := in.Model
	if model == "" {
		model = s.cfg.Model
	}

	atomic.AddInt64(&s.inflight, 1)
	defer atomic.AddInt64(&s.inflight, -1)

	embeddings, err := s.backend.Embed(r.Context(), model, in.Texts)
	if err != nil {
		http.Error(w, "embed failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"embeddings": embeddings,
		"model":      model,
		"dimensions": s.cfg.Dimensions,
	})
}

// nodeSecretOK checks X-Node-Secret in constant time. An empty secret admits nobody, unless devNoSecret.
func nodeSecretOK(r *http.Request, secret string, devNoSecret bool) bool {
	if secret == "" {
		return devNoSecret
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Node-Secret")), []byte(secret)) == 1
}

// ─── /health ─────────────────────────────────────

func (s *EmbedServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         "healthy",
		"model":          s.cfg.Model,
		"dimensions":     s.cfg.Dimensions,
		"max_batch":      s.cfg.MaxBatch,
		"speed_tps":      atomic.LoadInt64(&s.speedTPS),
		"inflight":       atomic.LoadInt64(&s.inflight),
		"backend":        s.backend.Name(),
		"uptime_seconds": int64(time.Since(s.startedAt).Seconds()),
	})
}

// ─── /benchmark ──────────────────────────────────

// handleBenchmark embeds a fixed set of test strings and reports
// the observed throughput. Useful for operators tuning their
// rig + for the marketplace surface (speed_tps shows up on
// EmbeddingNode.SpeedTPS).
func (s *EmbedServer) handleBenchmark(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !nodeSecretOK(r, s.secret, s.cfg.DevNoSecret) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tps, err := RunBenchmark(r.Context(), s.backend, s.cfg.Model)
	if err != nil {
		http.Error(w, "benchmark failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	atomic.StoreInt64(&s.speedTPS, tps)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"speed_tps": tps,
		"model":     s.cfg.Model,
	})
}

// ─── Benchmark helper (used by main.go on startup) ───

// benchmarkCorpus is the fixed input set for RunBenchmark. 100
// short strings exercise the backend without taking minutes
// even on CPU-only rigs.
var benchmarkCorpus = func() []string {
	out := make([]string, 100)
	for i := range out {
		out[i] = fmt.Sprintf("benchmark sample text number %d — the quick brown fox jumps over the lazy dog", i)
	}
	return out
}()

// RunBenchmark embeds 100 short strings and returns the observed
// throughput in embeddings/second. The result is what we report
// to Lens via the heartbeat speed_tps field.
func RunBenchmark(ctx context.Context, backend Backend, model string) (int64, error) {
	if backend == nil {
		return 0, fmt.Errorf("nil backend")
	}
	bctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := backend.Embed(bctx, model, benchmarkCorpus); err != nil {
		return 0, err
	}
	elapsed := time.Since(start)
	if elapsed <= 0 {
		elapsed = time.Nanosecond
	}
	return int64(float64(len(benchmarkCorpus)) / elapsed.Seconds()), nil
}

// ─── ListenAndServe ──────────────────────────────

// ListenAndServe starts the embedding-node HTTP(S) server. When
// certFile and keyFile are both non-empty the server binds with TLS
// (ISO 27001 A.13); otherwise it falls back to plain HTTP with a
// startup warning.
func (s *EmbedServer) ListenAndServe(port int, certFile, keyFile string) (*http.Server, error) {
	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if certFile != "" && keyFile != "" {
		go func() {
			log.Printf("✅ Talyvor Embedding Node listening (TLS) on port %d", port)
			if err := server.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
				log.Printf("embednode: HTTPS server error: %v", err)
			}
		}()
	} else {
		log.Printf("⚠️  embednode TLS is disabled — X-Node-Secret is transmitted in cleartext; " +
			"set EMBED_NODE_TLS_CERT + EMBED_NODE_TLS_KEY to enable (ISO 27001 A.13)")
		go func() {
			log.Printf("✅ Talyvor Embedding Node listening on port %d", port)
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("embednode: HTTP server error: %v", err)
			}
		}()
	}
	return server, nil
}

// SpeedTPS exposes the latest observed throughput so the
// heartbeat goroutine can pass it along to Lens.
func (s *EmbedServer) SpeedTPS() int64 {
	return atomic.LoadInt64(&s.speedTPS)
}
