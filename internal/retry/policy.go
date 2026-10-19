package retry

// policy.go — production retry/circuit-breaker abstractions.
// Sits alongside the legacy `Do` helper in retry.go; both
// stay mounted. New code paths should use the Executor /
// CircuitBreaker pair.
//
// Pieces:
//   - Policy           — exponential backoff + jitter + caps
//   - IsRetryable      — status-code + error classification
//   - ParseRetryAfter  — seconds or HTTP-date header parsing
//   - CircuitBreaker   — closed / open / half-open state machine
//   - BreakerRegistry  — per-provider lookup table
//   - Executor         — ties policy + breakers together
//
// Design notes:
//   - Jitter is "full jitter" — the final delay is a uniform
//     sample in [base-range, base+range], clamped to [1ms, MaxDelay].
//   - Context cancellation aborts both backoff and the next
//     attempt — callers can yank Execute by cancelling ctx.
//   - Breakers are per-provider; a flaky Anthropic shouldn't
//     trip the OpenAI circuit.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand" // nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used -- backoff jitter only, never an id, token or secret
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ─── policy ──────────────────────────────────────

// Policy is the backoff configuration for a retry loop.
type Policy struct {
	MaxAttempts     int
	InitialDelay    time.Duration
	MaxDelay        time.Duration
	Multiplier      float64
	Jitter          float64 // 0.0 - 1.0 fraction of base delay
	RetryableErrors []string
	NonRetryable    []string
}

// DefaultPolicy matches the spec — three attempts, half-second
// initial delay, doubling backoff, 25% full-jitter, 30s cap.
var DefaultPolicy = Policy{
	MaxAttempts:  3,
	InitialDelay: 500 * time.Millisecond,
	MaxDelay:     30 * time.Second,
	Multiplier:   2.0,
	Jitter:       0.25,
}

// minDelay is the floor — even with zero base and aggressive
// jitter we always sleep at least this long so the loop can't
// spin.
const minDelay = time.Millisecond

// NextDelay computes the wait before attempt N (0-indexed).
// Uses full-jitter: a uniform sample in [base*(1-jitter),
// base*(1+jitter)].
func (p *Policy) NextDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	mult := p.Multiplier
	if mult <= 0 {
		mult = 1
	}
	base := float64(p.InitialDelay) * math.Pow(mult, float64(attempt))
	maxF := float64(p.MaxDelay)
	if maxF <= 0 {
		maxF = float64(60 * time.Second)
	}
	if base > maxF {
		base = maxF
	}
	jr := base * p.Jitter
	// Use the global math/rand source — fine for a backoff
	// jitter, no need for crypto/rand.
	sample := base - jr + rand.Float64()*(2*jr)
	if sample > maxF {
		sample = maxF
	}
	d := time.Duration(sample)
	if d < minDelay {
		d = minDelay
	}
	return d
}

// ─── retryability ────────────────────────────────

// retryableStatuses is the HTTP-status whitelist. The codes
// match what the spec lists.
var retryableStatuses = map[int]bool{
	http.StatusTooManyRequests:     true, // 429
	http.StatusInternalServerError: true, // 500
	http.StatusBadGateway:          true, // 502
	http.StatusServiceUnavailable:  true, // 503
	http.StatusGatewayTimeout:      true, // 504
}

// nonRetryableStatuses is consulted when an err returns a
// status code that doesn't match the retryable set — these
// codes mean "fix your request" and must never retry.
var nonRetryableStatuses = map[int]bool{
	http.StatusBadRequest:          true, // 400
	http.StatusUnauthorized:        true, // 401
	http.StatusForbidden:           true, // 403
	http.StatusNotFound:            true, // 404
	http.StatusUnprocessableEntity: true, // 422
}

