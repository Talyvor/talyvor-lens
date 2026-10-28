package main

// server.go — the local HTTP server the node binary spins up.
// Lens makes inference requests against this endpoint when it
// routes work to the operator's GPU. Three routes:
//   POST /inference    (Lens's node token or X-Node-Secret)
//   POST /challenge    (signed by Lens)
//   POST /attestation  (signed by Lens)
//   GET  /health, /models, /v1/models  (health checks — Lens probes /v1/models without a secret)

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/talyvor/lens/internal/povi"
)

// InferenceServer is the small HTTP layer in front of the local
// provider. Holds the secret used to verify Lens-originated
// requests and a counter of in-flight calls for /health.
type InferenceServer struct {
	provider Provider
	secret   string
	cfg      NodeConfig

	// signer (optional) produces a signed PoVI receipt per served request;
	// lens (optional) submits it to Lens for verification + audit. Both nil
	// for a node without a signing key.
	signer *receiptSigner
	lens   *LensClient
	// attestor (optional) produces a hardware attestation report (Proof-of-Confidential-Compute, step a).
	// nil ⇒ /attestation returns 501 (the default, inert posture). See attestation.go.
	attestor Attestor
	// challengePub is Lens's pinned challenge-signing public key (PoVI Part 3).
	// The node verifies every /challenge is signed by Lens before answering, so
	// arbitrary callers can't extract its served-response content.
	challengePub ed25519.PublicKey
	// challengeRefetch re-fetches Lens's current challenge pubkey on demand (wired from
	// the LensClient in SetReceiptSigner). now is an injectable clock for tests. A nil
	// challengeRefetch disables reactive re-fetch (the node still re-pins on the 30s
	// heartbeat).
	challengeRefetch func(context.Context) (ed25519.PublicKey, error)
	now              func() time.Time

	mu             sync.Mutex
	activeRequests int64
	// lastReactiveRefetch rate-limits the on-challenge key re-fetch (mu-guarded).
	lastReactiveRefetch time.Time
	startedAt           time.Time
	// modelsCache is refreshed each /models call (kept simple —
	// the heartbeat will re-fetch every 30s anyway).
	modelsCache []string
}

// SetReceiptSigner wires PoVI receipt production. A nil signer disables
// receipts (older nodes); a nil lens disables the async submit (the receipt is
// still returned in the response).
func (s *InferenceServer) SetReceiptSigner(rs *receiptSigner, lens *LensClient) {
	s.signer = rs
	s.lens = lens
	if lens != nil {
		s.challengeRefetch = lens.FetchChallengePubKey
	}
}

// SetChallengePubKey pins Lens's challenge-signing public key (PoVI Part 3).
// Safe to call concurrently with handleChallenge (the heartbeat loop re-fetches
// and may rotate the key while requests are being served).
func (s *InferenceServer) SetChallengePubKey(pub ed25519.PublicKey) {
	s.mu.Lock()
	s.challengePub = pub
	s.mu.Unlock()
}

// challengeKey returns the currently-pinned Lens challenge pubkey.
func (s *InferenceServer) challengeKey() ed25519.PublicKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.challengePub
}

// reactiveRefetch{MinInterval,Timeout} bound the on-challenge key re-fetch.
const (
	reactiveRefetchMinInterval = 5 * time.Second
	reactiveRefetchTimeout     = 5 * time.Second
)

func (s *InferenceServer) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// reactiveRefetchChallengeKey re-fetches Lens's current challenge pubkey and re-pins
// it, returning the new key. RATE-LIMITED to at most one re-fetch per
// reactiveRefetchMinInterval, so a flood of unverifiable (e.g. forged) challenges can't
// make the node hammer Lens / DoS itself. Returns ok=false when rate-limited, no
// refetcher is wired, or the fetch fails — the caller then 401s.
func (s *InferenceServer) reactiveRefetchChallengeKey(ctx context.Context) (ed25519.PublicKey, bool) {
	s.mu.Lock()
	refetch := s.challengeRefetch
	now := s.clock()
	if refetch == nil || (!s.lastReactiveRefetch.IsZero() && now.Sub(s.lastReactiveRefetch) < reactiveRefetchMinInterval) {
		s.mu.Unlock()
		return nil, false
	}
	s.lastReactiveRefetch = now
	s.mu.Unlock()

	fctx, cancel := context.WithTimeout(ctx, reactiveRefetchTimeout)
	defer cancel()
	pub, err := refetch(fctx)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, false
	}
	s.SetChallengePubKey(pub)
	return pub, true
}

func NewInferenceServer(provider Provider, secret string, cfg NodeConfig) *InferenceServer {
	return &InferenceServer{
		provider:  provider,
		secret:    secret,
		cfg:       cfg,
		startedAt: time.Now(),
		now:       time.Now,
	}
}

