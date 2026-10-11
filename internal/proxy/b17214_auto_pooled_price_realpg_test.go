package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/router"
)

// B17.214 — A SHARED ANSWER SERVED TO "Auto (cheapest good)" IS PRICED AS THE MODEL LENS WOULD HAVE CHOSEN.
//
// "auto" is not in the catalog: priced as itself, a pooled serve to an Auto request fell to the catalog's
// fallback floor (≈ 2 µLXC in the e2e run of 2026-10-11, against 920 µLXC for the fresh answer) and its
// ledger row named "auto". The real handler on a migrated schema: one workspace asks Auto first and
// contributes the answer; a second asks Auto and is served it from the pool. Asserted on lxc_ledger: the
// row names the router's pick and charges that model's price at the pool discount.
func TestB17214_APooledAnswerServedToAutoIsPricedAsTheModelLensWouldHaveChosen(t *testing.T) {
	p, pool, calls := chatCacheProxy(t, chatSSE("The capital of the UK is London.", true))
	p.router = router.New() // Auto resolves through the router: a capital question is its cheap pick, claude-haiku-4-5
	b17214Ask(t, p, "wsPoolA")
	b17214Ask(t, p, "wsPoolB")
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("the model was asked %d times, want 1 — wsPoolB was not served from the pool", got)
	}
	var charged int64
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT -amount, COALESCE(metadata,'{}'::jsonb) FROM lxc_ledger
		  WHERE workspace_id = 'wsPoolB' AND amount < 0 AND description = 'chat: pooled answer'`).Scan(&charged, &raw); err != nil {
		t.Fatalf("want exactly one 'chat: pooled answer' charge for wsPoolB: %v", err)
	}
	var meta struct {
		ServedModel string `json:"served_model"`
		List        int64  `json:"pool_list_ulxc"`
	}
	_ = json.Unmarshal(raw, &meta)
	// A pooled serve is priced on the estimated-tokens convention (pricePooledServe): the stored answer of an
	// Anthropic stream carries no usage, so len(prompt)/4 in and len(answer)/4 out, at the pick's list price, 30% off.
	_, prompt, _ := extractPrompt([]byte(b17214Body))
	stored, _, err := p.exact.GetWithOwner(context.Background(), "anthropic", "auto",
		cache.FingerprintedKey(pooledPromptKey(prompt), cache.RequestFingerprint([]byte(b17214Body))))
	if err != nil || stored == nil {
		t.Fatalf("wsPoolA's answer is not in the pool under \"auto\": %v", err)
	}
	usd, _ := alerts.CostUSDResolved("claude-haiku-4-5", catalog.PurposeCharge, len(prompt)/4, 0, 0, len(stored)/4)
	want := pricePooledHit(usd, 0.3, "")
	if meta.ServedModel != "claude-haiku-4-5" || meta.List != want.ListULXC || charged != want.ChargedULXC {
		t.Errorf("wsPoolB's pooled serve: served_model %q, list %d, charged %d µLXC — want claude-haiku-4-5, list %d, charged %d (its price for %d in / %d out, 30%% off)",
			meta.ServedModel, meta.List, charged, want.ListULXC, want.ChargedULXC, len(prompt)/4, len(stored)/4)
	}
}

// b17214Body is chatQuestion asked of "auto".
var b17214Body = strings.Replace(chatQuestion, `"model":"claude-haiku-4-5"`, `"model":"auto"`, 1)

// b17214Ask is chatAsk with the model "auto".
func b17214Ask(t *testing.T, p *Proxy, ws string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(b17214Body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", ws)
	req = req.WithContext(auth.WithAuthContext(req.Context(),
		&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodSessionKey, Scopes: []string{auth.ScopeProxy}}))
	w := newFlushRecorder()
	p.HandleAnthropic(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ws=%s status=%d body=%s", ws, w.Code, w.Body.String())
	}
}