// IsRetryable says whether (err, statusCode) constitutes a
// transient failure worth retrying. Context cancellation is
// never retryable.
//
// `statusCode` may be 0 — that means "we didn't get a response,
// only a Go-level error". In that case we look at the error
// type: network timeouts and temporary errors are retryable.
func IsRetryable(err error, statusCode int) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if statusCode > 0 {
		if nonRetryableStatuses[statusCode] {
			return false
		}
		if retryableStatuses[statusCode] {
			return true
		}
		// 2xx is success, anything else 4xx is "your fault".
		if statusCode >= 200 && statusCode < 300 {
			return false
		}
		if statusCode >= 400 && statusCode < 500 {
			return false
		}
		// Unknown 5xx — retry.
		if statusCode >= 500 {
			return true
		}
	}
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		// net.Error.Timeout() captures dial / connect / read
		// timeouts. Anything matching is transient.
		if netErr.Timeout() {
			return true
		}
	}
	// Heuristic: a few text patterns the upstream HTTP clients
	// emit for connection-reset / EOF that don't satisfy
	// net.Error.
	msg := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"connection reset",
		"connection refused",
		"eof",
		"timeout",
		"temporary failure",
	} {
		if strings.Contains(msg, fragment) {
			return true
		}
	}
	return false
}

// ─── Retry-After parsing ─────────────────────────

// ParseRetryAfter accepts both forms of the HTTP Retry-After
// header — integer seconds or an HTTP-date — and returns the
// wait. Returns 0 for unparseable input so the caller can fall
// back to its normal backoff calculation.
func ParseRetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	// Integer seconds form.
	if n, err := strconv.Atoi(header); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	// HTTP-date form. RFC 7231 lists three acceptable date
	// formats; net/http exposes the canonical one as time.RFC1123.
	for _, layout := range []string{
		time.RFC1123,
		time.RFC1123Z,
		time.RFC850,
		time.ANSIC,
	} {
		if t, err := time.Parse(layout, header); err == nil {
			d := time.Until(t)
			if d > 0 {
				return d
			}
		}
	}
	return 0
}

// ─── circuit breaker ─────────────────────────────

// CBState is the three-position state of a CircuitBreaker.
type CBState string

const (
	CBClosed   CBState = "closed"
	CBOpen     CBState = "open"
	CBHalfOpen CBState = "half_open"
)

// CircuitBreaker is one provider's failure-tracking gate. Use
// BreakerRegistry to share these across handlers.
type CircuitBreaker struct {
	Name             string
	Threshold        int
	ResetTimeout     time.Duration
	SuccessThreshold int

	mu         sync.Mutex
	state      CBState
	failures   int
	successes  int
	lastFailAt time.Time

	// onStateChange is the structured-logging hook main.go
	// installs so transitions show up in slog.
	onStateChange func(name string, from, to CBState)
}

// NewCircuitBreaker builds a breaker with the documented
// defaults (threshold 5, reset 60s, success threshold 2).
// Pass zero values to use the defaults.
func NewCircuitBreaker(name string, threshold int, resetTimeout time.Duration, successThreshold int) *CircuitBreaker {
	if threshold <= 0 {
		threshold = 5
	}
	if resetTimeout <= 0 {
		resetTimeout = 60 * time.Second
	}
	if successThreshold <= 0 {
		successThreshold = 2
	}
	return &CircuitBreaker{
		Name:             name,
		Threshold:        threshold,
		ResetTimeout:     resetTimeout,
		SuccessThreshold: successThreshold,
		state:            CBClosed,
	}
}

// SetOnStateChange wires the structured-logging hook.
func (cb *CircuitBreaker) SetOnStateChange(fn func(name string, from, to CBState)) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.onStateChange = fn
}

// State returns the current state — safe to call from any
// goroutine.
func (cb *CircuitBreaker) State() CBState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

// Allow reports whether the next request should be attempted.
// Closed → always true.
// Open → true once ResetTimeout has elapsed (and transitions
//
//	the breaker into HalfOpen as a side effect).
//
// HalfOpen → true exactly once per call until RecordSuccess /
//
//	RecordFailure resolves the trial.
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	switch cb.state {
	case CBClosed:
		return true
	case CBOpen:
		if time.Since(cb.lastFailAt) >= cb.ResetTimeout {
			cb.transition(CBHalfOpen)
			cb.successes = 0
			return true
		}
		return false
	case CBHalfOpen:
		// In a proper half-open implementation we'd block all
		// but one trial. We keep it simple and let the upstream
		// retry/concurrency layer enforce serialisation.
		return true
	}
	return false
}

