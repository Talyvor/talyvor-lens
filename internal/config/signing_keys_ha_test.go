package config

// signing_keys_ha_test.go — sharp-edge #1: signing-key config must FAIL CLOSED
// instead of silently degrading to per-replica ephemeral keys.
//
// Two contracts, both enforced at Load() (the config chokepoint that already
// hard-errors on invalid values):
//
//  1. HA fail-closed: with LENS_HA_ENABLED, every replica must share STABLE
//     signing keys. An ephemeral per-replica JWT key means tokens minted by one
//     instance are rejected by the others and /v1/auth/jwks advertises the
//     wrong key; an ephemeral challenge key invalidates node-pinned pubkeys.
//     Both previously only logger.Warn'd — works in dev, breaks in prod.
//  2. A SET LENS_POVI_CHALLENGE_KEY must actually parse (base64 ed25519
//     32-byte seed or 64-byte private key) in ANY mode. The old path silently
//     replaced an operator-supplied-but-mistyped key with an ephemeral one
//     (loadOrGenChallengeKey's Warn branch) — the operator did the right thing
//     and still got the broken behavior. Mirrors the existing hard failure for
//     an invalid LENS_JWT_PRIVATE_KEY PEM in main.go.
//
// Load() enforces PRESENCE for the JWT key under HA; PEM parsing stays in
// main.go, which already os.Exit(1)s on an invalid key.

import (
	"encoding/base64"
	"strings"
	"testing"
)

// testJWTPEMPresent is any non-empty value: Load() enforces presence under HA;
// syntactic PEM validation is main.go's existing hard-fail.
const testJWTPEMPresent = "-----BEGIN EC PRIVATE KEY-----\ntest\n-----END EC PRIVATE KEY-----"

func b64Bytes(n int) string { return base64.StdEncoding.EncodeToString(make([]byte, n)) }

func TestLoad_HA_RequiresStableSigningKeys(t *testing.T) {
	t.Run("HA with no JWT key is refused", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_HA_ENABLED", "true")
		t.Setenv("LENS_POVI_CHALLENGE_KEY", b64Bytes(32))
		if _, err := Load(); err == nil {
			t.Fatal("LENS_HA_ENABLED without LENS_JWT_PRIVATE_KEY must be refused at load — an ephemeral per-replica JWT key silently breaks cross-instance token verification")
		}
	})
	t.Run("HA with no POVI challenge key is refused", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_HA_ENABLED", "true")
		t.Setenv("LENS_JWT_PRIVATE_KEY", testJWTPEMPresent)
		if _, err := Load(); err == nil {
			t.Fatal("LENS_HA_ENABLED without LENS_POVI_CHALLENGE_KEY must be refused at load — an ephemeral per-replica challenge key invalidates node-pinned pubkeys")
		}
	})
	t.Run("HA with both keys loads", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_HA_ENABLED", "true")
		t.Setenv("LENS_JWT_PRIVATE_KEY", testJWTPEMPresent)
		t.Setenv("LENS_POVI_CHALLENGE_KEY", b64Bytes(32))
		c, err := Load()
		if err != nil {
			t.Fatalf("Load with HA + both stable keys: %v", err)
		}
		if !c.HAEnabled {
			t.Error("HAEnabled should be true")
		}
	})
	t.Run("single-node with no keys still loads (dev path unchanged)", func(t *testing.T) {
		setRequiredEnv(t)
		if _, err := Load(); err != nil {
			t.Fatalf("single-node Load without signing keys must keep working (ephemeral + warn): %v", err)
		}
	})
}

func TestLoad_POVIChallengeKey_SetButInvalidRejected(t *testing.T) {
	// NOTE: an empty-string env var is indistinguishable from unset via
	// os.Getenv, so "" stays the single-node dev (ephemeral+warn) path by
	// contract — only a NON-EMPTY invalid value is rejected here.
	for name, bad := range map[string]string{
		"not base64": "not-valid-base64!!!",
		"16 bytes":   b64Bytes(16),
		"33 bytes":   b64Bytes(33),
	} {
		t.Run("rejected: "+name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("LENS_POVI_CHALLENGE_KEY", bad)
			if _, err := Load(); err == nil {
				t.Errorf("a SET but invalid LENS_POVI_CHALLENGE_KEY (%s) must be refused at load, not silently replaced with an ephemeral key", name)
			}
		})
	}
	t.Run("valid 32-byte seed accepted", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_POVI_CHALLENGE_KEY", b64Bytes(32))
		if _, err := Load(); err != nil {
			t.Fatalf("valid 32-byte seed rejected: %v", err)
		}
	})
	t.Run("valid 64-byte private key accepted", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_POVI_CHALLENGE_KEY", b64Bytes(64))
		if _, err := Load(); err != nil {
			t.Fatalf("valid 64-byte private key rejected: %v", err)
		}
	})
}

// B50.3 — LENS_ENV=production refuses to start without each signing key Lens would otherwise make afresh at each
// start; with all three it loads; a value that is not a known environment is refused rather than read as development.
func TestLoad_ProductionRequiresEverySigningKey(t *testing.T) {
	keys := map[string]string{
		"LENS_JWT_PRIVATE_KEY":    testJWTPEMPresent,
		"LENS_POVI_CHALLENGE_KEY": b64Bytes(32),
		"LENS_KYA_SIGNING_KEY":    b64Bytes(32),
	}
	for missing := range keys {
		t.Run("without "+missing, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("LENS_ENV", "production")
			for k, v := range keys {
				if k != missing {
					t.Setenv(k, v)
				}
			}
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("LENS_ENV=production without %s: want a refusal naming it, got %v", missing, err)
			}
		})
	}
	t.Run("with every key", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_ENV", "production")
		for k, v := range keys {
			t.Setenv(k, v)
		}
		if c, err := Load(); err != nil || !c.Production {
			t.Fatalf("LENS_ENV=production with every key: got %v, Production=%v", err, c != nil && c.Production)
		}
	})
	t.Run("an unknown LENS_ENV", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_ENV", "prod")
		if _, err := Load(); err == nil {
			t.Fatal(`LENS_ENV="prod" must be refused, not read as development`)
		}
	})
}
