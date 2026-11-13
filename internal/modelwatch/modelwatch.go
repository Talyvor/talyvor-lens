// Package modelwatch polls each provider's model list on a schedule and alerts when a provider is
// serving a model the catalog cannot price.
//
// ┌─ WHY THIS EXISTS ───────────────────────────────────────────────────────────────────────────────┐
// │ A model absent from the catalog used to be served FREE — catalog.Price returned ok=false and the │
// │ billing path spent that "not found" as the value zero: no hold, no ledger row, no ceiling check. │
// │ That hole is closed (catalog.ResolveRates has no zero outcome), but the CLOSURE IS A FLOOR, not  │
// │ a price: unknown traffic is now billed on the provider's cheapest known model, which recovers    │
// │ a fraction of the real cost. The floor buys time; it does not price anything correctly.          │
// │                                                                                                  │
// │ The only real fix is for a person to add the model with its published rate. That requires        │
// │ someone to KNOW a new model exists, and until now nobody did: the discovery mechanism was a      │
// │ customer sending traffic, i.e. after the money was already lost. This turns discovery around —   │
// │ the provider is asked what it serves, on a timer, BEFORE any request arrives.                    │
// └─────────────────────────────────────────────────────────────────────────────────────────────────┘
//
// ┌─ ⚠ DETECTION ONLY. THIS PACKAGE MUST NEVER SET A PRICE. ────────────────────────────────────────┐
// │ (B10.5: discovery.go carries a price a PERSON confirmed, with its source URL, into the catalog.  │
// │ That is the human step below, made immediate — nothing here ever reads a price from anywhere.)  │
// │ NOT a limitation of the current implementation — a permanent rule, and here is the reason, so    │
// │ nobody has to re-derive it before deciding to "improve" this:                                    │
// │                                                                                                  │
// │ NO PROVIDER RETURNS PRICES IN ANY API. Not in /v1/models, not anywhere. Anthropic's and OpenAI's │
// │ rates exist only on human-facing marketing/docs pages, as HTML, restructured at will and changed │
// │ without notice, versioning, or changelog. Anything that scrapes them is a parser aimed at a      │
// │ moving target — and the day the markup shifts it does not fail loudly, it silently extracts the  │
// │ wrong number and bills real customers on it. A scraper on a money path is a defect that has not  │
// │ happened yet.                                                                                    │
// │                                                                                                  │
// │ So the split is deliberate and asymmetric: EXISTENCE is machine-checkable and is checked here;   │
// │ PRICE is human-verified, cited to a URL, and pinned by internal/catalog/published_rates_test.go. │
// │ Detection is cheap and safe to be wrong about (a spurious alert costs someone a minute). Pricing │
// │ is expensive to be wrong about in both directions. Never merge the two.                          │
// └─────────────────────────────────────────────────────────────────────────────────────────────────┘
//
// ┌─ ⚠ WHAT THIS CANNOT CATCH — READ BEFORE TRUSTING IT ────────────────────────────────────────────┐
// │ 1. A PRICE CHANGE ON A MODEL THAT ALREADY EXISTS. This is the big one. Same id, new rate: the    │
// │    poller sees the id, finds it in the catalog, and reports nothing. Lens keeps charging the old  │
// │    rate indefinitely, and no test, metric, log or alert anywhere in this repo notices.            │
// │                                                                                                  │
// │    ⚠ THAT IS NOT THEORETICAL. On 2026-07-26 a by-hand read of the two pricing pages found FIVE   │
// │    live mispricings on existing ids: claude-opus-4-5 and claude-opus-4-6 billed at 15/75 when    │
// │    published at 5/25 (3x OVER), claude-haiku-4-5 at Haiku 3.5's 0.80/4.00 (UNDER), gpt-5.4 at    │
// │    5.00/20.00 vs 2.50/15.00 (OVER), gpt-5.4-mini at 0.50/2.00 vs 0.75/4.50 (UNDER). One of them  │
// │    had a passing test asserting the wrong number. This failure mode is the norm, not the edge.   │
// │                                                                                                  │
// │    What would actually catch it, in increasing order of reliability:                              │
// │      a) A DATED PERIODIC HUMAN RE-READ of the two pricing pages. Unglamorous and it is the        │
// │         honest answer. published_rates_test.go turns that re-read into a diff rather than a       │
// │         judgement call: change the constant, the test tells you what moved. Monthly, in the       │
// │         runbook, with the date recorded — an undated "we check sometimes" is not a control.       │
// │      b) INVOICE RECONCILIATION — the only fully automatic option, and the strongest, because the  │
// │         provider's bill is the authority that the marketing page merely describes. Sum token_     │
// │         events COGS per model per month and compare to the actual invoice; a per-model divergence │
// │         beyond rounding IS a rate change (or a tokenizer change, or a tier change — all of which  │
// │         matter and none of which this poller sees). It needs invoice ingestion, which does not    │
// │         exist yet. It is the right long-term answer and it should be built.                       │
// │      c) A provider-published price API. None exists. Do not wait for one.                         │
// │                                                                                                  │
// │ 2. A RETIRED model. A id vanishing from the list is not reported as drift here — it is not a      │
// │    money leak (requests to it fail outright, loudly, at the provider) and treating removals as    │
// │    alerts would fire on every transient list hiccup. It is still worth a periodic look.           │
// │ 3. ORG-SCOPED availability. /v1/models reflects THIS key's entitlements. A model another tenant    │
// │    can call may be invisible here; a limited-availability model may never appear at all (which is  │
// │    why claude-mythos-5 is deliberately unseeded — see verified_models_test.go).                    │
// │ 4. TIER / REGION / MODE pricing. Batch, priority, region, and Anthropic's fast mode (speed:"fast" │
// │    is a MODE on an existing id, priced ~2x, and Lens does not model it at all) all change the bill │
// │    without changing any id.                                                                       │
// │ 5. TOKENIZER CHANGES. Claude 4.7+ emit ~30% more tokens for the same text. Same id, same posted    │
// │    rate, a third more money. Only (b) sees this.                                                  │
// └─────────────────────────────────────────────────────────────────────────────────────────────────┘
package modelwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/metrics"
)

