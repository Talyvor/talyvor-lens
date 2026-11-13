package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/talyvor/lens/internal/tenant"
)

// B18.3 — THE SPENDING CAP AND RATE LIMITS A WORKSPACE SETS ARE ENFORCED.
//
// PUT /v1/workspaces/{ws}/config stores spending_cap_usd, rate_limit_rpm and rate_limit_tpm in
// workspace_configs, and until B18.3 nothing on the request path read them: a customer who set a cap
// was not protected by it. They are now checked before the provider is called, on the buffered and
// the streamed seam alike (the gate sits ahead of the streaming split), and a refusal names the limit.
//
//   - spending cap: this calendar month's spend (tenant.SpendTracker: SUM(cost_usd) over token_events,
//     kept current by every served request's cost) plus the request's input-only estimate must stay
//     within the cap; 402 otherwise.
//   - RPM: requests admitted in the last minute; 429 at the limit.
//   - TPM: estimated tokens admitted in the last minute — input (len/4) plus the output bound
//     (max_tokens, or the reservation default), which is how providers count it; 429 over the limit.
//
// The windows are this process's. A config read or spend read that fails lets the request through,
// as every other pre-serve gate does, and is logged.

// WorkspaceConfigs reads a workspace's stored config; (nil, nil) means it has none.
// *tenant.Store satisfies it.
type WorkspaceConfigs interface {
	GetConfig(ctx context.Context, workspaceID string) (*tenant.WorkspaceConfig, error)
}

// WorkspaceMonthSpend is this calendar month's spend, and how a served request adds to it.
// *tenant.SpendTracker satisfies it.
type WorkspaceMonthSpend interface {
	CurrentSpend(ctx context.Context, workspaceID string) (float64, error)
	RecordSpend(ctx context.Context, workspaceID string, costUSD float64)
}

// limitsConfigTTL bounds how long a config is trusted without re-reading it. PUT .../config also
// drops it at once (InvalidateWorkspaceLimits), so the TTL only matters across processes.
const limitsConfigTTL = 30 * time.Second

const rateWindow = time.Minute

type workspaceLimits struct {
	configs WorkspaceConfigs
	spend   WorkspaceMonthSpend
	now     func() time.Time

	mu      sync.Mutex
	cached  map[string]cachedConfig
	windows map[string][]admission
}

type cachedConfig struct {
	cfg *tenant.WorkspaceConfig
	at  time.Time
}

type admission struct {
	at     time.Time
	tokens int
}

// SetWorkspaceLimits wires B18.3's enforcement. Unset, nothing a workspace configured is enforced.
func (p *Proxy) SetWorkspaceLimits(configs WorkspaceConfigs, spend WorkspaceMonthSpend) {
	p.limits = newWorkspaceLimits(configs, spend, time.Now)
}

func newWorkspaceLimits(configs WorkspaceConfigs, spend WorkspaceMonthSpend, now func() time.Time) *workspaceLimits {
	return &workspaceLimits{configs: configs, spend: spend, now: now,
		cached: map[string]cachedConfig{}, windows: map[string][]admission{}}
}

// InvalidateWorkspaceLimits drops wsID's cached config, so a change made through PUT .../config
// applies to the very next request.
func (p *Proxy) InvalidateWorkspaceLimits(wsID string) {
	if p.limits == nil {
		return
	}
	p.limits.mu.Lock()
	delete(p.limits.cached, wsID)
	p.limits.mu.Unlock()
}

// admit decides whether wsID's own limits let this request reach the provider. estCostUSD and tokens
// are its estimates. A refusal is an HTTP status and a message naming the limit; (0, "") admits and
// counts the request against the rate windows.
func (l *workspaceLimits) admit(ctx context.Context, wsID string, estCostUSD float64, tokens int) (int, string) {
	if l == nil {
		return 0, ""
	}
	cfg := l.config(ctx, wsID)
	if cfg == nil {
		return 0, ""
	}
	if cfg.SpendingCapUSD > 0 && l.spend != nil {
		spent, err := l.spend.CurrentSpend(ctx, wsID)
		switch {
		case err != nil:
			slog.Warn("workspace limits: month spend unreadable, spending cap not checked",
				slog.String("workspace_id", wsID), slog.String("err", err.Error()))
		case spent+estCostUSD > cfg.SpendingCapUSD:
			return http.StatusPaymentRequired, fmt.Sprintf(
				"spending cap reached: this workspace's cap is %s a month and %s is spent — raise spending_cap_usd to continue",
				usd(cfg.SpendingCapUSD), usd(spent))
		}
	}
	if cfg.RateLimitRPM <= 0 && cfg.RateLimitTPM <= 0 {
		return 0, ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	win := l.windows[wsID]
	for len(win) > 0 && now.Sub(win[0].at) >= rateWindow {
		win = win[1:]
	}
	l.windows[wsID] = win
	if cfg.RateLimitRPM > 0 && len(win) >= cfg.RateLimitRPM {
		return http.StatusTooManyRequests, fmt.Sprintf(
			"rate limit reached: this workspace allows %d requests a minute (rate_limit_rpm)", cfg.RateLimitRPM)
	}
	if cfg.RateLimitTPM > 0 {
		used := 0
		for _, a := range win {
			used += a.tokens
		}
		if used+tokens > cfg.RateLimitTPM {
			return http.StatusTooManyRequests, fmt.Sprintf(
				"rate limit reached: this workspace allows %d tokens a minute (rate_limit_tpm) and this request needs up to %d",
				cfg.RateLimitTPM, tokens)
		}
	}
	l.windows[wsID] = append(win, admission{at: now, tokens: tokens})
	return 0, ""
}

// limitRefusalCode names admit's refusal for Chat (B28.82): its 402 is the spending cap, its 429 the
// workspace's own rate limit.
func limitRefusalCode(status int) string {
	switch status {
	case http.StatusPaymentRequired:
		return refusalSpendingCap
	case http.StatusTooManyRequests:
		return refusalWorkspaceRateLimit
	}
	return ""
}

// recordSpend adds a served request's cost to the month the spending cap is checked against.
func (l *workspaceLimits) recordSpend(ctx context.Context, wsID string, costUSD float64) {
	if l == nil || l.spend == nil || costUSD <= 0 {
		return
	}
	l.spend.RecordSpend(ctx, wsID, costUSD)
}

func (l *workspaceLimits) config(ctx context.Context, wsID string) *tenant.WorkspaceConfig {
	l.mu.Lock()
	c, ok := l.cached[wsID]
	l.mu.Unlock()
	if ok && l.now().Sub(c.at) < limitsConfigTTL {
		return c.cfg
	}
	cfg, err := l.configs.GetConfig(ctx, wsID)
	if err != nil {
		slog.Warn("workspace limits: config unreadable, limits not checked",
			slog.String("workspace_id", wsID), slog.String("err", err.Error()))
		return nil
	}
	l.mu.Lock()
	l.cached[wsID] = cachedConfig{cfg: cfg, at: l.now()}
	l.mu.Unlock()
	return cfg
}

// usd renders a dollar amount at up to six decimals, so a cap of $0.0005 does not read as $0.00.
func usd(v float64) string {
	return "$" + strconv.FormatFloat(math.Round(v*1e6)/1e6, 'f', -1, 64)
}
