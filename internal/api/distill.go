package api

import (
	"net/http"

	"github.com/talyvor/lens/internal/metrics"
)

// distillSummaryResponse is the honest DISTILL value view for the dashboard
// panel. Every number is labeled for what it IS — a saving, a cost, the net, or
// an avoided-reconversion rate — so the panel can never read as overclaiming.
type distillSummaryResponse struct {
	TokensSaved      float64 `json:"tokens_saved"`       // deterministic-tier savings (a real saving)
	VisionTokensCost float64 `json:"vision_tokens_cost"` // vision-OCR spend (a real COST), shown distinctly
	NetTokens        float64 `json:"net_tokens"`         // tokens_saved - vision_tokens_cost (honest NET; negative when OCR outweighs savings)
	CacheHits        float64 `json:"cache_hits"`
	CacheMisses      float64 `json:"cache_misses"`
	CacheLookups     float64 `json:"cache_lookups"`
	CacheHitRate     float64 `json:"cache_hit_rate"` // hits/(hits+misses); a DISTINCT avoided-reconversion saving, 0 when no lookups
	Basis            string  `json:"basis"`          // the token unit, surfaced for transparency
}

// distillSummary turns a raw counter snapshot into the honest display view. Net
// is savings MINUS vision cost on the same len/4 unit (may be negative — that's
// the point: OCR is spend, never hidden). Hit rate is computed separately and
// guards divide-by-zero.
func distillSummary(s metrics.DistillStats) distillSummaryResponse {
	lookups := s.CacheHits + s.CacheMisses
	rate := 0.0
	if lookups > 0 {
		rate = s.CacheHits / lookups
	}
	return distillSummaryResponse{
		TokensSaved:      s.TokensSaved,
		VisionTokensCost: s.VisionTokensCost,
		NetTokens:        s.TokensSaved - s.VisionTokensCost,
		CacheHits:        s.CacheHits,
		CacheMisses:      s.CacheMisses,
		CacheLookups:     lookups,
		CacheHitRate:     rate,
		Basis:            "len/4 token estimate",
	}
}

// handleDistillSummary surfaces the DISTILL conversion metrics for the
// dashboard. Read-only: it reads the in-process Prometheus counters, computes
// the honest net, and never touches the request path or any store. The counters are
// process-wide — every workspace's documents — so admin only (B42.1).
func (s *Server) handleDistillSummary(w http.ResponseWriter, r *http.Request) {
	if adminRead(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, distillSummary(metrics.DistillSnapshot()))
}