// listTimeout bounds one provider call. Short by design: this is a background poller with nothing
// waiting on it, so a hung provider must self-limit rather than pin a goroutine until the next tick.
const listTimeout = 10 * time.Second

// DefaultInterval is the poll cadence. Hourly, not per-minute: a new model is a once-in-weeks event and
// the cost of noticing it 40 minutes later is nil, whereas hammering a provider's control-plane
// endpoint from every deployment is rude and risks rate-limiting the key that serves real traffic.
const DefaultInterval = time.Hour

// Finding is one model a provider serves that the catalog cannot price exactly.
type Finding struct {
	Provider string
	ModelID  string
	// FallbackRate is what Lens WOULD bill this model today, per 1M output tokens, on the derived
	// floor. Included because "unknown model" is abstract while "we are billing $5 where the provider
	// may charge $75" is a number an operator can act on.
	FallbackOutputPer1M float64
}

// Notifier delivers a drift alert somewhere a PERSON will see it.
//
// ⚠ THE INTERFACE EXISTS BECAUSE A LOG LINE IS NOT AN ALERT. slog.Error is what this codebase already
// had at every other unpriced-model site, and it is precisely why five wrong rates and a whole missing
// model family survived for months: nobody reads a log they are not paged to. Delivery must be a push
// to a system a human is subscribed to, not a line in a file someone could in principle grep.
type Notifier interface {
	// Notify delivers one alert. It must not block indefinitely and its error is advisory — a failed
	// notification is logged by the caller and dropped, never retried into a queue this package owns.
	Notify(ctx context.Context, subject, body string) error
	// Describe names the sink for boot-time reporting, e.g. "webhook https://…/hooks/ops". It must
	// never include a secret.
	Describe() string
}

// provider is one upstream's model-list endpoint and how to authenticate to it.
type provider struct {
	name    string
	url     string
	key     string
	setAuth func(*http.Request, string)
}

// Watcher polls provider model lists and reports what the catalog cannot price.
type Watcher struct {
	providers []provider
	client    *http.Client
	notifier  Notifier
	// alerted remembers what has already been reported so an hourly poll does not re-alert the same
	// model 24 times a day. In memory only, deliberately: a restart re-alerting once is harmless and
	// far better than a persistence layer (and a migration) for a notification dedup.
	alerted map[string]bool

	// B10.5 — the catalog side of discovery (discovery.go). store is nil when there is no database,
	// and then the Watcher only alerts, as before.
	store   *Store
	applyMu sync.Mutex
	retired map[string]catalog.Model // catalog models this Watcher marked retired, as they were before
	applied map[string]bool          // ids whose price came from a confirmed discovery, not the seed
}