// Handler wires the three routes onto an http.ServeMux. Kept
// separate from ListenAndServe so tests can mount it on a
// httptest.Server.
func (s *InferenceServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/inference", s.handleInference)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/models", s.handleModels)
	// /v1/models is the OpenAI/vllm-convention alias — the gateway's localRouterMulti health-probes
	// a "vllm" endpoint at /v1/models (multi.go), so a registered node must answer it to be picked
	// by SelectEndpoint for auto-route. Same handler as /models.
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/challenge", s.handleChallenge)
	mux.HandleFunc("/attestation", s.handleAttestation)
	return mux
}

// SetAttestor wires the hardware-attestation producer (Proof-of-Confidential-Compute, step a). A nil
// attestor keeps /attestation inert (501). main wires it only when NODE_ATTESTATION_ENABLED + a command.
func (s *InferenceServer) SetAttestor(a Attestor) { s.attestor = a }

// handleAttestation answers a gateway nonce challenge with the node's hardware attestation: it relays the
// nonce to the Attestor (NVIDIA producer), then binds the returned EAT to THIS node with the node's ed25519
// receipt key (povi.SignAttestation) so the gateway gets both node identity + hardware attestation. INERT
// by default: nil attestor ⇒ 501. The daemon does NOT verify the EAT (the NVIDIA signature is checked
// gateway-side, step b) — it only relays + node-signs. Mints nothing.
func (s *InferenceServer) handleAttestation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.attestor == nil {
		http.Error(w, "node produces no attestation", http.StatusNotImplemented) // 501 — inert default
		return
	}
	if s.signer == nil {
		http.Error(w, "node has no signing key to bind the attestation", http.StatusServiceUnavailable)
		return
	}
	var req povi.AttestationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Only Lens may make the node spend a hardware attestation: same pinned key and rotation re-fetch as /challenge.
	pub := s.challengeKey()
	if len(pub) != ed25519.PublicKeySize {
		http.Error(w, "node has no pinned Lens challenge key", http.StatusServiceUnavailable)
		return
	}
	if povi.VerifyAttestationRequest(req, pub) != nil {
		refreshed, ok := s.reactiveRefetchChallengeKey(r.Context())
		if !ok || povi.VerifyAttestationRequest(req, refreshed) != nil {
			http.Error(w, "unauthorized attestation request", http.StatusUnauthorized)
			return
		}
	}
	eat, err := s.attestor.Report(r.Context(), req.Nonce)
	if err != nil {
		http.Error(w, "attestation failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	resp := povi.SignAttestation(s.signer.priv, s.signer.nodeID, req.Nonce, eat)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleChallenge answers a PoVI Part-3 challenge: it verifies the challenge was
// signed by Lens (pinned pubkey), then returns the {leaf, proof} for each
// sampled position from the retained trace. A node that can't answer (trace
// expired, no signer) returns an error → Lens treats it as a failed challenge
// → slash. Failing to verify the signature → 401 (don't leak trace content).
func (s *InferenceServer) handleChallenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.signer == nil {
		http.Error(w, "node produces no receipts", http.StatusBadRequest)
		return
	}
	var req povi.ChallengeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Fail CLOSED: without a pinned Lens key we can't authenticate the caller,
	// so we refuse rather than leak the trace content.
	pub := s.challengeKey()
	if len(pub) != ed25519.PublicKeySize {
		http.Error(w, "node has no pinned Lens challenge key", http.StatusServiceUnavailable)
		return
	}
	if err := povi.VerifyChallenge(req, pub); err != nil {
		// An unverifiable challenge may simply mean Lens rotated its challenge key and
		// we still hold the old one — the propagation race that would otherwise get an
		// HONEST node slashed (Lens reads our 401 as a failed challenge → timeout →
		// slash). Reactively re-fetch Lens's CURRENT key once (rate-limited so a flood
		// of forged challenges can't make us hammer Lens) and retry. A genuinely forged
		// challenge (not signed by Lens) still fails after the re-fetch → 401, so the
		// slash deterrent is unchanged.
		refreshed, ok := s.reactiveRefetchChallengeKey(r.Context())
		if !ok || povi.VerifyChallenge(req, refreshed) != nil {
			http.Error(w, "unauthorized challenge", http.StatusUnauthorized)
			return
		}
	}
	answers, err := s.signer.traces.SampledLeafProofs(req.RequestID, req.Positions)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answers)
}

// ─── inference ───────────────────────────────────