// RecordSuccess advances the state on a successful call.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	switch cb.state {
	case CBClosed:
		cb.failures = 0
	case CBHalfOpen:
		cb.successes++
		if cb.successes >= cb.SuccessThreshold {
			cb.transition(CBClosed)
			cb.failures = 0
			cb.successes = 0
		}
	case CBOpen:
		// Shouldn't normally happen — Allow() refused this call
		// — but if it does, treat as a recovery probe.
		cb.transition(CBHalfOpen)
		cb.successes = 1
	}
}

// RecordFailure advances the state on a failed call.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.lastFailAt = time.Now()
	switch cb.state {
	case CBClosed:
		cb.failures++
		if cb.failures >= cb.Threshold {
			cb.transition(CBOpen)
		}
	case CBHalfOpen:
		// Single failure in half-open re-opens the breaker.
		cb.transition(CBOpen)
	}
}

// ApplyRemoteState forces the breaker into `to` because a peer instance
// gossiped that transition over the HA bus. It deliberately does NOT fire
// onStateChange: that hook is the publish path, and re-firing it on a mirrored
// transition would echo the event back onto the bus indefinitely. Counters are
// reset so the local breaker behaves sensibly from the new state, and an
// open mirror stamps lastFailAt so this instance honours ResetTimeout before
// probing half-open — exactly as if it had tripped locally.
func (cb *CircuitBreaker) ApplyRemoteState(to CBState) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.state == to {
		return
	}
	cb.state = to
	cb.failures = 0
	cb.successes = 0
	if to == CBOpen {
		cb.lastFailAt = time.Now()
	}
}

// transition is the lock-held state mutator. Caller must
// already hold cb.mu.
func (cb *CircuitBreaker) transition(to CBState) {
	from := cb.state
	cb.state = to
	if cb.onStateChange != nil && from != to {
		// Defer the callback so we don't hold the mutex across
		// arbitrary user code.
		go cb.onStateChange(cb.Name, from, to)
	}
}

// ─── registry ────────────────────────────────────

// BreakerRegistry is the per-provider lookup. Lazily constructs
// breakers with default settings.
type BreakerRegistry struct {
	mu       sync.RWMutex
	breakers map[string]*CircuitBreaker

	threshold        int
	resetTimeout     time.Duration
	successThreshold int
	onStateChange    func(name string, from, to CBState)
}

// NewBreakerRegistry builds a registry with the supplied
// defaults. Pass zeros to use library defaults.
func NewBreakerRegistry(threshold int, resetTimeout time.Duration, successThreshold int) *BreakerRegistry {
	return &BreakerRegistry{
		breakers:         map[string]*CircuitBreaker{},
		threshold:        threshold,
		resetTimeout:     resetTimeout,
		successThreshold: successThreshold,
	}
}

// SetOnStateChange wires the structured-logging hook for every
// breaker the registry creates from now on. Already-created
// breakers are updated too.
func (r *BreakerRegistry) SetOnStateChange(fn func(name string, from, to CBState)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onStateChange = fn
	for _, b := range r.breakers {
		b.SetOnStateChange(fn)
	}
}

// GetOrCreate returns the breaker for `provider`, constructing
// a fresh one if needed.
func (r *BreakerRegistry) GetOrCreate(provider string) *CircuitBreaker {
	r.mu.RLock()
	b, ok := r.breakers[provider]
	r.mu.RUnlock()
	if ok {
		return b
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.breakers[provider]; ok {
		return b
	}
	b = NewCircuitBreaker(provider, r.threshold, r.resetTimeout, r.successThreshold)
	if r.onStateChange != nil {
		b.SetOnStateChange(r.onStateChange)
	}
	r.breakers[provider] = b
	return b
}

// ApplyRemoteState mirrors a peer instance's breaker transition into this
// registry, lazily creating the breaker if this instance hasn't served the
// provider yet. Used by the HA breaker-gossip layer so a trip on one instance
// propagates to all of them.
func (r *BreakerRegistry) ApplyRemoteState(provider string, to CBState) {
	r.GetOrCreate(provider).ApplyRemoteState(to)
}

// States returns a snapshot of every breaker's state. Useful
// for the admin dashboard.
func (r *BreakerRegistry) States() map[string]CBState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]CBState, len(r.breakers))
	for name, b := range r.breakers {
		out[name] = b.State()
	}
	return out
}

