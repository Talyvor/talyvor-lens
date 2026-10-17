package main

// config.go — env-driven configuration for talyvor-embednode.
// Same shape as cmd/node and cmd/cachenode: an env loader + a
// Validate() that enforces the "required to register" subset.

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// knownEmbedModels mirrors the allowlist in
// internal/mining/embedding_mining.go — kept in sync by code
// review since the binary doesn't import that package.
var knownEmbedModels = map[string]bool{
	"nomic-embed-text":       true,
	"e5-large":               true,
	"mxbai-embed-large":      true,
	"text-embedding-3-small": true,
	"text-embedding-3-large": true,
}

var knownEmbedDimensions = map[int]bool{
	768:  true,
	1024: true,
	1536: true,
}

// EmbedNodeConfig is the operator-facing settings bundle.
type EmbedNodeConfig struct {
	LensURL     string
	LensAPIKey  string
	WorkspaceID string
	NodeURL     string
	Model       string
	Dimensions  int
	MaxBatch    int
	BackendURL  string
	Port        int
	// TLSCertFile and TLSKeyFile enable HTTPS (ISO 27001 A.13).
	// When both are set, the server calls ListenAndServeTLS; when
	// absent, it falls back to plain HTTP with a startup warning.
	TLSCertFile string
	TLSKeyFile  string

	// DevNoSecret (TALYVOR_NODE_DEV_NO_SECRET=true) lets the node start and serve with no node secret.
	// Local development only: without it a node with no secret refuses to start.
	DevNoSecret bool
}

// LoadConfig pulls every value from the environment with the
// documented defaults applied.
func LoadConfig() EmbedNodeConfig {
	return EmbedNodeConfig{
		LensURL:     os.Getenv("LENS_URL"),
		LensAPIKey:  os.Getenv("LENS_API_KEY"),
		WorkspaceID: os.Getenv("LENS_WORKSPACE_ID"),
		NodeURL:     os.Getenv("EMBED_NODE_URL"),
		Model:       strings.ToLower(os.Getenv("EMBED_NODE_MODEL")),
		Dimensions:  parseIntDefault("EMBED_NODE_DIMENSIONS", 1536),
		MaxBatch:    parseIntDefault("EMBED_NODE_MAX_BATCH", 100),
		BackendURL:  defaultStr(os.Getenv("EMBED_NODE_BACKEND"), "http://localhost:11434"),
		Port:        parseIntDefault("EMBED_NODE_PORT", 9092),
		TLSCertFile: os.Getenv("EMBED_NODE_TLS_CERT"),
		TLSKeyFile:  os.Getenv("EMBED_NODE_TLS_KEY"),
		DevNoSecret: os.Getenv("TALYVOR_NODE_DEV_NO_SECRET") == "true",
	}
}

// checkNodeSecret is the start-up gate: a node with no secret would serve any caller.
func checkNodeSecret(secret string, devNoSecret bool) error {
	if secret == "" && !devNoSecret {
		return errors.New("no node secret: refusing to start (set TALYVOR_NODE_DEV_NO_SECRET=true for local development only)")
	}
	return nil
}

// Validate enforces the registration-required subset.
func (c EmbedNodeConfig) Validate() error {
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
		missing = append(missing, "EMBED_NODE_URL")
	}
	if c.Model == "" {
		missing = append(missing, "EMBED_NODE_MODEL")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	if !knownEmbedModels[c.Model] {
		return fmt.Errorf("EMBED_NODE_MODEL %q not in allowed list (nomic-embed-text, e5-large, mxbai-embed-large, text-embedding-3-small, text-embedding-3-large)", c.Model)
	}
	if !knownEmbedDimensions[c.Dimensions] {
		return errors.New("EMBED_NODE_DIMENSIONS must be 768, 1024, or 1536")
	}
	if c.MaxBatch <= 0 {
		return errors.New("EMBED_NODE_MAX_BATCH must be positive")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return errors.New("EMBED_NODE_PORT must be between 1 and 65535")
	}
	return nil
}

// ─── small helpers ───────────────────────────────

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
