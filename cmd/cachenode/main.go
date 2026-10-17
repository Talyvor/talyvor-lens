package main

// talyvor-cachenode — companion mining binary GPU-light operators
// run to contribute cache capacity to the Lens network. Three
// subcommands cover the lifecycle (start/stop/status), two more
// expose live data (stats/flush).
//
// Separate binary from talyvor-node so an operator can opt into
// just one mining track without pulling in the other's deps.

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
)

// heartbeatInterval is 60s — cache nodes are less latency-
// sensitive than inference nodes (no live request to time out)
// so we ping less often to reduce Lens-side load.
const heartbeatInterval = 60 * time.Second

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "start":
		runStart(args)
	case "stop":
		runStop(args)
	case "status":
		runStatus(args)
	case "stats":
		runStats(args)
	case "flush":
		runFlush(args)
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `talyvor-cachenode — cache-mining binary for the Lens network

Usage:
  talyvor-cachenode <command>

Commands:
  start      Register with Lens and start serving cache requests
  stop       Deregister and wipe local state
  status     Show node status + earnings summary
  stats      Show local cache statistics
  flush      Flush local cache contents

Configuration (env vars):
  LENS_URL                Lens server URL
  LENS_API_KEY            API key to authenticate with Lens
  LENS_WORKSPACE_ID       Workspace this node belongs to
  CACHE_NODE_URL          Public URL Lens uses to reach this node
  CACHE_NODE_REDIS_URL    Local Redis connection (e.g. redis://localhost:6379/0)
  CACHE_NODE_MAX_GB       Max cache size in GB (default: 10)
  CACHE_NODE_PORT         Listen port (default: 9091)
  CACHE_NODE_SHARE        Allow cross-workspace serving (default: false)
  CACHE_NODE_TLS_CERT     Path to TLS certificate file (PEM)
  CACHE_NODE_TLS_KEY      Path to TLS private key file (PEM)
  TALYVOR_NODE_DEV_NO_SECRET  true: start and serve with no node secret (local development only)
`)
}

// ─── start ───────────────────────────────────────

func runStart(args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	_ = fs.Parse(args)

	cfg := LoadConfig()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("❌ config error: %v", err)
	}

	rdb, err := connectRedis(cfg.RedisURL)
	if err != nil {
		log.Fatalf("❌ redis: %v", err)
	}
	defer rdb.Close()

	storage := NewCacheStorage(rdb, cfg.MaxCacheGB)

	client := NewLensClient(cfg.LensURL, cfg.LensAPIKey)
	regCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	state, err := client.Register(regCtx, cfg)
	cancel()
	if err != nil {
		log.Fatalf("❌ register with Lens: %v", err)
	}

	log.Printf("📡 Registered with Lens at %s", cfg.LensURL)
	log.Printf("🪪 Node ID: %s", state.NodeID)
	log.Printf("💾 Cache capacity: %.1f GB", cfg.MaxCacheGB)
	if cfg.ShareEnabled {
		log.Printf("🌐 Cross-workspace sharing: enabled")
	}

	if err := checkNodeSecret(state.NodeSecret, cfg.DevNoSecret); err != nil {
		log.Fatalf("❌ %v", err)
	}
	srv := NewCacheServer(storage, state.NodeSecret, cfg)
	httpServer, _ := srv.ListenAndServe(cfg.Port, cfg.TLSCertFile, cfg.TLSKeyFile)

	// Heartbeat loop.
	hbCtx, hbCancel := context.WithCancel(context.Background())
	defer hbCancel()
	go heartbeatLoop(hbCtx, client, state, storage)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Printf("🛑 shutdown signal received — deregistering")

	hbCancel()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	if err := client.Deregister(shutdownCtx, state); err != nil {
		log.Printf("⚠️  deregister failed: %v", err)
	}
	log.Printf("👋 bye")
}

func heartbeatLoop(ctx context.Context, client *LensClient, state CacheNodeState, storage *CacheStorage) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			beatCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			stats, _ := storage.Stats(beatCtx)
			if err := client.Heartbeat(beatCtx, state,
				stats.TotalEntries, stats.SizeMB, stats.HitRate); err != nil {
				log.Printf("⚠️  heartbeat failed: %v", err)
			}
			cancel()
		}
	}
}

// ─── stop ────────────────────────────────────────

