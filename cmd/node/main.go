package main

// talyvor-node — mining binary GPU operators run to contribute
// inference capacity to the Lens network. Commands:
//   start        register with Lens, serve inference, heartbeat
//   stop         deregister + wipe local state
//   status       print the local state + a live health probe
//   models       enumerate models from the configured provider
//   earnings     pretty-print LENS earnings + balance
//
// Separate binary so operators don't need to ship the whole
// Lens proxy + its dependencies on every GPU box.

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// heartbeatInterval matches the spec — 30s between pings.
const heartbeatInterval = 30 * time.Second

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
	case "models":
		runModels(args)
	case "earnings":
		runEarnings(args)
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `talyvor-node — mining binary for the Lens network

Usage:
  talyvor-node <command>

Commands:
  start      Register with Lens and start serving inference
  stop       Deregister and wipe local state
  status     Show node status + live health
  models     List models reported by the local provider
  earnings   Show LENS earnings + balance

Configuration (env vars):
  LENS_URL              Lens server URL
  LENS_API_KEY          API key to authenticate with Lens
  LENS_WORKSPACE_ID     Workspace this node belongs to
  NODE_URL              Public URL Lens uses to reach this node
  NODE_PROVIDER         ollama | vllm | llamacpp (default: ollama)
  NODE_MODELS           comma-separated models (required)
  NODE_GPU_TYPE         cpu | rtx4090 | a100 | h100 (default: cpu)
  NODE_PORT             listen port (default: 9090)
  NODE_MAX_CONCURRENT   max parallel inferences (default: 4)
  NODE_PROVIDER_URL     override the local provider endpoint
  NODE_TLS_CERT         path to TLS certificate file (PEM)
  NODE_TLS_KEY          path to TLS private key file (PEM)
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

	provider, err := NewProvider(cfg.Provider, cfg.ProviderURL())
	if err != nil {
		log.Fatalf("❌ provider: %v", err)
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := provider.Health(probeCtx); err != nil {
		cancel()
		log.Fatalf("❌ local %s provider not reachable at %s: %v", cfg.Provider, cfg.ProviderURL(), err)
	}
	cancel()

	client := NewLensClient(cfg.LensURL, cfg.LensAPIKey)
	regCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	state, err := client.Register(regCtx, cfg)
	cancel()
	if err != nil {
		log.Fatalf("❌ register with Lens: %v", err)
	}

	log.Printf("📡 Registered with Lens at %s", cfg.LensURL)
	log.Printf("🔧 Serving models: %s", strings.Join(cfg.Models, ", "))
	log.Printf("🪪 Node ID: %s", state.NodeID)

	if err := checkNodeSecret(state.NodeSecret, cfg.DevNoSecret); err != nil {
		log.Fatalf("❌ %v", err)
	}
	srv := NewInferenceServer(provider, state.NodeSecret, cfg)
	// Wire PoVI receipt production (Token Economy Phase 1, Part 1). A node
	// without a signing key simply produces no receipts.
	if signer, serr := newReceiptSigner(state); serr != nil {
		log.Printf("node: PoVI receipt signing disabled: %v", serr)
	} else if signer != nil {
		srv.SetReceiptSigner(signer, client)
		log.Printf("🔏 PoVI receipts enabled (attestation + tamper-evidence; minting stays OFF until stake+challenge)")
		// Pin Lens's challenge pubkey so the node can verify + answer Part-3
		// challenges (and refuse forged ones). Best-effort at startup; the
		// heartbeat loop re-fetches so a Lens key rotation is picked up without
		// a restart.
		refreshChallengeKey(context.Background(), client, srv)
		if len(srv.challengeKey()) == 0 {
			log.Printf("node: Lens challenge pubkey not yet available — Part-3 challenges refused (fail-closed) until a heartbeat re-fetch succeeds")
		}
	}
	// Proof-of-Confidential-Compute (step a) — INERT by default. Wire the attestation producer only when the
	// operator opts in with BOTH the flag and a command; otherwise /attestation stays 501. Mints nothing.
	if cfg.AttestationEnabled {
		if at := newExecAttestor(cfg.AttestationCmd); at != nil {
			srv.SetAttestor(at)
			log.Printf("🔐 hardware attestation enabled (Proof-of-Confidential-Compute handshake; minting stays OFF — no mint exists yet)")
		} else {
			log.Printf("node: NODE_ATTESTATION_ENABLED set but NODE_ATTESTATION_CMD empty — /attestation stays inert (501)")
		}
	}
	httpServer, _ := srv.ListenAndServe(cfg.Port, cfg.TLSCertFile, cfg.TLSKeyFile)

	// Heartbeat loop — runs until ctx is cancelled by the
	// signal handler below.
	hbCtx, hbCancel := context.WithCancel(context.Background())
	defer hbCancel()
	go heartbeatLoop(hbCtx, client, state, srv, provider)

	// Block until SIGTERM/SIGINT.
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

// refreshChallengeKey fetches Lens's challenge-signing pubkey and pins it,
// swapping ONLY when a valid, DIFFERENT key arrives (a key rotation). A failed
// fetch is non-fatal and leaves the currently-pinned key in place, so a
// transient Lens blip doesn't break challenge-answering. A node that has never
// successfully fetched stays unpinned (handleChallenge then fails closed).
func refreshChallengeKey(ctx context.Context, client *LensClient, srv *InferenceServer) {
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pub, err := client.FetchChallengePubKey(fetchCtx)
	if err != nil {
		return // keep last-known-good (silent — the heartbeat retries)
	}
	cur := srv.challengeKey()
	if bytes.Equal(cur, pub) {
		return // unchanged
	}
	srv.SetChallengePubKey(pub)
	if len(cur) == 0 {
		log.Printf("🛡️  PoVI challenge verification enabled (Lens-signed challenges)")
	} else {
		log.Printf("🔄 Lens challenge key rotated — re-pinned new key")
	}
}

// heartbeatLoop pings Lens every 30s with the live counters.
// Failures are logged but never fatal — Lens marks us inactive
// after 90s of silence so a transient blip recovers naturally.
func heartbeatLoop(ctx context.Context, client *LensClient, state NodeState, srv *InferenceServer, p Provider) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			beatCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			models, _ := p.ListModels(beatCtx)
			if err := client.Heartbeat(beatCtx, state,
				srv.ActiveRequests(), srv.UptimeSeconds(), models); err != nil {
				log.Printf("⚠️  heartbeat failed: %v", err)
			}
			cancel()
			// PoVI Part 3: re-fetch the Lens challenge pubkey on the heartbeat
			// cadence so a key rotation is picked up without a node restart.
			// Last-known-good on failure (a transient Lens blip must not break
			// challenge-answering). Only relevant once the node produces
			// receipts (has a signer).
			if srv.signer != nil {
				refreshChallengeKey(ctx, client, srv)
			}
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
		log.Printf("⚠️  no registered node found; nothing to do")
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
	log.Printf("👋 deregistered node %s", state.NodeID)
}

// ─── status ──────────────────────────────────────

func runStatus(_ []string) {
	state, err := LoadState()
	if err != nil || state.NodeID == "" {
		log.Fatalf("❌ no registered node found — run `talyvor-node start` first")
	}
	cfg := LoadConfig()
	provider, err := NewProvider(state.Provider, cfg.ProviderURL())
	if err != nil {
		log.Fatalf("❌ provider: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	healthy := provider.Health(ctx) == nil
	fmt.Print(FormatStatus(state, healthy))
}

// ─── models ──────────────────────────────────────

func runModels(_ []string) {
	cfg := LoadConfig()
	if cfg.Provider == "" {
		cfg.Provider = "ollama"
	}
	provider, err := NewProvider(cfg.Provider, cfg.ProviderURL())
	if err != nil {
		log.Fatalf("❌ provider: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	models, err := provider.ListModels(ctx)
	if err != nil {
		log.Fatalf("❌ list models: %v", err)
	}
	if len(models) == 0 {
		fmt.Println("(no models reported by provider)")
		return
	}
	for _, m := range models {
		fmt.Println(m)
	}
}

// ─── earnings ────────────────────────────────────

func runEarnings(_ []string) {
	state, err := LoadState()
	if err != nil || state.NodeID == "" {
		log.Fatalf("❌ no registered node found — run `talyvor-node start` first")
	}
	apiKey := os.Getenv("LENS_API_KEY")
	if apiKey == "" {
		log.Fatalf("❌ LENS_API_KEY required to query earnings")
	}
	client := NewLensClient(state.LensURL, apiKey)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	earnings, err := client.Earnings(ctx, state.WorkspaceID)
	if err != nil {
		log.Fatalf("❌ earnings: %v", err)
	}
	balance, err := client.Balance(ctx, state.WorkspaceID)
	if err != nil {
		// Balance is optional — render with what we have.
		log.Printf("⚠️  balance lookup failed: %v", err)
		balance = map[string]any{}
	}
	_ = errors.Is // keep stdlib referenced if we later add error-class checks
	fmt.Print(FormatEarnings(state, earnings, balance))
}
