package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/keypool"
)

// operator_reads_handlers_test.go — what GET /v1/api/keys/pool and
// GET /v1/api/fallback/chains put on the wire, and for whom.
//
// Both routes are admin-gated since B42.1, like their mutating siblings; the assertions
// below are about WHAT is exposed to the admin, and one is a tripwire on key material.

const probeProviderSecret = "sk-live-DO-NOT-SHIP-THIS-abcdef0123456789"

func poolWithASecret(t *testing.T) *keypool.Pool {
	t.Helper()
	p := keypool.New()
	pk, err := p.Add("openai", probeProviderSecret, "primary-billing-key", 100)
	if err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	// ⚠ ARMED AT THE SOURCE. "the response does not contain the secret" is worthless
	// if the pool never held it, so assert the key material is really in there
	// before any test asserts it is not on the wire.
	if pk.Key != probeProviderSecret {
		t.Fatalf("the pool did not store the probe secret (got %q) — every absence assertion "+
			"downstream would then be vacuous", pk.Key)
	}
	return p
}

func getJSON(t *testing.T, h http.HandlerFunc, target string) (string, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String(), rec
}

// ⚠ THE TRIPWIRE, AND IT NEEDS NO DECISION FROM ANYONE.
// keypool.PoolKey holds the real provider key in `Key` and carries NO json tags at
// all, so anything that marshals PoolKey instead of KeyStats would put every
// provider credential on a route any tenant can call. KeyStats omits it today; this
// is what keeps that true.
func TestKeyPoolStats_NeverCarriesKeyMaterial(t *testing.T) {
	body, _ := getJSON(t, newKeyPoolStatsHandler(poolWithASecret(t)), "/v1/api/keys/pool")

	// (poolWithASecret has already asserted the pool really holds the key material.)
	var stats []keypool.KeyStats
	if err := json.Unmarshal([]byte(body), &stats); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	if len(stats) != 1 || stats[0].Alias != "primary-billing-key" {
		t.Fatalf("the fixture did not reach the response (%d rows) — an empty response satisfies "+
			"every assertion below while proving nothing", len(stats))
	}

	for _, needle := range []string{probeProviderSecret, "sk-live", "DO-NOT-SHIP-THIS"} {
		if strings.Contains(body, needle) {
			t.Errorf("GET /v1/api/keys/pool put provider KEY MATERIAL (%q) on the wire. This route "+
				"is reachable by any tenant's API key.\n    body=%s", needle, body)
		}
	}
	// Raw-substring, not field-name: a rename must not launder it.
	if strings.Contains(strings.ToLower(body), `"key"`) {
		t.Errorf("the response carries a \"key\" field: %s", body)
	}
}

// ⚠ THE RECORD, NOT AN APPROVAL. This is what an ordinary tenant credential can
// read today. It is pinned so that (a) widening it fires, and (b) whoever takes
// W6.18's decision can see exactly what was on the table.
func TestKeyPoolStats_WhatATenantCanReadToday(t *testing.T) {
	body, _ := getJSON(t, newKeyPoolStatsHandler(poolWithASecret(t)), "/v1/api/keys/pool")

	var rows []map[string]any
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	got := make([]string, 0, len(rows[0]))
	for k := range rows[0] {
		got = append(got, k)
	}
	want := map[string]bool{
		"id": true, "provider": true, "alias": true, "request_count": true,
		"error_count": true, "error_rate": true, "healthy": true, "last_used_at": true,
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("GET /v1/api/keys/pool now exposes %q, which W6.18 did not measure and "+
				"nobody decided about. This route has no gate: any tenant reads it.", k)
		}
	}
	for k := range want {
		if _, ok := rows[0][k]; !ok {
			t.Errorf("the recorded exposure no longer includes %q — if that was deliberate, "+
				"update W6.18's record with the reason", k)
		}
	}
	t.Logf("a tenant credential reads, for every pooled OPERATOR key: %s", strings.Join(got, ", "))
}

