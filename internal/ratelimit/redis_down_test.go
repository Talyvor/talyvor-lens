package ratelimit

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/metrics"
)

// B28.251: with Redis down the limiter still holds each window in this instance's memory, so an abusive
// caller is refused with 429 instead of every request being let through, and every failed Redis call
// moves lens_rate_limit_redis_errors_total so the outage pages someone.
func TestCheck_RedisDownStillRefusesAbuseAndCountsTheError(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rc.Close() })
	mr.Close() // Redis is down from the first request

	limiter := New(rc, []RateRule{{RequestsPerSecond: 1000, RequestsPerMinute: 3}})
	ctx := context.Background()
	before := testutil.ToFloat64(metrics.RateLimitRedisErrorsTotal)

	for i := 0; i < 3; i++ {
		if r := limiter.Check(ctx, "ws-down", "key-down"); !r.Allowed {
			t.Fatalf("call %d refused with Redis down (limit_type=%q); want allowed under the limit", i+1, r.LimitType)
		}
	}
	r := limiter.Check(ctx, "ws-down", "key-down")
	if r.Allowed {
		t.Fatal("4th call allowed with Redis down; want refused by the per-minute window")
	}
	if r.LimitType != "minute" || r.RetryAfterSecs <= 0 || r.RetryAfterSecs > 60 {
		t.Errorf("refusal = %+v; want limit_type minute and Retry-After within the minute", r)
	}
	if !limiter.Check(ctx, "ws-down", "other-key").Allowed {
		t.Error("another key refused; the fallback must count each key on its own")
	}
	if got := testutil.ToFloat64(metrics.RateLimitRedisErrorsTotal) - before; got < 5 {
		t.Errorf("lens_rate_limit_redis_errors_total rose by %v over 5 checks with Redis down; want at least 5", got)
	}
}

// Once the in-memory windows are full, a new key is counted in one shared overflow window instead of growing
// the map, so rotating addresses during an outage neither grows memory nor escapes the limit.
func TestLocalWindows_FullMapCountsNewKeysTogether(t *testing.T) {
	var lw localWindows
	for i := 0; i < maxLocalWindows; i++ {
		lw.incr(fmt.Sprintf("k%d", i), 60)
	}
	lw.incr("new-a", 60)
	lw.incr("new-a", 60)
	if n, _ := lw.incr("new-b", 60); n != 3 {
		t.Errorf("third hit across two new keys counted %d; want 3 in the shared overflow window", n)
	}
	if got := len(lw.counts); got > maxLocalWindows+1 {
		t.Errorf("map holds %d windows; want at most %d", got, maxLocalWindows+1)
	}
}
