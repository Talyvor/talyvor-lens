package proxy

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

// B28.250 — the pooled-hit oracle. An answer another workspace contributed came back in a millisecond, a live one
// in a second or more, so any tenant could ask a guessed prompt and learn from the clock alone whether some other
// workspace had asked it. A pooled serve now waits as long as a live call to the same provider and model took:
// the wait is drawn from the live calls Lens has just made, so the two latencies come from one distribution.
// A buffered call is matched on its whole time, a stream on the time to its first byte.

// latencyRingSize is how many recent live calls each provider and model keeps to draw from.
const latencyRingSize = 256

// latencyKeys bounds how many provider-and-model pairs are kept, so a stream of made-up model names cannot grow it.
const latencyKeys = 512

type latencyRing struct {
	d    [latencyRingSize]time.Duration
	n    int
	next int
}

// upstreamLatencies holds recent live-call latencies by provider, model and kind of call. Its zero value is ready.
type upstreamLatencies struct {
	mu    sync.Mutex
	rings map[string]*latencyRing
}

func latencyKey(stream bool, provider, model string) string {
	if stream {
		return "stream\x00" + provider + "\x00" + model
	}
	return "call\x00" + provider + "\x00" + model
}

// record keeps one live call's latency.
func (l *upstreamLatencies) record(key string, d time.Duration) {
	if d <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rings == nil {
		l.rings = map[string]*latencyRing{}
	}
	r := l.rings[key]
	if r == nil {
		if len(l.rings) >= latencyKeys {
			return
		}
		r = &latencyRing{}
		l.rings[key] = r
	}
	r.d[r.next] = d
	r.next = (r.next + 1) % latencyRingSize
	if r.n < latencyRingSize {
		r.n++
	}
}

// sample draws one of the kept latencies at random; false when no live call has been seen yet.
func (l *upstreamLatencies) sample(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.rings[key]
	if r == nil || r.n == 0 {
		return 0, false
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return r.d[binary.LittleEndian.Uint64(b[:])%uint64(r.n)], true
}

// holdLikeALiveCall waits as long as a live call to provider and model would have taken, or until ctx ends.
func (p *Proxy) holdLikeALiveCall(ctx context.Context, provider, model string, stream bool) {
	d, ok := p.liveLatency.sample(latencyKey(stream, provider, model))
	if !ok {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// poolProbeCounter counts, per workspace and per minute, the requests that looked in the pool for another
// workspace's answer. A workspace asking guessed prompts at volume is probing the pool, and its answers stop
// carrying the X-Talyvor-Pool-* saving headers (the answer and its bill are unchanged).
type poolProbeCounter struct {
	mu     sync.Mutex
	window time.Time
	counts map[string]int
}

// note counts one pool lookup by wsID and reports whether wsID is now over limit in this minute. limit 0 never is.
func (c *poolProbeCounter) note(wsID string, limit int, now time.Time) bool {
	if limit <= 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if w := now.Truncate(time.Minute); !w.Equal(c.window) || c.counts == nil {
		c.window, c.counts = w, map[string]int{}
	}
	c.counts[wsID]++
	return c.counts[wsID] > limit
}

// SetPoolProbeLimit sets how many pool lookups a workspace may make in a minute before its pooled answers stop
// carrying the saving headers (config.PoolProbeLimit). 0, the default, never withholds them.
func (p *Proxy) SetPoolProbeLimit(n int) {
	if n < 0 {
		n = 0
	}
	p.poolProbeLimit = n
}
