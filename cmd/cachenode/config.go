package main

// config.go — env-driven configuration for talyvor-cachenode.
// Same pattern as cmd/node/config.go: every knob is an env
// var; Validate() enforces what registration needs; read-only
// commands (status, stats) load state via client.go without
// requiring full registration config.

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// CacheNodeConfig is the operator-facing settings bundle.
type CacheNodeConfig struct {
	LensURL      string
	LensAPIKey   string
	WorkspaceID  string
	NodeURL      string
	RedisURL     string
	MaxCacheGB   float64
	Port         int
	ShareEnabled bool
	// TLSCertFile and TLSKeyFile enable HTTPS (ISO 27001 A.13).
	// When both are set, the server calls ListenAndServeTLS; when
	// absent, it falls back to plain HTTP with a startup warning.
	TLSCertFile string
	TLSKeyFile  string

	// DevNoSecret (TALYVOR_NODE_DEV_NO_SECRET=true) lets the node start and serve with no node secret.
	// Local development only: without it a node with no secret refuses to start.
	DevNoSecret bool
}

// LoadConfig pulls every value from the environment and applies
// the documented defaults.
func LoadConfig() CacheNodeConfig {
	return CacheNodeConfig{
		LensURL:      os.Getenv("LENS_URL"),
		LensAPIKey:   os.Getenv("LENS_API_KEY"),
		WorkspaceID:  os.Getenv("LENS_WORKSPACE_ID"),
		NodeURL:      os.Getenv("CACHE_NODE_URL"),
		RedisURL:     os.Getenv("CACHE_NODE_REDIS_URL"),
		MaxCacheGB:   parseFloatDefault("CACHE_NODE_MAX_GB", 10),
		Port:         parseIntDefault("CACHE_NODE_PORT", 9091),
		ShareEnabled: parseBoolEnv("CACHE_NODE_SHARE"),
		TLSCertFile:  os.Getenv("CACHE_NODE_TLS_CERT"),
		TLSKeyFile:   os.Getenv("CACHE_NODE_TLS_KEY"),
		DevNoSecret:  os.Getenv("TALYVOR_NODE_DEV_NO_SECRET") == "true",
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
func (c CacheNodeConfig) Validate() error {
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
		missing = append(missing, "CACHE_NODE_URL")
	}
	if c.RedisURL == "" {
		missing = append(missing, "CACHE_NODE_REDIS_URL")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	if c.MaxCacheGB <= 0 {
		return errors.New("CACHE_NODE_MAX_GB must be positive")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return errors.New("CACHE_NODE_PORT must be between 1 and 65535")
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

func parseFloatDefault(env string, def float64) float64 {
	if v := os.Getenv(env); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return def
}

func parseBoolEnv(env string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(env)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}