// ─── executor ────────────────────────────────────

// ErrCircuitOpen is returned when the breaker is open at the
// start of an Execute call. Callers can `errors.Is` against it
// to distinguish "the upstream is melting" from "the request
// itself was bad".
var ErrCircuitOpen = errors.New("retry: circuit breaker open")

// ExecuteResult reports what Execute did. Useful for metrics.
// Distinct from the legacy `Result` in retry.go (HTTP-only).
type ExecuteResult struct {
	Value       interface{}
	StatusCode  int
	Attempts    int
	TotalDelay  time.Duration
	CircuitOpen bool
}

// Executor wires a Policy and a BreakerRegistry together.
type Executor struct {
	Policy   *Policy
	Breakers *BreakerRegistry
}

// NewExecutor builds an executor with sensible defaults.
func NewExecutor(policy *Policy, breakers *BreakerRegistry) *Executor {
	if policy == nil {
		p := DefaultPolicy
		policy = &p
	}
	if breakers == nil {
		breakers = NewBreakerRegistry(0, 0, 0)
	}
	return &Executor{Policy: policy, Breakers: breakers}
}

// Execute is the retry loop. `fn` returns (value, statusCode, err).
// statusCode = 0 means "no HTTP response at all" (network error).
//
// On 429, fn may attach the Retry-After header to the error via
// the RetryAfterError wrapper; Execute respects that delay
// instead of the policy backoff.
func (e *Executor) Execute(
	ctx context.Context,
	provider string,
	fn func() (interface{}, int, error),
) (*ExecuteResult, error) {
	breaker := e.Breakers.GetOrCreate(provider)
	if !breaker.Allow() {
		return &ExecuteResult{CircuitOpen: true}, ErrCircuitOpen
	}

	policy := e.Policy
	if policy == nil {
		p := DefaultPolicy
		policy = &p
	}
	attempts := policy.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}

	var (
		lastErr    error
		lastStatus int
		totalDelay time.Duration
	)
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 && !breaker.Allow() {
			return &ExecuteResult{Attempts: attempt, CircuitOpen: true}, ErrCircuitOpen
		}
		val, status, err := fn()
		lastStatus = status
		if err == nil && status >= 200 && status < 300 {
			breaker.RecordSuccess()
			return &ExecuteResult{
				Value:      val,
				StatusCode: status,
				Attempts:   attempt + 1,
				TotalDelay: totalDelay,
			}, nil
		}
		lastErr = err
		breaker.RecordFailure()

		if !IsRetryable(err, status) {
			break
		}
		if attempt == attempts-1 {
			break
		}

		// Pick a delay — when the upstream wrapped the error in
		// *RetryAfterError, honour that value (even zero —
		// "retry immediately" is a legitimate server signal).
		delay := policy.NextDelay(attempt)
		var rae *RetryAfterError
		if errors.As(err, &rae) {
			delay = rae.After
			if delay < 0 {
				delay = 0
			}
			if policy.MaxDelay > 0 && delay > policy.MaxDelay {
				delay = policy.MaxDelay
			}
		}
		if delay < minDelay {
			delay = minDelay
		}
		totalDelay += delay

		select {
		case <-ctx.Done():
			return &ExecuteResult{Attempts: attempt + 1, TotalDelay: totalDelay}, ctx.Err()
		case <-time.After(delay):
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("retry: exhausted attempts with status %d", lastStatus)
	}
	return &ExecuteResult{
		StatusCode: lastStatus,
		Attempts:   attempts,
		TotalDelay: totalDelay,
	}, lastErr
}

// RetryAfterError is the wrapper fn() can return when it wants
// Execute to use a server-supplied Retry-After value instead of
// the policy backoff calculation.
type RetryAfterError struct {
	After time.Duration
	Err   error
}

func (r *RetryAfterError) Error() string {
	if r.Err != nil {
		return r.Err.Error()
	}
	return "retry-after"
}

func (r *RetryAfterError) Unwrap() error { return r.Err }

// NewRetryAfterError builds the wrapper from a Retry-After
// header string and the underlying error.
func NewRetryAfterError(header string, err error) *RetryAfterError {
	return &RetryAfterError{After: ParseRetryAfter(header), Err: err}
}
