package main

// talyvor-embednode — third standalone mining binary. CPU-friendly
// embedding farm — operators with spare CPU run this to serve
// embedding requests for other workspaces and earn LENS.
//
// Subcommands: start / stop / status / benchmark.

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
)

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
	case "benchmark":
		runBenchmark(args)
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `talyvor-embednode — embedding-mining binary for the Lens network

Usage:
  talyvor-embednode <command>

Commands:
  start       Register with Lens and serve embedding requests
  stop        Deregister and wipe local state
  status      Show node status + speed
  benchmark   Run a speed test locally and print the result

Configuration (env vars):
  LENS_URL                 Lens server URL
  LENS_API_KEY             API key to authenticate with Lens
  LENS_WORKSPACE_ID        Workspace this node belongs to
  EMBED_NODE_URL           Public URL Lens uses to reach this node
  EMBED_NODE_MODEL         nomic-embed-text | e5-large | mxbai-embed-large
                           | text-embedding-3-small | text-embedding-3-large
  EMBED_NODE_DIMENSIONS    768 | 1024 | 1536 (default: 1536)
  EMBED_NODE_MAX_BATCH     max embeddings per request (default: 100)
  EMBED_NODE_BACKEND       local embedding-server URL (default: http://localhost:11434)
  EMBED_NODE_PORT          listen port (default: 9092)
  EMBED_NODE_TLS_CERT      path to TLS certificate file (PEM)
  EMBED_NODE_TLS_KEY       path to TLS private key file (PEM)
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

	// Auto-detect the backend (ollama vs openai-compat).
	detectCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	backend, err := DetectBackend(detectCtx, cfg.BackendURL)
	cancel()
	if err != nil {
		log.Fatalf("❌ backend: %v", err)
	}
	log.Printf("🔌 Detected backend: %s @ %s", backend.Name(), cfg.BackendURL)

	// Benchmark — mandatory per spec ("Benchmark runs on
	// startup (no option to skip)").
	log.Printf("🧪 Running 100-embedding benchmark…")
	bctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	speedTPS, err := RunBenchmark(bctx, backend, cfg.Model)
	cancel()
	if err != nil {
		log.Fatalf("❌ benchmark failed: %v", err)
	}
	log.Printf("📊 Benchmark: %d embeddings/sec", speedTPS)

	client := NewLensClient(cfg.LensURL, cfg.LensAPIKey)
	regCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	state, err := client.Register(regCtx, cfg, speedTPS)
	cancel()
	if err != nil {
		log.Fatalf("❌ register with Lens: %v", err)
	}

	log.Printf("📡 Registered with Lens at %s", cfg.LensURL)
	log.Printf("🪪 Node ID: %s", state.NodeID)
	log.Printf("📚 Model: %s (%dD, batch %d)", cfg.Model, cfg.Dimensions, cfg.MaxBatch)

	if err := checkNodeSecret(state.NodeSecret, cfg.DevNoSecret); err != nil {
		log.Fatalf("❌ %v", err)
	}
	srv := NewEmbedServer(backend, state.NodeSecret, cfg, speedTPS)
	httpServer, _ := srv.ListenAndServe(cfg.Port, cfg.TLSCertFile, cfg.TLSKeyFile)

	hbCtx, hbCancel := context.WithCancel(context.Background())
	defer hbCancel()
	go heartbeatLoop(hbCtx, client, state, srv)

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

func heartbeatLoop(ctx context.Context, client *LensClient, state EmbedNodeState, srv *EmbedServer) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			beatCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := client.Heartbeat(beatCtx, state, srv.SpeedTPS(), 0); err != nil {
				// Older Lens deployments don't have the heartbeat
				// endpoint yet — log + continue.
				log.Printf("⚠️  heartbeat failed (likely older Lens): %v", err)
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
		log.Printf("⚠️  no registered embedding node found; nothing to do")
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
	log.Printf("👋 deregistered embedding node %s", state.NodeID)
}

// ─── status ──────────────────────────────────────

func runStatus(_ []string) {
	state, err := LoadState()
	if err != nil || state.NodeID == "" {
		log.Fatalf("❌ no registered node — run `talyvor-embednode start` first")
	}
	var sb strings.Builder
	sb.WriteString("Talyvor Embedding Node\n")
	sb.WriteString("──────────────────────\n")
	fmt.Fprintf(&sb, "Node ID:        %s\n", state.NodeID)
	fmt.Fprintf(&sb, "Workspace:      %s\n", state.WorkspaceID)
	fmt.Fprintf(&sb, "Node URL:       %s\n", state.NodeURL)
	fmt.Fprintf(&sb, "Model:          %s\n", state.Model)
	fmt.Fprintf(&sb, "Dimensions:     %d\n", state.Dimensions)
	fmt.Fprintf(&sb, "Max batch:      %d\n", state.MaxBatch)
	fmt.Fprintf(&sb, "Speed (TPS):    %d\n", state.SpeedTPS)
	fmt.Fprintf(&sb, "Registered:     %s\n", state.RegisteredAt.Format("2006-01-02 15:04:05"))
	fmt.Print(sb.String())
}

// ─── benchmark ───────────────────────────────────

func runBenchmark(_ []string) {
	cfg := LoadConfig()
	if cfg.Model == "" {
		log.Fatalf("❌ EMBED_NODE_MODEL required for benchmark")
	}
	if cfg.BackendURL == "" {
		log.Fatalf("❌ EMBED_NODE_BACKEND required for benchmark")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	backend, err := DetectBackend(ctx, cfg.BackendURL)
	cancel()
	if err != nil {
		log.Fatalf("❌ backend: %v", err)
	}
	bctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	speedTPS, err := RunBenchmark(bctx, backend, cfg.Model)
	if err != nil {
		log.Fatalf("❌ benchmark: %v", err)
	}
	fmt.Printf("📊 %d embeddings/sec using %s backend (model %s)\n",
		speedTPS, backend.Name(), cfg.Model)
}
