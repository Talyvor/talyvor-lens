package api

// health.go — detailed /healthz handler. Replaces the boolean
// {"ok":true} probe with a real dependency rollup so an
// operator can tell at a glance which subsystem is degraded.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// HealthChecker is the contract the handler uses to query each
// subsystem. Returns (healthy, latencyMs, detail). The detail
// string surfaces partial failures (e.g. "1/3 endpoints
// unhealthy" for the local-model router).
type HealthChecker interface {
	Check(ctx context.Context) (healthy bool, latencyMs int64, detail string)
}

// HealthCheckFunc adapts a plain function to the HealthChecker
// interface. Most callers in main.go wire their dependencies
// with one of these.
type HealthCheckFunc func(ctx context.Context) (bool, int64, string)

func (f HealthCheckFunc) Check(ctx context.Context) (bool, int64, string) { return f(ctx) }

// HealthHandler runs every registered checker in parallel and
// rolls them up into the documented response shape.
type HealthHandler struct {
	version  string
	started  time.Time
	checkers map[string]HealthChecker
	sections []healthSection
	operator func(*http.Request) bool
}

// healthSection is a top-level field of the response that reports state rather than pass/fail — the
// database pool's counters, the requests in flight (B27.11). It never changes the status code.
type healthSection struct {
	name string
	read func(ctx context.Context) any
	// public: everyone sees it, not only the operator (B61.113's phone_signin).
	public bool
}

func NewHealthHandler(version string, checkers map[string]HealthChecker) *HealthHandler {
	return &HealthHandler{
		version:  version,
		started:  time.Now(),
		checkers: checkers,
	}
}

// AddSection adds a top-level field, read on every request, to the response. Wire it before serving.
func (h *HealthHandler) AddSection(name string, read func(ctx context.Context) any) *HealthHandler {
	h.sections = append(h.sections, healthSection{name: name, read: read})
	return h
}

// AddPublicSection adds a top-level field everyone sees, for state that tells no attacker anything (B61.113).
func (h *HealthHandler) AddPublicSection(name string, read func(ctx context.Context) any) *HealthHandler {
	h.sections = append(h.sections, healthSection{name, read, true})
	return h
}

// ForOperator decides who sees the whole answer: check details and every section. Everyone else gets
// status, version, uptime and each check's status and latency only (B37.11) — the pool counters and
// slowest routes help someone time a load attack. Unset, nobody sees them.
func (h *HealthHandler) ForOperator(isOperator func(*http.Request) bool) *HealthHandler {
	h.operator = isOperator
	return h
}

// ServeHTTP runs every checker in parallel with a 100ms budget
// per checker (so the overall response stays under 100ms as
// long as no checker hangs all by itself).
func (h *HealthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
	defer cancel()

	type result struct {
		name    string
		healthy bool
		latency int64
		detail  string
	}
	results := make([]result, 0, len(h.checkers))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for name, c := range h.checkers {
		wg.Add(1)
		go func(name string, c HealthChecker) {
			defer wg.Done()
			ok, lat, detail := c.Check(ctx)
			mu.Lock()
			results = append(results, result{name, ok, lat, detail})
			mu.Unlock()
		}(name, c)
	}
	wg.Wait()

	operator := h.operator != nil && h.operator(r)
	checks := map[string]any{}
	anyDown, anyDegraded := false, false
	for _, r := range results {
		entry := map[string]any{
			"status":     statusString(r.healthy, r.detail),
			"latency_ms": r.latency,
		}
		if operator && r.detail != "" {
			entry["detail"] = r.detail
		}
		checks[r.name] = entry
		if !r.healthy {
			anyDown = true
		}
		if r.detail != "" && r.healthy {
			anyDegraded = true
		}
	}

	overall := "healthy"
	switch {
	case anyDown:
		overall = "unhealthy"
	case anyDegraded:
		overall = "degraded"
	}

	status := http.StatusOK
	if overall == "unhealthy" {
		// 503 keeps load balancers from sending traffic during
		// dependency outages.
		status = http.StatusServiceUnavailable
	}

	body := map[string]any{
		"status":         overall,
		"version":        h.version,
		"uptime_seconds": int64(time.Since(h.started).Seconds()),
		"checks":         checks,
	}
	for _, s := range h.sections {
		if operator || s.public {
			body[s.name] = s.read(r.Context())
		}
	}

	w.Header().Set("Content-Type", "application/json")
	// The operator's answer must never be served to the public from a cache.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func statusString(healthy bool, detail string) string {
	if !healthy {
		return "unhealthy"
	}
	if detail != "" {
		return "degraded"
	}
	return "healthy"
}
