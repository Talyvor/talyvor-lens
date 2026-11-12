package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/metrics"
)

type Limiter struct {
	redis  *redis.Client
	mu     sync.RWMutex
	rules  []RateRule
	shared SharedChecker
	perWS  WorkspaceRuleSource
	keyed  localWindows // windows of validated API keys: one per issued key, so no cap
	anon   localWindows // keyless windows (client IP, inbound email): capped at maxLocalWindows
}

// localWindows holds the same fixed windows as the Redis script, in this instance's memory. Check counts a
// request here only when its Redis call fails, so a Redis outage still refuses a caller past its limit
// instead of letting every request through. Each instance counts on its own, so with N instances a caller
// can get up to N times its limit while Redis is down.
type localWindows struct {
	mu        sync.Mutex
	counts    map[string]*localCount
	lastSweep time.Time
}

// maxLocalWindows caps the keyless windows so a caller rotating addresses during an outage cannot grow them
// without bound; once full, every new keyless key shares one overflow window. A workspace's API keys are
// counted apart and never land in it.
const maxLocalWindows = 100_000

type localCount struct {
	n       int
	resetAt time.Time
}

// incr mirrors incrScript: it counts one hit on key's window and returns the count and the seconds left.
// max caps the number of windows held (0 = no cap).
func (lw *localWindows) incr(key string, secs, max int) (int, int) {
	now := time.Now()
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.counts == nil {
		lw.counts = map[string]*localCount{}
	}
	_, known := lw.counts[key]
	full := max > 0 && !known && len(lw.counts) >= max
	if since := now.Sub(lw.lastSweep); since > time.Minute || (full && since > time.Second) {
		for k, c := range lw.counts {
			if !now.Before(c.resetAt) {
				delete(lw.counts, k)
			}
		}
		lw.lastSweep = now
	}
	if _, ok := lw.counts[key]; full && !ok && len(lw.counts) >= max {
		key = fmt.Sprintf("lens:rl:overflow:%d", secs)
	}
	c, ok := lw.counts[key]
	if !ok || !now.Before(c.resetAt) {
		c = &localCount{resetAt: now.Add(time.Duration(secs) * time.Second)}
		lw.counts[key] = c
	}
	c.n++
	return c.n, int(c.resetAt.Sub(now).Seconds() + 0.999)
}

// SharedChecker is an optional authoritative cross-instance limiter (the HA
// shared limiter) that Check consults AFTER the local windows pass. It may only
// further restrict — never loosen — so attaching one can never let a request
// through that the local windows would have rejected. When nil (the default,
// and whenever HA is disabled) Check behaves exactly as it did before.
type SharedChecker interface {
	Allow(ctx context.Context, wsID, keyID string, rule RateRule) bool
}

// AttachShared installs the authoritative cross-instance limiter. Wired only
// when HA is enabled; leaving it unset keeps single-instance behaviour
// byte-for-byte unchanged.
func (l *Limiter) AttachShared(s SharedChecker) {
	l.mu.Lock()
	l.shared = s
	l.mu.Unlock()
}

// AttachWorkspaceLimits installs the operator's per-workspace limits (B39.9): a workspace that has its own
// replaces the rules for every key of that workspace; every other workspace keeps them.
func (l *Limiter) AttachWorkspaceLimits(src WorkspaceRuleSource) {
	l.mu.Lock()
	l.perWS = src
	l.mu.Unlock()
}

type RateRule struct {
	WorkspaceID       string
	KeyID             string
	RequestsPerSecond int
	RequestsPerMinute int
	RequestsPerHour   int
}

type RateLimitResult struct {
	Allowed        bool
	LimitType      string
	RetryAfterSecs int
	Remaining      int
}

func New(redisClient *redis.Client, rules []RateRule) *Limiter {
	cp := append([]RateRule(nil), rules...)
	return &Limiter{redis: redisClient, rules: cp}
}

// DefaultRules is the package-level entry point for sensible default
// limits. The Limiter method below delegates so callers can reach the
// defaults either via the package or via an instance.
func DefaultRules() []RateRule {
	return []RateRule{{
		RequestsPerSecond: 100,
		RequestsPerMinute: 1000,
		RequestsPerHour:   10000,
	}}
}

func (l *Limiter) DefaultRules() []RateRule {
	return DefaultRules()
}

func (l *Limiter) AddRule(rule RateRule) {
	l.mu.Lock()
	l.rules = append(l.rules, rule)
	l.mu.Unlock()
}