// New builds a Watcher for whichever providers have a key configured. A provider without a key is
// skipped — not an error: a deployment that does not use OpenAI should not log OpenAI failures hourly.
//
// notifier may be nil, and that case is handled LOUDLY rather than silently — see ReportReadiness.
func New(anthropicKey, openAIKey string, notifier Notifier) *Watcher {
	w := &Watcher{
		client:   &http.Client{Timeout: listTimeout},
		notifier: notifier,
		alerted:  map[string]bool{},
		retired:  map[string]catalog.Model{},
		applied:  map[string]bool{},
	}
	if anthropicKey != "" {
		w.providers = append(w.providers, provider{
			name: "anthropic",
			// ⚠ limit=1000 IS LOAD-BEARING, NOT TIDINESS. Anthropic's /v1/models paginates and DEFAULTS
			// TO 20. There are already ~20 published Claude models, so on the default page size a newly
			// released one can sit on page 2 — and this poller would see a 200, find every id it read
			// already priced, and report NO DRIFT. A blind check that returns "all clear" is worse than
			// no check, and it is the same absence-read-as-a-value shape as the zero this whole change
			// removed. 1000 is the documented maximum; has_more is ALSO checked below, so a truncated
			// list is a failed poll rather than a false all-clear.
			url: "https://api.anthropic.com/v1/models?limit=1000",
			key: anthropicKey,
			setAuth: func(r *http.Request, k string) {
				r.Header.Set("x-api-key", k)
				r.Header.Set("anthropic-version", "2023-06-01")
			},
		})
	}
	if openAIKey != "" {
		w.providers = append(w.providers, provider{
			name: "openai",
			url:  "https://api.openai.com/v1/models",
			key:  openAIKey,
			setAuth: func(r *http.Request, k string) {
				r.Header.Set("Authorization", "Bearer "+k)
			},
		})
	}
	return w
}

// ReportReadiness states at boot whether this watcher can actually reach a person, and makes the
// answer visible to monitoring either way.
//
// ⚠ THIS IS THE ANTI-INERT CHECK, and it is the difference between building an alert and building the
// APPEARANCE of one. A webhook nobody configures is a no-op that reads, in code review and in the
// changelog, exactly like a working alert — the same shape as the Track spend-alert emitter, which
// returns nil when unconfigured and is unconfigured in every deploy surface in this repo, so no spend
// alert has ever been delivered. Enabling a detector whose sink is unset would reproduce that with a
// clear conscience. So: no sink is reported as a DEFECT, at ERROR, naming the exact env vars, and the
// gauge lets "nobody is listening" be alerted on and shown on a dashboard.
func (w *Watcher) ReportReadiness() {
	if len(w.providers) == 0 {
		slog.Warn("modelwatch: no provider key configured — the catalog-drift poller has nothing to poll",
			"remedy", "set LENS_ANTHROPIC_API_KEY and/or LENS_OPENAI_API_KEY")
	}
	if w.notifier == nil {
		metrics.ModelWatchSinkConfigured.Set(0)
		slog.Error("modelwatch: NO ALERT SINK CONFIGURED — new-model detection will only write to this log, "+
			"which is the exact failure that let five wrong prices and an entire model family go unnoticed. "+
			"This detector is running but cannot reach a person.",
			"remedy", "set LENS_OPERATOR_ALERT_WEBHOOK_URL and LENS_OPERATOR_ALERT_WEBHOOK_SECRET — "+
				"the URL must accept a POST of THIS payload: "+
				`{"kind":"catalog_drift","severity":"error","subject":...,"body":...,"emitted_at":...,"source":...} `+
				"with a hex HMAC-SHA256 of the exact body in X-Lens-Signature. It is a custom contract, "+
				"NOT a chat-provider shape: a Slack incoming webhook needs a top-level `text` and Discord "+
				"needs `content`, so pointing this at either rejects every delivery with a 400 and the "+
				"alert you configured never arrives. Point it at something that takes arbitrary JSON, or "+
				"at a small relay that reshapes it.",
			"alternative", "for a single-operator deployment this sink may not be worth standing up at all: "+
				"every restart re-reports the COMPLETE current finding set at ERROR (the alerted-set is "+
				"in-memory, so nothing is suppressed across a boot), and lens_modelwatch_unpriced_models "+
				"carries the same number to any dashboard with no configuration",
			"metric", "lens_modelwatch_sink_configured=0")
		return
	}
	metrics.ModelWatchSinkConfigured.Set(1)
	slog.Info("modelwatch: catalog-drift detection armed", "sink", w.notifier.Describe(),
		"providers", len(w.providers), "interval", DefaultInterval.String())
}

