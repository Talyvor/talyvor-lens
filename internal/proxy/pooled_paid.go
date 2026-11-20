package proxy

import (
	"context"
	"log/slog"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/poolroyalty"
)

// B17.29 — AN ANSWER PAID FOR ONCE IS NOT PAID FOR AGAIN.
//
// A workspace served another's answer from the shared pool pays the pooled price for it. Asked the
// same thing again — in a new chat, or by a client retry — it is served that answer again, and it is
// its earlier answer: 0 LXC, and no second royalty. No copy of the answer is kept for the workspace
// (the contributor's deletion and a thumbs-down must still stop it being served); only a marker that
// it was paid for, naming the pooled entry and the answer's digest, so a changed answer is charged.

// pooledPaidBefore reports whether wsID has already been charged for exactly this pooled answer.
func (p *Proxy) pooledPaidBefore(ctx context.Context, wsID string, hit *poolroyalty.ServedHit, answer []byte) bool {
	if hit == nil || p.exact == nil || wsID == "" {
		return false
	}
	paid, err := p.exact.PooledPaid(ctx, wsID, hit.EntryID, cache.AnswerDigest(answer))
	return err == nil && paid
}

// markPooledPaid records that wsID was charged for this pooled answer. A nil hit (an own-cache serve,
// or a pooled one already paid for) records nothing.
func (p *Proxy) markPooledPaid(ctx context.Context, wsID string, hit *poolroyalty.ServedHit, answer []byte) {
	if hit == nil || p.exact == nil || wsID == "" {
		return
	}
	ctx, cancel := detachBounded(ctx) // B28.242: the charge it records survived a hang-up, so must the mark
	defer cancel()
	if err := p.exact.MarkPooledPaid(ctx, wsID, hit.EntryID, cache.AnswerDigest(answer)); err != nil {
		slog.Warn("proxy: could not record a paid pooled answer (an exact repeat will be charged again)",
			slog.String("workspace_id", wsID), slog.String("err", err.Error()))
	}
}