func TestFallbackChains_WhatATenantCanReadToday(t *testing.T) {
	r := fallback.New()
	r.SetChain("openai", []fallback.FallbackTarget{
		{Provider: "anthropic", Model: "claude-3-haiku", Priority: 1},
	})
	body, _ := getJSON(t, newFallbackChainsHandler(r), "/v1/api/fallback/chains")

	var chains map[string][]map[string]any
	if err := json.Unmarshal([]byte(body), &chains); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	// NON-VACUITY: the chain reached the response.
	if len(chains["openai"]) != 1 {
		t.Fatalf("the fixture chain did not reach the response: %s", body)
	}
	for k := range chains["openai"][0] {
		switch k {
		case "provider", "model", "priority":
		default:
			t.Errorf("GET /v1/api/fallback/chains now exposes %q per target, which W6.18 did not "+
				"measure. This route has no gate while its PUT sibling is requireAdmin.", k)
		}
	}
	// It carries routing CONFIGURATION and no tenant data — which is why W6.18 calls
	// this the weaker half of the pair and says so rather than implying otherwise.
	t.Logf("a tenant credential reads the operator's full fallback configuration: %s", body)
}

// ── the wiring ──

// ⚠ BOTH OF THESE WERE strings.Contains OVER THE RAW SOURCE UNTIL #526, so a COMMENT carrying
// the old registration text answered them. Measured: DELETE /v1/api/keys/pool/{keyID} stripped of
// requireAdmin, with the admin-gated form left in a comment, kept
// TestOperatorReadWriteAsymmetryIsStillWhatW618Recorded GREEN — a route that REMOVES A PROVIDER
// API KEY FROM THE POOL, reachable by any authenticated tenant, while the record says the
// asymmetry holds. Registrations, their handlers and their gates are now read from the AST.
// Arms: ~/talyvor-queue/w61-operatorkeys-controls-h2r7.py.

// operatorRouteRegs parses main.go once for both guards below.
func operatorRouteRegs(t *testing.T) []routeReg {
	t.Helper()
	regs, _, _, err := scanRouteRegistrations("main.go", []byte(readMainGo(t)))
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	if len(regs) < 100 {
		t.Fatalf("only %d route registrations parsed from main.go — the scan is blind", len(regs))
	}
	return regs
}

// findRoute returns the registration for verb+path.
func findRoute(regs []routeReg, verb, path string) (routeReg, bool) {
	for _, r := range regs {
		if r.verb == verb && r.path == path {
			return r, true
		}
	}
	return routeReg{}, false
}

func TestOperatorReadsGoThroughTheirNamedHandlers(t *testing.T) {
	regs := operatorRouteRegs(t)
	for _, want := range []struct{ verb, path, handler string }{
		{"Get", "/v1/api/keys/pool", "newKeyPoolStatsHandler"},
		{"Get", "/v1/api/fallback/chains", "newFallbackChainsHandler"},
	} {
		r, ok := findRoute(regs, want.verb, want.path)
		if !ok {
			t.Errorf("main.go does not register %s %s at all", want.verb, want.path)
			continue
		}
		if !r.wrapsCall(want.handler) {
			t.Errorf("main.go does not register %s %s through the named handler %s — handler is %s",
				want.verb, want.path, want.handler, r.handler)
		}
	}
}

// W6.18's asymmetry, answered by B42.1: the operator reads are admin-gated like their writes.
// GET /v1/local/endpoints is the same decision on the same kind of process-wide config.
func TestOperatorReadsAreAdminGatedLikeTheirWrites(t *testing.T) {
	regs := operatorRouteRegs(t)
	for _, c := range []struct{ verb, path string }{
		{"Post", "/v1/api/keys/pool"},
		{"Delete", "/v1/api/keys/pool/{keyID}"},
		{"Put", "/v1/api/fallback/chains/{provider}"},
		{"Get", "/v1/api/keys/pool"},
		{"Get", "/v1/api/fallback/chains"},
		{"Get", "/v1/local/endpoints"},
	} {
		r, ok := findRoute(regs, c.verb, c.path)
		if !ok {
			t.Errorf("%s %s is not registered at all", c.verb, c.path)
			continue
		}
		if !r.wrapsCall("requireAdmin") {
			t.Errorf("%s %s is not admin-gated (main.go line %d, handler %s): any workspace's key "+
				"reads Lens's process-wide configuration", c.verb, c.path, r.line, r.handler)
		}
	}
}
