package main

// server.go — HTTP server the cache-node binary spins up. Nothing
// in Lens calls these routes today (see README.md): every route but
// /health needs the X-Node-Secret, and /cache/:key also needs the
// caller's X-Workspace-ID, checked against the key's owner.

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// CacheServer wraps a CacheStorage in an HTTP handler.
type CacheServer struct {
	storage *CacheStorage
	secret  string
	cfg     CacheNodeConfig

	startedAt time.Time
}

func NewCacheServer(storage *CacheStorage, secret string, cfg CacheNodeConfig) *CacheServer {
	return &CacheServer{
		storage:   storage,
		secret:    secret,
		cfg:       cfg,
		startedAt: time.Now(),
	}
}

// Handler wires the routes onto a fresh ServeMux. Kept separate
// from ListenAndServe so tests can mount it on a httptest.Server.
func (s *CacheServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/cache/", s.handleCache)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/stats", s.handleStats)
	return mux
}

// ─── /cache/:key ─────────────────────────────────

// handleCache multiplexes GET / POST / DELETE under /cache/:key.
// The X-Node-Secret check runs first; an empty configured secret
// admits nobody unless the node runs with TALYVOR_NODE_DEV_NO_SECRET.
// X-Workspace-ID names the workspace the call is for.
func (s *CacheServer) handleCache(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/cache/")
	if key == "" {
		http.Error(w, "key required", http.StatusBadRequest)
		return
	}
	ws := r.Header.Get("X-Workspace-ID")
	if ws == "" {
		http.Error(w, "X-Workspace-ID required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.cacheGet(w, r, key, ws)
	case http.MethodPost:
		s.cachePost(w, r, key, ws)
	case http.MethodDelete:
		s.cacheDelete(w, r, key, ws)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// authorized checks X-Node-Secret in constant time.
func (s *CacheServer) authorized(r *http.Request) bool {
	if s.secret == "" {
		return s.cfg.DevNoSecret
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Node-Secret")), []byte(s.secret)) == 1
}

// otherOwner reports whether key exists and belongs to a workspace other than ws.
func (s *CacheServer) otherOwner(r *http.Request, key, ws string) (bool, error) {
	owner, err := s.storage.Owner(r.Context(), key)
	return owner != "" && owner != ws, err
}

func (s *CacheServer) cacheGet(w http.ResponseWriter, r *http.Request, key, ws string) {
	val, owner, err := s.storage.Get(r.Context(), key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Another workspace's entry reads as a miss unless the operator shares across workspaces.
	if val == "" || (owner != ws && !s.cfg.ShareEnabled) {
		// Miss → 404 per spec.
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache-Owner", owner)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"value":           val,
		"owner_workspace": owner,
	})
}

func (s *CacheServer) cachePost(w http.ResponseWriter, r *http.Request, key, ws string) {
	var in struct {
		Value          string `json:"value"`
		TTLSeconds     int    `json:"ttl_seconds"`
		OwnerWorkspace string `json:"owner_workspace"`
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &in); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if in.Value == "" {
		http.Error(w, "value required", http.StatusBadRequest)
		return
	}
	if in.OwnerWorkspace != "" && in.OwnerWorkspace != ws {
		http.Error(w, "owner_workspace must be the caller's workspace", http.StatusForbidden)
		return
	}
	// shortcut: check-then-set is not atomic; a Lua CAS if these routes ever get a caller.
	if other, err := s.otherOwner(r, key, ws); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if other {
		http.Error(w, "key belongs to another workspace", http.StatusForbidden)
		return
	}
	if in.TTLSeconds <= 0 {
		in.TTLSeconds = 3600
	}
	ttl := time.Duration(in.TTLSeconds) * time.Second
	if err := s.storage.Set(r.Context(), key, in.Value, ws, ttl); err != nil {
		// Capacity-exhaustion errors are a 507 Insufficient Storage
		// per RFC 4918 — bridges nicely to the cache-full
		// constraint enforced by storage.Set.
		if strings.Contains(err.Error(), "cache full") {
			http.Error(w, err.Error(), http.StatusInsufficientStorage)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (s *CacheServer) cacheDelete(w http.ResponseWriter, r *http.Request, key, ws string) {
	if other, err := s.otherOwner(r, key, ws); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if other {
		http.Error(w, "key belongs to another workspace", http.StatusForbidden)
		return
	}
	if err := s.storage.Delete(r.Context(), key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── /health ─────────────────────────────────────

func (s *CacheServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	stats, err := s.storage.Stats(r.Context())
	status := "healthy"
	if err != nil {
		status = "unhealthy"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         status,
		"entries":        stats.TotalEntries,
		"size_mb":        stats.SizeMB,
		"hit_rate":       stats.HitRate,
		"uptime_seconds": int64(time.Since(s.startedAt).Seconds()),
	})
}

// ─── /stats ──────────────────────────────────────

func (s *CacheServer) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	stats, err := s.storage.Stats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stats)
}

// ─── ListenAndServe ──────────────────────────────

// ListenAndServe starts the cache-node HTTP(S) server. When certFile
// and keyFile are both non-empty the server binds with TLS (ISO 27001
// A.13); otherwise it falls back to plain HTTP with a startup warning.
func (s *CacheServer) ListenAndServe(port int, certFile, keyFile string) (*http.Server, error) {
	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if certFile != "" && keyFile != "" {
		go func() {
			log.Printf("✅ Talyvor Cache Node listening (TLS) on port %d", port)
			if err := server.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
				log.Printf("cachenode: HTTPS server error: %v", err)
			}
		}()
	} else {
		log.Printf("⚠️  cachenode TLS is disabled — X-Node-Secret is transmitted in cleartext; " +
			"set CACHE_NODE_TLS_CERT + CACHE_NODE_TLS_KEY to enable (ISO 27001 A.13)")
		go func() {
			log.Printf("✅ Talyvor Cache Node listening on port %d", port)
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("cachenode: HTTP server error: %v", err)
			}
		}()
	}
	return server, nil
}