// matchRule scores each rule by specificity and returns the highest
// scorer that matches (wsID, keyID). Scoring:
//   - WorkspaceID + KeyID set and both match → 15
//   - WorkspaceID set and matches            → 10
//   - KeyID set and matches                  →  5
//   - Both empty (global)                    →  0
//
// A rule that names a workspace/key but doesn't match is skipped entirely.
// If no rule scores positive a zero RateRule is returned, meaning "no
// limits enforced".
func (l *Limiter) matchRule(wsID, keyID string) RateRule {
	l.mu.RLock()
	defer l.mu.RUnlock()

	bestScore := -1
	var best RateRule
	for _, r := range l.rules {
		score := 0
		if r.WorkspaceID != "" {
			if r.WorkspaceID != wsID {
				continue
			}
			score += 10
		}
		if r.KeyID != "" {
			if r.KeyID != keyID {
				continue
			}
			score += 5
		}
		if score > bestScore {
			bestScore = score
			best = r
		}
	}
	return best
}

// incrScript atomically INCRs a counter, sets its expiry on first hit,
// and returns {count, ttl}. Atomic via EVAL so we don't race between
// INCR and EXPIRE — that race could leave keys with no TTL and a
// permanently-tripped limiter.
var incrScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
    redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return {count, redis.call('TTL', KEYS[1])}
`)

type window struct {
	name  string
	secs  int
	limit int
}

func windowsFor(rule RateRule) []window {
	return []window{
		{"second", 1, rule.RequestsPerSecond},
		{"minute", 60, rule.RequestsPerMinute},
		{"hour", 3600, rule.RequestsPerHour},
	}
}

// Check evaluates each window in turn. The first window that exceeds its
// limit short-circuits and returns Allowed=false with the matching
// LimitType. If every window passes, Remaining is the minimum across
// windows so the client sees the tightest budget.
//
// On a Redis error the window is counted in this instance's memory instead
// (localWindows), and lens_rate_limit_redis_errors_total moves so the
// outage alerts (LensRateLimitRedisDown).
func (l *Limiter) Check(ctx context.Context, wsID, keyID string) RateLimitResult {
	rule := l.matchRule(wsID, keyID)
	l.mu.RLock()
	perWS := l.perWS
	l.mu.RUnlock()
	if perWS != nil && wsID != "" {
		if own, ok := perWS.WorkspaceRule(ctx, wsID); ok {
			rule = own
		}
	}

	if l.redis == nil {
		return RateLimitResult{Allowed: true}
	}

	minRemaining := -1
	for _, w := range windowsFor(rule) {
		if w.limit <= 0 {
			continue // no limit configured for this window
		}
		key := fmt.Sprintf("lens:rl:%s:%s:%s", wsID, keyID, w.name)
		var count, ttl int64
		raw, err := incrScript.Run(ctx, l.redis, []string{key}, w.secs).Slice()
		switch {
		case err != nil:
			metrics.RateLimitRedisError()
			slog.Warn("ratelimit: redis script failed; counting in memory",
				slog.String("key", key),
				slog.String("err", err.Error()),
			)
			local, max := &l.keyed, 0
			if wsID == "" {
				local, max = &l.anon, maxLocalWindows
			}
			n, left := local.incr(key, w.secs, max)
			count, ttl = int64(n), int64(left)
		case len(raw) < 2:
			continue
		default:
			count, _ = raw[0].(int64)
			ttl, _ = raw[1].(int64)
		}

		if int(count) > w.limit {
			retry := int(ttl)
			if retry <= 0 {
				retry = w.secs
			}
			return RateLimitResult{
				Allowed:        false,
				LimitType:      w.name,
				RetryAfterSecs: retry,
				Remaining:      0,
			}
		}
		remaining := w.limit - int(count)
		if minRemaining < 0 || remaining < minRemaining {
			minRemaining = remaining
		}
	}
	if minRemaining < 0 {
		minRemaining = 0
	}

	// Authoritative cross-instance check (HA only). Consulted last, and only
	// able to reject — never to loosen the local decision above.
	l.mu.RLock()
	shared := l.shared
	l.mu.RUnlock()
	if shared != nil && !shared.Allow(ctx, wsID, keyID, rule) {
		return RateLimitResult{
			Allowed:        false,
			LimitType:      "shared",
			RetryAfterSecs: 60, // shared cap is a per-minute window
			Remaining:      0,
		}
	}

	return RateLimitResult{Allowed: true, Remaining: minRemaining}
}