// StartLoop polls until ctx is cancelled. Leader-elected by the caller, so exactly one instance in an
// HA set polls — see the wiring in cmd/lens.
func (w *Watcher) StartLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	// Check once at startup rather than waiting a full interval: a deployment that has been down for a
	// week should learn about a new model at boot, not an hour later.
	w.checkAndAlert(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.checkAndAlert(ctx)
		}
	}
}

// Check polls every configured provider and returns the models the catalog cannot price exactly.
// Errors are per-provider and non-fatal: one unreachable provider must not suppress another's findings.
func (w *Watcher) Check(ctx context.Context) ([]Finding, []error) {
	lists, _, errs := w.poll(ctx)
	return findingsIn(lists), errs
}

// poll lists every provider's models. A provider whose list failed is absent from the map (and its
// error returned), so nothing downstream mistakes an unreachable provider for an empty one. released
// is the date (YYYY-MM-DD) each provider's list gives a model, where it gives one (B18.12).
func (w *Watcher) poll(ctx context.Context) (map[string][]string, map[string]string, []error) {
	lists := map[string][]string{}
	released := map[string]string{}
	var errs []error
	for _, p := range w.providers {
		ids, err := w.listModels(ctx, p, released)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.name, err))
			continue
		}
		lists[p.name] = ids
	}
	return lists, released, errs
}

// findingsIn is every listed id the money path cannot price exactly, sorted.
func findingsIn(lists map[string][]string) []Finding {
	var findings []Finding
	for name, ids := range lists {
		for _, id := range ids {
			// ⚠ The question is NOT "is this id in the seed table" but "can the money path price it
			// exactly" — which is what ResolveRates answers, and it accounts for aliases and for
			// operator overrides supplied via LENS_MODEL_CATALOG_OVERRIDES. Asking the catalog data
			// directly would re-alert hourly on a model an operator had already correctly priced on
			// the box, and an alert that fires after the problem is fixed trains people to ignore it.
			rates, prov := catalog.ResolveRates(id, catalog.PurposeCharge)
			if prov == catalog.ProvenanceExact {
				continue
			}
			findings = append(findings, Finding{Provider: name, ModelID: id, FallbackOutputPer1M: rates.OutputPer1M})
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Provider != findings[j].Provider {
			return findings[i].Provider < findings[j].Provider
		}
		return findings[i].ModelID < findings[j].ModelID
	})
	return findings
}

func (w *Watcher) checkAndAlert(ctx context.Context) {
	lists, released, errs := w.poll(ctx)
	// B10.5 — record what each provider lists, then bring the catalog in line with it (confirmed
	// prices in, unlisted models retired) BEFORE deciding what is unpriced: a model priced since the
	// last poll must not be reported again.
	w.recordAndApply(ctx, lists, released)
	findings := findingsIn(lists)
	for _, err := range errs {
		// A provider we cannot reach is a REAL failure of this check, not a silent skip: it means the
		// deployment is running blind on that provider until the next successful poll.
		slog.Warn("modelwatch: provider model list unreachable — catalog drift is UNDETECTED for it "+
			"until the next successful poll", "error", err.Error())
	}
	metrics.ModelWatchUnpricedModels.Set(float64(len(findings)))

	var fresh []Finding
	for _, f := range findings {
		if !w.alerted[f.Provider+"/"+f.ModelID] {
			fresh = append(fresh, f)
		}
	}
	if len(fresh) == 0 {
		return
	}

	subject := fmt.Sprintf("Lens: %d provider model(s) the catalog cannot price", len(fresh))
	body := renderAlert(fresh)
	// The log line stays — it is the audit trail — but it is explicitly NOT the alert.
	slog.Error("modelwatch: provider is serving models the catalog cannot price; billing them on a "+
		"DERIVED FLOOR until a person adds the published rate", "count", len(fresh), "detail", body)

	if w.notifier == nil {
		// Do NOT mark these as alerted: nothing was delivered, so the next tick must try again rather
		// than record a notification that never happened.
		return
	}
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), listTimeout)
	defer cancel()
	if err := w.notifier.Notify(nctx, subject, body); err != nil {
		slog.Error("modelwatch: ALERT DELIVERY FAILED — a person has not been told", "error", err.Error(),
			"sink", w.notifier.Describe())
		return // again: not delivered, so not marked; retry next tick
	}
	for _, f := range fresh {
		w.alerted[f.Provider+"/"+f.ModelID] = true
	}
}

