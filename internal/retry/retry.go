package retry

import (
	"context"
	"io"
	"math/rand" // nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used -- backoff jitter only, never an id, token or secret
	"net/http"
	"strconv"
	"time"
)

type Config struct {
	MaxAttempts    int
	BaseDelay      time.Duration
	MaxDelay       time.Duration
	RetryableCodes []int
}

type Result struct {
	Response   *http.Response
	Attempts   int
	TotalDelay time.Duration
	LastError  error
}

func DefaultConfig() Config {
	return Config{
		MaxAttempts:    3,
		BaseDelay:      1 * time.Second,
		MaxDelay:       16 * time.Second,
		RetryableCodes: []int{429, 500, 502, 503, 504},
	}
}

// Do runs fn with retry. fn MUST build a fresh, re-readable request each
// call — typically by constructing http.NewRequestWithContext with a
// bytes.NewReader over the body. The loop discards each non-final
// response body before retrying so connections can be reused.
//
// Retry semantics:
//   - Network/transport errors: retry until MaxAttempts.
//   - Response with status in RetryableCodes: retry until MaxAttempts.
//   - Any other status (success or non-retryable): return immediately.
//   - On 429 with a numeric Retry-After header, that value overrides the
//     calculated backoff so we respect the upstream's hint.
//   - Context cancellation: stop immediately and surface ctx.Err().
func Do(ctx context.Context, cfg Config, fn func(context.Context) (*http.Response, error)) Result {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	result := Result{}

	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		// Honour cancellation before each network round-trip.
		if err := ctx.Err(); err != nil {
			result.LastError = err
			return result
		}

		resp, err := fn(ctx)
		result.Attempts = attempt

		if err != nil {
			result.LastError = err
			result.Response = nil
			if attempt >= cfg.MaxAttempts {
				return result
			}
			if waited, ok := sleepWithContext(ctx, computeDelay(cfg, attempt)); !ok {
				result.LastError = ctx.Err()
				result.TotalDelay += waited
				return result
			} else {
				result.TotalDelay += waited
			}
			continue
		}

		// Got a response — decide whether to retry on status code.
		if !isRetryable(cfg.RetryableCodes, resp.StatusCode) || attempt >= cfg.MaxAttempts {
			result.Response = resp
			result.LastError = nil
			return result
		}

		// Compute backoff, allowing a 429 Retry-After header to override.
		backoff := computeDelay(cfg, attempt)
		if hint, ok := parseRetryAfter(resp); ok {
			backoff = hint
		}

		// Drain + close so the underlying connection can be reused for the
		// next attempt; otherwise the transport leaks sockets.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		waited, ok := sleepWithContext(ctx, backoff)
		result.TotalDelay += waited
		if !ok {
			result.LastError = ctx.Err()
			return result
		}
	}
	return result
}

func isRetryable(codes []int, status int) bool {
	for _, c := range codes {
		if c == status {
			return true
		}
	}
	return false
}

// computeDelay returns the exponential backoff for the given attempt
// (1-indexed) plus a jitter of up to delay/4 to spread thundering herds.
func computeDelay(cfg Config, attempt int) time.Duration {
	d := cfg.BaseDelay << (attempt - 1)
	if d > cfg.MaxDelay {
		d = cfg.MaxDelay
	}
	if d <= 0 {
		return 0
	}
	jitter := time.Duration(rand.Int63n(int64(d)/4 + 1))
	return d + jitter
}

// parseRetryAfter accepts the integer-seconds form of the Retry-After
// header. The HTTP-date form is rare for these providers and not worth
// the parsing surface area here — fall back to backoff if absent or
// malformed.
func parseRetryAfter(resp *http.Response) (time.Duration, bool) {
	if resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

// sleepWithContext waits for d unless ctx is cancelled first. Returns
// the actual time slept and whether the wait completed normally.
func sleepWithContext(ctx context.Context, d time.Duration) (time.Duration, bool) {
	if d <= 0 {
		return 0, true
	}
	start := time.Now()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return d, true
	case <-ctx.Done():
		return time.Since(start), false
	}
}
