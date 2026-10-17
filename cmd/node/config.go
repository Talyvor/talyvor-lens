package main

// config.go — environment-driven configuration for talyvor-node.
// Same pattern as cmd/lens/config-loading: every knob is an env
// var, with sensible defaults where the spec defines them. The
// validation step has its own function so commands like `status`
// + `earnings` can run from a state file without re-validating
// the whole registration surface.

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// NodeConfig captures everything the node needs to operate.
// Loaded from env at start-up.
type NodeConfig struct {
	LensURL       string
	LensAPIKey    string
	WorkspaceID   string
	NodeURL       string
	Provider      string
	Models        []string
	GPUType       string
	MaxConcurrent int
	Port          int
	// TLSCertFile and TLSKeyFile enable HTTPS on the node server
	// (ISO 27001 A.13). When both are set, the server calls
	// ListenAndServeTLS; when absent, it falls back to plain HTTP and
	// logs a startup warning. Self-signed certs are fine for LAN
	// deployments — the only requirement is that Lens can verify them
	// (or is configured to skip verification for trusted private nets).
	TLSCertFile string
	TLSKeyFile  string

	// AttestationEnabled + AttestationCmd arm the Proof-of-Confidential-Compute producer (step a). Default
	// off; when on, AttestationCmd is the operator's NVIDIA attestation tool the daemon shells out to
	// (NODE_ATTESTATION_CMD). Both unset ⇒ no Attestor wired ⇒ /attestation returns 501.
	AttestationEnabled bool
	AttestationCmd     string

	// DevNoSecret (TALYVOR_NODE_DEV_NO_SECRET=true) lets the node start and serve with no node secret.
	// Local development only: without it a node with no secret refuses to start.
	DevNoSecret bool
}

// LoadConfig pulls every value from the environment and applies
// the documented defaults.
func LoadConfig() NodeConfig {
	return NodeConfig{
		LensURL:       os.Getenv("LENS_URL"),
		LensAPIKey:    os.Getenv("LENS_API_KEY"),
		WorkspaceID:   os.Getenv("LENS_WORKSPACE_ID"),
		NodeURL:       os.Getenv("NODE_URL"),
		Provider:      defaultStr(os.Getenv("NODE_PROVIDER"), "ollama"),
		Models:        ParseModels(os.Getenv("NODE_MODELS")),
		GPUType:       defaultStr(os.Getenv("NODE_GPU_TYPE"), "cpu"),
		MaxConcurrent: parseIntDefault("NODE_MAX_CONCURRENT", 4),
		Port:          parseIntDefault("NODE_PORT", 9090),
		TLSCertFile:   os.Getenv("NODE_TLS_CERT"),
		TLSKeyFile:    os.Getenv("NODE_TLS_KEY"),
		// Proof-of-Confidential-Compute (step a) — INERT by default. Both must be set to arm the producer:
		// the capability flag AND the operator's NVIDIA attestation command line.
		AttestationEnabled: os.Getenv("NODE_ATTESTATION_ENABLED") == "true",
		AttestationCmd:     os.Getenv("NODE_ATTESTATION_CMD"),
		DevNoSecret:        os.Getenv("TALYVOR_NODE_DEV_NO_SECRET") == "true",
	}
}

// checkNodeSecret is the start-up gate: a node with no secret would serve any caller.
func checkNodeSecret(secret string, devNoSecret bool) error {
	if secret == "" && !devNoSecret {
		return errors.New("no node secret: refusing to start (set TALYVOR_NODE_DEV_NO_SECRET=true for local development only)")
	}
	return nil
}

// Validate enforces the "required to register" subset of fields.
// Read-only commands (status, earnings) can skip this and load
// the node-state file instead.
func (c NodeConfig) Validate() error {
	var missing []string
	if c.LensURL == "" {
		missing = append(missing, "LENS_URL")
	}
	if c.LensAPIKey == "" {
		missing = append(missing, "LENS_API_KEY")
	}
	if c.WorkspaceID == "" {
		missing = append(missing, "LENS_WORKSPACE_ID")
	}
	if c.NodeURL == "" {
		missing = append(missing, "NODE_URL")
	}
	if len(c.Models) == 0 {
		missing = append(missing, "NODE_MODELS")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	if !validProvider(c.Provider) {
		return fmt.Errorf("invalid NODE_PROVIDER %q (must be ollama / vllm / llamacpp)", c.Provider)
	}
	if !validGPU(c.GPUType) {
		return fmt.Errorf("invalid NODE_GPU_TYPE %q (must be cpu / rtx4090 / a100 / h100)", c.GPUType)
	}
	if c.Port <= 0 || c.Port > 65535 {
		return errors.New("NODE_PORT must be between 1 and 65535")
	}
	if c.MaxConcurrent <= 0 {
		return errors.New("NODE_MAX_CONCURRENT must be positive")
	}
	return nil
}

// ProviderURL is the local provider endpoint we'll forward
// inference requests to. Defaults track the canonical local
// ports for each provider so the operator usually doesn't have
// to set it.
func (c NodeConfig) ProviderURL() string {
	v := os.Getenv("NODE_PROVIDER_URL")
	if v != "" {
		return strings.TrimRight(v, "/")
	}
	switch c.Provider {
	case "ollama":
		return "http://localhost:11434"
	case "vllm":
		return "http://localhost:8000"
	case "llamacpp":
		return "http://localhost:8080"
	}
	return ""
}

// ─── small helpers ───────────────────────────────

// ParseModels splits a comma-separated NODE_MODELS env value
// into a clean slice. Whitespace around each entry is trimmed;
// empties are dropped.
func ParseModels(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, m := range strings.Split(raw, ",") {
		m = strings.TrimSpace(m)
		if m != "" {
			out = append(out, m)
		}
	}
	return out
}

func validProvider(p string) bool {
	switch p {
	case "ollama", "vllm", "llamacpp":
		return true
	}
	return false
}

func validGPU(g string) bool {
	switch strings.ToLower(g) {
	case "cpu", "rtx4090", "a100", "h100":
		return true
	}
	return false
}

func parseIntDefault(env string, def int) int {
	if v := os.Getenv(env); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