// verifyNodeToken validates a gateway-signed node-auth token (blocker 6 auto-route) against the
// node's PINNED Lens challenge pubkey + its bindings (node_id == this node, body_sha256 == this
// body, not expired). Mirrors handleChallenge: on first failure it reactively re-fetches Lens's
// current key once (rate-limited) to survive a key rotation, then retries. Returns true iff valid.
func (s *InferenceServer) verifyNodeToken(ctx context.Context, tok string, body []byte) bool {
	if s.signer == nil {
		return false // no node identity (no receipts) → not an auto-route target
	}
	pub := s.challengeKey()
	if len(pub) != ed25519.PublicKeySize {
		return false // fail closed: no pinned Lens key
	}
	sum := sha256.Sum256(body)
	bodyHash := hex.EncodeToString(sum[:])
	if povi.VerifyNodeAuthToken(tok, pub, s.signer.nodeID, bodyHash, time.Now()) == nil {
		return true
	}
	// Lens may have rotated its challenge key — reactively re-fetch once (rate-limited) and retry.
	if refreshed, ok := s.reactiveRefetchChallengeKey(ctx); ok {
		return povi.VerifyNodeAuthToken(tok, refreshed, s.signer.nodeID, bodyHash, time.Now()) == nil
	}
	return false
}

func (s *InferenceServer) handleInference(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Read the body once — needed for BOTH the node-auth token's body-hash binding and the decode.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Auth: a valid gateway-signed node-auth token (auto-route, blocker 6) OR the X-Node-Secret. A node
	// with no secret refuses every secret-path request unless it runs with TALYVOR_NODE_DEV_NO_SECRET.
	if tok := r.Header.Get("X-Lens-Node-Token"); tok != "" {
		if !s.verifyNodeToken(r.Context(), tok, body) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	} else if !nodeSecretOK(r, s.secret, s.cfg.DevNoSecret) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req InferRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		http.Error(w, "model required", http.StatusBadRequest)
		return
	}

	atomic.AddInt64(&s.activeRequests, 1)
	defer atomic.AddInt64(&s.activeRequests, -1)

	resp, err := s.provider.Infer(r.Context(), req)
	if err != nil {
		http.Error(w, "inference failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	// PoVI: produce a signed receipt attesting to this response (attestation +
	// tamper-evidence, NOT proof of honest computation). Returned in the body
	// and best-effort submitted to Lens off the response path.
	if s.signer != nil {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			reqID = "req_" + generateSecret()
		}
		rec := s.signer.sign(reqID, req.Model, resp.InputTokens, resp.OutputTokens, resp.Text)
		resp.Receipt = &rec
		if s.lens != nil {
			go func(rc povi.Receipt, ws string) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = s.lens.SubmitReceipt(ctx, ws, rc)
			}(rec, s.signer.workspaceID)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// nodeSecretOK checks X-Node-Secret in constant time. An empty secret admits nobody, unless devNoSecret.
func nodeSecretOK(r *http.Request, secret string, devNoSecret bool) bool {
	if secret == "" {
		return devNoSecret
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Node-Secret")), []byte(secret)) == 1
}

// ─── health ──────────────────────────────────────

func (s *InferenceServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	probeCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	status := "healthy"
	if err := s.provider.Health(probeCtx); err != nil {
		status = "unhealthy"
	}
	out := map[string]any{
		"status":          status,
		"models":          s.cfg.Models,
		"gpu_type":        s.cfg.GPUType,
		"provider":        s.cfg.Provider,
		"active_requests": atomic.LoadInt64(&s.activeRequests),
		"uptime_seconds":  int64(time.Since(s.startedAt).Seconds()),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}

// ─── models ──────────────────────────────────────

func (s *InferenceServer) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	names, err := s.provider.ListModels(ctx)
	if err != nil {
		http.Error(w, "list models: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.mu.Lock()
	s.modelsCache = names
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"models": names})
}

// ─── start / shutdown ────────────────────────────

// ListenAndServe starts the node HTTP(S) server. When certFile and
// keyFile are both non-empty the server binds with TLS (ISO 27001
// A.13 — communications security); otherwise it falls back to plain
// HTTP and logs a startup warning so operators know the X-Node-Secret
// is in transit unencrypted. Returns the http.Server so the caller
// can call Shutdown on SIGTERM.
func (s *InferenceServer) ListenAndServe(port int, certFile, keyFile string) (*http.Server, error) {
	// Inference is capped at inferTimeout and never streams, so the write deadline outlasts the slowest answer.
	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      inferTimeout + 30*time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if certFile != "" && keyFile != "" {
		go func() {
			log.Printf("✅ Talyvor Node listening (TLS) on port %d", port)
			if err := server.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
				log.Printf("node: HTTPS server error: %v", err)
			}
		}()
	} else {
		log.Printf("⚠️  node TLS is disabled — X-Node-Secret is transmitted in cleartext; " +
			"set NODE_TLS_CERT + NODE_TLS_KEY to enable (ISO 27001 A.13)")
		go func() {
			log.Printf("✅ Talyvor Node listening on port %d", port)
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("node: HTTP server error: %v", err)
			}
		}()
	}
	return server, nil
}

// ActiveRequests is a thread-safe accessor used by the heartbeat
// goroutine.
func (s *InferenceServer) ActiveRequests() int64 {
	return atomic.LoadInt64(&s.activeRequests)
}

// UptimeSeconds is similar.
func (s *InferenceServer) UptimeSeconds() int64 {
	return int64(time.Since(s.startedAt).Seconds())
}
