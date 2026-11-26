package proxy

import (
	"context"
	"log/slog"

	"github.com/talyvor/lens/internal/inference"
)

// byok.go — B27.26, BYOK, the one subscription tier. A workspace on the BYOK plan that holds its own key for
// a request's provider is served on that key, and Talyvor charges it no tokens for that request: no balance
// gate, no hold, no charge, on either seam — the $199 a month is the platform fee. Pooling still serves it
// (a pooled answer makes no upstream call), free and minting nothing, because a BYOK requester pays no cash
// to fund a royalty. Its own answers are pooled like any other, so a paying workspace that reuses one pays
// for it and mints the contributor's share as usual.

// ownKeySource opens a workspace's own provider keys while it is on the BYOK plan (*byok.Store).
type ownKeySource interface {
	OwnKeys(ctx context.Context, workspaceID string) (map[string]string, error)
}

// SetOwnKeys wires BYOK. Unwired (nil), every request is served on the deployment's keys.
func (p *Proxy) SetOwnKeys(s ownKeySource) { p.ownKeySrc = s }

// ownKeys returns the workspace's own keys, by provider, when this request is BYOK — the workspace is on
// the BYOK plan and holds a key for provider — else nil. A read error leaves the request as it was before
// BYOK: served on Talyvor's key and charged.
func (p *Proxy) ownKeys(ctx context.Context, wsID, provider string) map[string]string {
	if p.ownKeySrc == nil || wsID == "" || wsID == defaultWorkspaceID {
		return nil
	}
	keys, err := p.ownKeySrc.OwnKeys(ctx, wsID)
	if err != nil {
		slog.Warn("byok: a provider key could not be read", slog.String("workspace", wsID), slog.String("err", err.Error()))
	}
	if keys[provider] == "" {
		return nil
	}
	return keys
}

// agentRuleChecker judges an agent's rules for a request that moves no LXC (*economy.DualTokenStore).
type agentRuleChecker interface {
	CheckAgentRules(ctx context.Context, scopedKeyID string) error
}

// byokAgentRules holds a BYOK agent request to its agent's rules — paused, hours, models, providers — which a
// hold or debit judges for every other agent request; a BYOK request takes neither. Inert where they are.
func (p *Proxy) byokAgentRules(ctx context.Context, apiKeyID string) error {
	c, ok := p.agentSpender.(agentRuleChecker)
	if !ok || p.agentAllocEnabled == nil || !p.agentAllocEnabled() {
		return nil
	}
	return c.CheckAgentRules(ctx, apiKeyID)
}

// ownKeyConfig is provider's config on the workspace's own key, or ok=false when it holds none for it.
func (p *Proxy) ownKeyConfig(ctx context.Context, own map[string]string, provider string) (providerConfig, bool) {
	if own[provider] == "" {
		return providerConfig{}, false
	}
	return inference.ConfigForOwnKey(provider, p.endpointsFor(ctx), own[provider])
}