func runStop(_ []string) {
	state, err := LoadState()
	if err != nil {
		log.Fatalf("❌ load state: %v", err)
	}
	if state.NodeID == "" {
		log.Printf("⚠️  no registered cache node found; nothing to do")
		return
	}
	apiKey := os.Getenv("LENS_API_KEY")
	if apiKey == "" {
		log.Fatalf("❌ LENS_API_KEY required to deregister")
	}
	client := NewLensClient(state.LensURL, apiKey)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Deregister(ctx, state); err != nil {
		log.Fatalf("❌ deregister: %v", err)
	}
	log.Printf("👋 deregistered cache node %s", state.NodeID)
}

// ─── status ──────────────────────────────────────

func runStatus(_ []string) {
	state, err := LoadState()
	if err != nil || state.NodeID == "" {
		log.Fatalf("❌ no registered cache node found — run `talyvor-cachenode start` first")
	}
	apiKey := os.Getenv("LENS_API_KEY")
	if apiKey == "" {
		log.Fatalf("❌ LENS_API_KEY required to fetch earnings")
	}
	client := NewLensClient(state.LensURL, apiKey)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	earnings, _ := client.Earnings(ctx, state.WorkspaceID)

	var sb strings.Builder
	sb.WriteString("Talyvor Cache Node\n")
	sb.WriteString("──────────────────\n")
	fmt.Fprintf(&sb, "Node ID:        %s\n", state.NodeID)
	fmt.Fprintf(&sb, "Node URL:       %s\n", state.NodeURL)
	fmt.Fprintf(&sb, "Max capacity:   %.1f GB\n", state.MaxCacheGB)
	fmt.Fprintf(&sb, "Lens URL:       %s\n", state.LensURL)
	fmt.Fprintf(&sb, "Registered:     %s\n", state.RegisteredAt.Format("2006-01-02 15:04:05"))
	if earnings != nil {
		if v, ok := earnings["lifetime_earned"].(float64); ok {
			fmt.Fprintf(&sb, "Lifetime LENS:  %.4f LENS ($%.2f)\n", v, v*0.10)
		}
	}
	fmt.Print(sb.String())
}

// ─── stats ───────────────────────────────────────

func runStats(_ []string) {
	cfg := LoadConfig()
	if cfg.RedisURL == "" {
		log.Fatalf("❌ CACHE_NODE_REDIS_URL required for stats")
	}
	rdb, err := connectRedis(cfg.RedisURL)
	if err != nil {
		log.Fatalf("❌ redis: %v", err)
	}
	defer rdb.Close()
	storage := NewCacheStorage(rdb, cfg.MaxCacheGB)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stats, err := storage.Stats(ctx)
	if err != nil {
		log.Fatalf("❌ stats: %v", err)
	}
	fmt.Printf("Cache statistics\n────────────────\n")
	fmt.Printf("Entries:    %d\n", stats.TotalEntries)
	fmt.Printf("Size:       %.2f MB / %.2f MB (%.0f%%)\n",
		stats.SizeMB, stats.MaxSizeMB, 100*stats.SizeMB/stats.MaxSizeMB)
	fmt.Printf("Hit rate:   %.2f%%\n", stats.HitRate*100)
	fmt.Printf("Hits:       %d\n", stats.HitsTotal)
	fmt.Printf("Misses:     %d\n", stats.MissTotal)
}

// ─── flush ───────────────────────────────────────

func runFlush(_ []string) {
	cfg := LoadConfig()
	if cfg.RedisURL == "" {
		log.Fatalf("❌ CACHE_NODE_REDIS_URL required for flush")
	}
	rdb, err := connectRedis(cfg.RedisURL)
	if err != nil {
		log.Fatalf("❌ redis: %v", err)
	}
	defer rdb.Close()
	storage := NewCacheStorage(rdb, cfg.MaxCacheGB)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, err := storage.Flush(ctx)
	if err != nil {
		log.Fatalf("❌ flush: %v", err)
	}
	fmt.Printf("🧹 flushed %d cache entries\n", n)
}

// ─── small helpers ───────────────────────────────

// connectRedis parses the URL and runs a quick PING so a typo in
// the env var fails loud at start-up rather than at first cache
// request.
func connectRedis(url string) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	rdb := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return rdb, nil
}
