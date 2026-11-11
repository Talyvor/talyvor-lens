package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/poolsafety"
)

// B58.14: the nightly's pool-isolation check could not tell "pooling is off" from "a miss",
// because nothing a workspace's token can read said which. GET /v1/pooling says it, and why.

func readPoolState(t *testing.T, h http.Handler, workspaceID string) (map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/pooling", nil)
	req = req.WithContext(auth.WithAPIKey(req.Context(), &auth.APIKey{WorkspaceID: workspaceID}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/pooling = %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %v: %s", err, rec.Body.String())
	}
	return body, rec.Body.String()
}

func TestPoolState_FlagOff_SaysOffAndWhy(t *testing.T) {
	cfg := &config.Config{EconomyEnabled: true, CachePoolableEnabled: false}
	body, raw := readPoolState(t, poolStateHandler(cfg, poolsafety.NewGate()), "ws-a")
	if body["pooling"] != "off" || body["cause"] != "flag" {
		t.Fatalf("pooling switched off in config must read off/flag, got %s", raw)
	}
	if !strings.Contains(body["reason"].(string), "LENS_CACHE_POOLABLE_ENABLED") {
		t.Errorf("reason does not name the switch: %s", raw)
	}

	cfg.EconomyEnabled = false
	body, raw = readPoolState(t, poolStateHandler(cfg, poolsafety.NewGate()), "ws-a")
	if body["pooling"] != "off" || body["cause"] != "economy_off" {
		t.Fatalf("pooling forced off with the economy must say so, got %s", raw)
	}
}

func TestPoolState_AttestationOff_SaysOffAndTheGatesReason(t *testing.T) {
	cfg := &config.Config{EconomyEnabled: true, CachePoolableEnabled: true, EmbeddingModel: "text-embedding-3-small", SemanticThreshold: 0.98}
	gate := poolsafety.NewGate()
	db := &stubReader{row: stubRow{err: poolsafety.ErrNoAttestation}}
	gate.Refresh(context.Background(), db, cfg.EmbeddingModel, cfg.SemanticThreshold)

	body, raw := readPoolState(t, poolStateHandler(cfg, gate), "ws-a")
	if body["pooling"] != "off" || body["cause"] != "attestation" {
		t.Fatalf("pooling held off by the attestation must read off/attestation, got %s", raw)
	}
	if body["reason"] != gate.Reason() {
		t.Errorf("reason %q is not the gate's reason %q", body["reason"], gate.Reason())
	}

	db.row = stubRow{vals: []any{"text-embedding-3-small", 0.98, "p", 0.6534}}
	gate.Refresh(context.Background(), db, cfg.EmbeddingModel, cfg.SemanticThreshold)
	body, raw = readPoolState(t, poolStateHandler(cfg, gate), "ws-a")
	if body["pooling"] != "on" {
		t.Fatalf("an attested gate with the flag on must read on, got %s", raw)
	}
	if _, ok := body["reason"]; ok {
		t.Errorf("pooling on carries a reason, which reads as a problem: %s", raw)
	}
}

// A database error can name a host; the read says only that the attestation could not be read.
func TestPoolState_UnreadableAttestation_CarriesNoDriverText(t *testing.T) {
	cfg := &config.Config{EconomyEnabled: true, CachePoolableEnabled: true, EmbeddingModel: "m", SemanticThreshold: 0.98}
	gate := poolsafety.NewGate()
	gate.Refresh(context.Background(), &stubReader{row: stubRow{err: errors.New("dial tcp db-internal.example:5432: refused")}}, cfg.EmbeddingModel, cfg.SemanticThreshold)

	body, raw := readPoolState(t, poolStateHandler(cfg, gate), "ws-a")
	if body["pooling"] != "off" || body["cause"] != "attestation" {
		t.Fatalf("got %s", raw)
	}
	if strings.Contains(raw, "db-internal") || body["reason"] != poolsafety.ReasonUnreadable {
		t.Errorf("the driver's error reached a workspace's token: %s", raw)
	}
}

// Tenancy: any workspace reads the same deployment state, and nothing of any workspace.
func TestPoolState_EveryWorkspaceReadsTheSameDeploymentState(t *testing.T) {
	cfg := &config.Config{EconomyEnabled: true, CachePoolableEnabled: false}
	h := poolStateHandler(cfg, poolsafety.NewGate())
	a, rawA := readPoolState(t, h, "ws-a")
	_, rawB := readPoolState(t, h, "ws-b")
	if rawA != rawB {
		t.Fatalf("two workspaces read different answers:\n%s\n%s", rawA, rawB)
	}
	for k := range a {
		switch k {
		case "pooling", "cause", "reason":
		default:
			t.Errorf("unexpected field %q: the read carries the deployment's state only", k)
		}
	}
	if strings.Contains(rawA, "ws-a") {
		t.Errorf("the caller's workspace appears in the answer: %s", rawA)
	}
}