// renderAlert writes the alert body. It names the remedy explicitly, because an alert that reports a
// condition without the action leaves the reader to rediscover what this package's doc comment already
// knows.
func renderAlert(fresh []Finding) string {
	var b strings.Builder
	b.WriteString("A provider is serving models the Lens catalog cannot price.\n")
	b.WriteString("These are billed on a DERIVED FLOOR (the provider's cheapest known model), so this " +
		"traffic is UNDER-RECOVERED until someone adds the real rate:\n\n")
	for _, f := range fresh {
		fmt.Fprintf(&b, "  - %s / %s  (currently billed at $%.2f per 1M output tokens)\n",
			f.Provider, f.ModelID, f.FallbackOutputPer1M)
	}
	b.WriteString("\nWHAT TO DO:\n")
	b.WriteString("  1. Look up the published rate on the provider's own pricing page:\n")
	b.WriteString("       anthropic  https://platform.claude.com/docs/en/about-claude/pricing\n")
	b.WriteString("       openai     https://developers.openai.com/api/docs/pricing\n")
	b.WriteString("  2. Immediate, no rebuild: PUT /v1/admin/catalog/models/{id}/price with\n")
	b.WriteString("     {input_per_1m, output_per_1m, source} — or LENS_MODEL_CATALOG_OVERRIDES.\n")
	b.WriteString("  3. Durable: add it to internal/catalog/seed.go AND to published_rates_test.go\n")
	b.WriteString("     with the source URL, so the rate is pinned to a citation.\n")
	b.WriteString("\n⚠ DO NOT let an automated process fill in the price. No provider serves rates in\n")
	b.WriteString("any API; they exist only as HTML on pages that change without notice, so a scraper\n")
	b.WriteString("is silently wrong one day on a money path. A person reads the page. See\n")
	b.WriteString("internal/modelwatch/modelwatch.go for the full reasoning.\n")
	return b.String()
}

// listModels calls one provider's model-list endpoint. Both Anthropic and OpenAI return
// {"data":[{"id":...}]}, so one decoder covers both; a provider that does not is simply not polled
// rather than special-cased on a guess about its shape.
func (w *Watcher) listModels(ctx context.Context, p provider, released map[string]string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return nil, err
	}
	p.setAuth(req, p.key)
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model list returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID        string `json:"id"`
			Created   int64  `json:"created"`    // OpenAI: unix seconds
			CreatedAt string `json:"created_at"` // Anthropic: RFC 3339
		} `json:"data"`
		// HasMore is Anthropic's truncation flag. OpenAI's response omits it, where absent decodes to
		// false and is correct: that list is not paginated.
		HasMore bool `json:"has_more"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode model list: %w", err)
	}
	ids := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
			if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
				released[m.ID] = t.UTC().Format(time.DateOnly)
			} else if m.Created > 0 {
				released[m.ID] = time.Unix(m.Created, 0).UTC().Format(time.DateOnly)
			}
		}
	}
	if payload.HasMore {
		// A truncated list must NOT be diffed. Every id on the unread pages would be silently treated as
		// "not serving a new model", which is exactly the false all-clear this check exists to prevent.
		return nil, fmt.Errorf("model list was TRUNCATED (has_more=true) after %d ids, so it refuses to "+
			"report 'no drift' from a partial list; raise the page limit or add pagination", len(ids))
	}
	if len(ids) == 0 {
		// An empty list from a 200 is not "no new models" — it is a response we do not understand, and
		// reporting no drift on it would be an absence read as a value, the bug class this whole
		// change exists to remove.
		return nil, fmt.Errorf("model list was empty or unparseable, so it is treated as a failed poll, not as 'no drift'")
	}
	return ids, nil
}
