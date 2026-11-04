// Package catalog is the single source of truth for the models Lens knows
// about — their provider, pricing (USD per 1M tokens), capabilities
// (vision/audio/document), and context limits. Before this, those facts were
// scattered across the alerts price table, the modality capability registry,
// and the router; consolidating them here means adding a model is data, not
// code edits in several places.
//
// Boundary: the catalog owns model FACTS (price, capabilities, context). It
// does NOT own routing POLICY — the router's cost-tier ranks, the cheap/mid/
// premium tiers, fallback chains, and provider dispatch remain in their
// packages and operate on catalog models. (modelRanks is a tier order, not a
// price order, so it isn't derivable from catalog pricing.)
//
// The registry is read-mostly and concurrency-safe: pricing/capability
// lookups happen on the request hot path, so reads take an RWMutex read lock
// and never allocate beyond the returned value. Runtime overrides (config/DB)
// layer on top of the embedded default so an operator can add or reprice a
// model without a rebuild.
package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Capabilities are the non-text modalities a model can serve. Mirrors
// modality.Capabilities (which now reads from here).
type Capabilities struct {
	Vision   bool `json:"vision"`
	Audio    bool `json:"audio"`
	Document bool `json:"document"`
}

// Model is one catalog entry. InputPer1M/OutputPer1M are USD per 1,000,000
// tokens — the canonical pricing the cost-attribution moat depends on.
// ContextTokens/MaxOutput are best-effort informational values (no behavior
// gates on them today); pricing + capabilities are authoritative.
//
// CachedInputPer1M / CacheWritePer1M are the prompt-caching rates (USD per 1M):
// what a provider charges for input tokens served from its cache (a read) and
// for tokens written into its cache (a write). They are seeded from published
// per-provider multipliers on the base input rate (see seed.go withCacheRates)
// and are what lets the cost basis price a cache-heavy request at what Talyvor
// actually paid rather than at the full list rate. A zero value means "unset"
// and PriceDetailed falls it back to the input rate (never free).
type Model struct {
	ID               string       `json:"id"`
	Provider         string       `json:"provider"`
	DisplayName      string       `json:"display_name"`
	InputPer1M       float64      `json:"input_per_1m"`
	OutputPer1M      float64      `json:"output_per_1m"`
	CachedInputPer1M float64      `json:"cached_input_per_1m,omitempty"` // cache READ rate (Anthropic 0.1x input, OpenAI ~0.5x)
	CacheWritePer1M  float64      `json:"cache_write_per_1m,omitempty"`  // cache WRITE rate (Anthropic 1.25x input)
	Capabilities     Capabilities `json:"capabilities"`
	ContextTokens    int          `json:"context_tokens"`
	MaxOutput        int          `json:"max_output"`
	Deprecated       bool         `json:"deprecated,omitempty"`
	Aliases          []string     `json:"aliases,omitempty"` // e.g. dated snapshots → this canonical id
	// B38.8 — set only once the provider has RETIRED the model (requests to it fail there), or with RetiresAt
	// for a retirement the provider has dated: from then on Lens refuses a request for it with a "model retired"
	// error naming this successor, and never sends it to the successor instead.
	RetiredSuccessor string `json:"retired_successor,omitempty"`
	// B38.11 — when the provider retires the model (its /v1/models retires_at). Until then it is offered with
	// "retiring on <date>" beside its name in the listing; from then it is retired as above. nil = no date.
	RetiresAt *time.Time `json:"retires_at,omitempty"`
	// B18.12 — when the provider released the model (YYYY-MM-DD, from its release notes) and its tier,
	// so the chat picker can list the newest first and default to the newest frontier model without
	// guessing either from the name.
	ReleaseDate string `json:"release_date,omitempty"`
	Tier        string `json:"tier,omitempty"`
	// B37.12 — a price that follows the prompt's length (Gemini Pro above 200k tokens). nil = one price.
	LongPrompt *LongPromptRates `json:"long_prompt,omitempty"`
	// B38.2 — a price the provider has announced for a set date (Gemini 3.8 Flash on 2027-01-01). nil = none.
	NewPrice *DatedRates `json:"new_price,omitempty"`
	// B40.10 — speech models are priced by what they hear or say, not in tokens: USD per minute of audio
	// transcribed, and USD per 1,000,000 characters spoken. Zero on every other model.
	PerMinute  float64 `json:"per_minute,omitempty"`
	Per1MChars float64 `json:"per_1m_chars,omitempty"`
}

// DatedRates replace a model's own rates for every request served at or after From. A LongPrompt still
// applies above its threshold: no seeded model has both, and one that does needs its long rates dated too.
// A zero cache rate falls back to InputPer1M, as in PriceDetailed.
type DatedRates struct {
	From             time.Time `json:"from"`
	InputPer1M       float64   `json:"input_per_1m"`
	OutputPer1M      float64   `json:"output_per_1m"`
	CachedInputPer1M float64   `json:"cached_input_per_1m,omitempty"`
	CacheWritePer1M  float64   `json:"cache_write_per_1m,omitempty"`
}

// LongPromptRates are the prices a provider charges for the WHOLE request — every input, cache and
// output token — once its prompt is longer than AboveTokens. The prompt's length counts all its input
// tokens, cache reads and writes included. A zero cache rate falls back to InputPer1M, as in PriceDetailed.
type LongPromptRates struct {
	AboveTokens      int     `json:"above_tokens"`
	InputPer1M       float64 `json:"input_per_1m"`
	OutputPer1M      float64 `json:"output_per_1m"`
	CachedInputPer1M float64 `json:"cached_input_per_1m,omitempty"`
	CacheWritePer1M  float64 `json:"cache_write_per_1m,omitempty"`
}

// The tiers a model is in: the most capable a provider offers, its everyday middle, its cheap and fast
// one, and embedding models (which are not chat models at all).
const (
	TierFrontier  = "frontier"
	TierBalanced  = "balanced"
	TierFast      = "fast"
	TierEmbedding = "embedding"
	TierSpeech    = "speech" // B40.10: transcription and spoken replies, served on /v1/audio/*
)

// TierFor infers the tier of a model the seed does not know — one discovery found — from the words
// providers use in their ids. The seed states every tier outright; this is only for new models, and
// the price a person confirms for one is where a wrong guess would be seen.
func TierFor(id string) string {
	id = strings.ToLower(id)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(id, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("embed"):
		return TierEmbedding
	case has("mini", "nano", "haiku", "flash", "lite", "small", "instant", "luna"):
		return TierFast
	case has("opus", "fable", "mythos", "-pro", "astra"):
		return TierFrontier
	}
	return TierBalanced
}

// Registry holds the models keyed by canonical id, with an alias index.
type Registry struct {
	mu      sync.RWMutex
	byID    map[string]Model
	aliasTo map[string]string
	now     func() time.Time // nil: time.Now; when a dated price is in force (B38.2)
}

// NewRegistry builds a registry from a seed list.
func NewRegistry(models []Model) *Registry {
	r := &Registry{byID: make(map[string]Model, len(models)), aliasTo: map[string]string{}}
	for _, m := range models {
		r.put(m)
	}
	return r
}

// put inserts/updates a model + its aliases. Caller must hold the write lock
// (or be single-threaded construction).
func (r *Registry) put(m Model) {
	r.byID[m.ID] = m
	for _, a := range m.Aliases {
		r.aliasTo[a] = m.ID
	}
}

// resolve maps an id-or-alias to a canonical id. Caller holds at least RLock.
func (r *Registry) resolve(id string) string {
	if _, ok := r.byID[id]; ok {
		return id
	}
	if canon, ok := r.aliasTo[id]; ok {
		return canon
	}
	return id
}

// Get returns the model for an id or alias.
func (r *Registry) Get(id string) (Model, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.byID[r.resolve(id)]
	return m, ok
}

// Price returns (input, output) USD per 1M tokens; ok=false for unknown
// models (callers price an unknown model at 0, exactly as before).
func (r *Registry) Price(id string) (in, out float64, ok bool) {
	m, ok := r.Get(id)
	return m.InputPer1M, m.OutputPer1M, ok
}

// PriceDetailed returns the full cache-aware per-1M price breakdown:
//
//	in         — uncached input rate
//	cachedIn   — cache-READ rate (input tokens served from the provider's cache)
//	cacheWrite — cache-WRITE rate (input tokens written into the cache)
//	out        — output rate
//
// in/out are byte-identical to Price for every model (a golden test enforces
// this), so the existing cost basis is never perturbed. ok=false for unknown
// models (priced at 0, exactly as Price). When a model carries no explicit
// cached/write rate — e.g. a runtime Override that set only input/output — those
// fall back to the input rate: the conservative choice that never bills a cached
// token at zero and never under-states cost.
func (r *Registry) PriceDetailed(id string) (in, cachedIn, cacheWrite, out float64, ok bool) {
	return r.PriceDetailedAt(id, 0)
}

// PriceDetailedAt is PriceDetailed for a request whose prompt is promptTokens input tokens long (cache
// reads and writes included): above the model's LongPrompt threshold it returns the long-prompt rates.
func (r *Registry) PriceDetailedAt(id string, promptTokens int) (in, cachedIn, cacheWrite, out float64, ok bool) {
	m, ok := r.Get(id)
	if !ok {
		return 0, 0, 0, 0, false
	}
	in, out = m.InputPer1M, m.OutputPer1M
	cachedIn, cacheWrite = m.CachedInputPer1M, m.CacheWritePer1M
	if np := m.NewPrice; np != nil && !r.clock().Before(np.From) {
		in, out, cachedIn, cacheWrite = np.InputPer1M, np.OutputPer1M, np.CachedInputPer1M, np.CacheWritePer1M
	}
	if lp := m.LongPrompt; lp != nil && promptTokens > lp.AboveTokens {
		in, out, cachedIn, cacheWrite = lp.InputPer1M, lp.OutputPer1M, lp.CachedInputPer1M, lp.CacheWritePer1M
	}
	if cachedIn == 0 {
		cachedIn = in
	}
	if cacheWrite == 0 {
		cacheWrite = in
	}
	return in, cachedIn, cacheWrite, out, true
}

func (r *Registry) clock() time.Time {
	r.mu.RLock()
	now := r.now
	r.mu.RUnlock()
	if now == nil {
		return time.Now()
	}
	return now()
}

// CapabilitiesOf returns a model's capabilities (zero value = text-only for
// unknowns — the conservative default).
func (r *Registry) CapabilitiesOf(id string) Capabilities {
	m, _ := r.Get(id)
	return m.Capabilities
}

// Resolve maps an id-or-alias to its canonical id (the id itself if unknown).
func (r *Registry) Resolve(id string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolve(id)
}

// All returns every model, sorted by provider then id (deterministic API).
func (r *Registry) All() []Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Model, 0, len(r.byID))
	for _, m := range r.byID {
		out = append(out, m)
	}
	sortModels(out, false)
	return out
}

// Retired reports whether the model is retired now, and the successor to name: a RetiredSuccessor with no
// RetiresAt, or one whose RetiresAt has passed (B38.11).
func (r *Registry) Retired(m Model) bool {
	return m.RetiredSuccessor != "" && (m.RetiresAt == nil || !r.clock().Before(*m.RetiresAt))
}

// Listing is All as the catalog routes show it (B38.11): a model with a dated retirement still ahead carries
// "retiring on <date>" beside its name, and one whose date has passed is deprecated, so it leaves the picker.
// All itself stays raw: model watch writes what it reads back with Override.
func (r *Registry) Listing() []Model {
	out := r.All()
	for i, m := range out {
		switch {
		case m.RetiresAt == nil:
		case r.Retired(m):
			out[i].Deprecated = true
		default:
			out[i].DisplayName += " · retiring on " + m.RetiresAt.UTC().Format("2 Jan 2006")
		}
	}
	return out
}

// ByProvider returns a provider's models sorted by input price (cheapest first).
//
// ⚠ ITS DOC COMMENT USED TO SAY "the order the modality redirect uses to pick a capable model" AND
// THAT WAS FALSE. MEASURED: `grep -rn ByProvider` over every .go file in this repo finds the
// declaration, the package-level alias, this comment, and FOUR call sites that are all in _test.go —
// zero production callers. The modality redirect walks modality.providerPreference, a HARDCODED
// per-provider list, and takes capability facts (not order) from the catalog; the fallback bound in
// resolve.go#fallbackRates iterates r.byID directly and filters on m.Provider. Neither ever calls
// this. The consequence of believing the comment is pinned in
// internal/proxy/modality_redirect_reach_test.go: the redirect's reachable set is SIX openai models,
// not the sixteen this catalog records as vision-capable.
//
// It is kept rather than deleted because the fallback-anchor half of the claim IS a real population
// (fallbackRates selects exactly the set this returns, by the same predicate) and because a
// provider-scoped, price-ordered view is the shape any future redirect widening would want. What it
// is NOT is a description of what runs today.
func (r *Registry) ByProvider(provider string) []Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Model
	for _, m := range r.byID {
		if m.Provider == provider {
			out = append(out, m)
		}
	}
	sortModels(out, true)
	return out
}

// Override adds or updates a model at runtime (config/DB-driven), layered on
// the embedded default. Concurrency-safe with hot-path reads.
func (r *Registry) Override(m Model) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.put(m)
}

// LoadOverrides applies a batch of overrides (e.g. parsed from a config file
// or DB) on top of the embedded default.
func (r *Registry) LoadOverrides(models []Model) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range models {
		r.put(m)
	}
}

// DecodeOverrides decodes the operator override document — a JSON array of Model, the exact
// shape LENS_MODEL_CATALOG_OVERRIDES carries — into the batch LoadOverrides applies.
//
// ⚠ IT EXISTS BECAUSE `json.Unmarshal` INTO A FRESH Model MAKES "UNSAID" AND "FALSE" THE SAME BYTE,
// AND put REPLACES THE WHOLE ENTRY. cmd/lens advertises this variable as the way to "reprice models
// without a rebuild"; a reprice states a price and nothing else, so every FACT the catalog holds
// about that model — provider, capabilities, context window, aliases, display name — decoded as its
// zero value and overwrote the seeded truth. MEASURED through the proxy's real entry point, not
// inferred: `[{"id":"gpt-4o","input_per_1m":3.75,"output_per_1m":15.00}]` turned a 200 streaming
// vision request to gpt-4o into a 422 "model gpt-4o does not support it", dropped gpt-4o out of
// ByProvider("openai") (19 models → 18, so it stops anchoring that provider's fallback bound — ⚠ and
// stops being a redirect target for the SEPARATE reason that its capabilities were blanked, NOT
// because ByProvider is what the redirect reads; it has no production caller at all, see its own
// doc), and emptied its capability entry in the introspection API. A price
// change silently withdrew a capability.
//
// So an override for an id the registry ALREADY holds is decoded ON TOP OF that entry: an absent
// field means "unchanged", a present one means what it says. `"capabilities":{"vision":false}` still
// turns vision off — the operator can still state a fact, they simply can no longer erase one by
// omission. An id the registry does not hold decodes from the zero Model exactly as before, because
// there is no truth to preserve and inventing one would be worse.
//
// ⚠ THE FOUR PRICE FIELDS ARE DELIBERATELY NOT PRESERVED, so this change moves NO money. They are
// zeroed before the decode, which leaves an omitted price reading exactly as it does today: unpriced,
// which ResolveRates routes to the loud fallback arm (see resolve.go#unpriced) rather than to a
// silent zero. Carrying the OLD rates forward would look tidier and would be a pricing decision —
// a model repriced 2.50 → 3.75 with cached_input_per_1m omitted would keep billing cache reads at
// the 1.25 that was 50% of the OLD list price and is 33% of the new one. That is a real question
// and it is not this function's to answer.
//
// ⚠ Lookup is by EXACT id, never through resolve(): an override written against a dated-snapshot
// ALIAS registers a new entry keyed by that alias, exactly as it does today. Merging an alias onto
// its canonical entry would rewrite the alias index as a side effect of a reprice, which is the
// class of silent consequence this function exists to remove.
//
// ⚠⚠ AN ELEMENT THAT NAMES NO MODEL IS REFUSED, AND WITH IT THE WHOLE DOCUMENT. `id` is the only
// field that says WHICH model the element is about, and an absent one used to decode to "" and be
// handed to put, which keys byID by exactly that string. Two things then happened and neither was
// said out loud: the reprice the operator wrote did NOT happen (cmd/lens still logged "applied model
// overrides count=1"), and a PHANTOM entry entered the catalog under the empty id — which the money
// path reads. MEASURED at 3fa1196 through the real functions, on 1M input + 1M output: proxy
// extractPrompt returns req.Model with no non-empty check and HandleOpenAI serves a body carrying no
// `model` key with 200, so ResolveRates("") is a live call; it billed charge $0.02 / hold $210.00 as
// `fallback` with an ERROR and a metric on every request, and with the phantom registered it billed
// $18.75 on BOTH arms as `exact` — silently, and invisibly to modelwatch, which skips exact. An
// over-bill on the charge, a collapsed ceiling on the hold, and the loud instrument switched off, all
// from writing `model` where the document wants `id`.
//
// ⚠ THE WHOLE DOCUMENT, NOT JUST THE ELEMENT, is a choice: cmd/lens#applyCatalogOverrides already
// applies NOTHING when the document does not parse, so "Lens could not read this" keeps one meaning
// and one outcome. It costs nothing on the money axis — every model the document meant to price falls
// to the fallback arm, which is non-zero by construction, defensible, and screams once per request.
// Loud-and-unpriced beats silent-and-wrong. The alternative (apply the named elements, skip the rest)
// keeps more pricing and leaves a half-applied document; it is a defensible answer and it is not the
// one taken here.
func (r *Registry) DecodeOverrides(raw []byte) ([]Model, error) {
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Model, 0, len(elems))
	for i, e := range elems {
		var probe struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(e, &probe); err != nil {
			return nil, err
		}
		// Whitespace is folded in with absence deliberately: an id of spaces can never match a
		// request either, so it is the same failure wearing a different byte.
		if strings.TrimSpace(probe.ID) == "" {
			return nil, fmt.Errorf(`element %d names no model: every override element needs a non-empty "id" `+
				`(the field is "id" here — the wire calls the same thing "model", which is the usual way to `+
				`write this wrong). Refusing the whole document: an element with no id registers a catalog `+
				`entry under the empty string, which is what prices a request that names no model`, i)
		}
		base := r.byID[probe.ID] // zero Model when the id is new — the pre-existing behaviour
		base.InputPer1M, base.OutputPer1M = 0, 0
		base.CachedInputPer1M, base.CacheWritePer1M = 0, 0
		base.PerMinute, base.Per1MChars = 0, 0
		base.LongPrompt = nil // a price too: a reprice that does not restate it drops it
		base.NewPrice = nil
		// DisallowUnknownFields HERE and deliberately NOT on the id probe above: the
		// probe reads one field on purpose, so refusing unknowns there would refuse
		// every document ever written.
		//
		// ⚠ A FIELD encoding/json CANNOT PLACE IS A FIELD THE OPERATOR BELIEVES IS
		// DOING SOMETHING, AND WHEN IT IS A RATE THE ZEROING TWO LINES UP MAKES IT
		// COST MONEY SILENTLY. `[{"id":"gpt-4o","input_per_1m":3.75,"outputPer1M":15.00}]`
		// used to decode with OutputPer1M 0.00 and resolve 3.75/0.00 as
		// ProvenanceEXACT — every output token billed at zero, with no error, no
		// metric, and no drift finding (modelwatch skips the exact arm). `unpriced`
		// cannot catch it: it is BOTH-rates-zero by necessity, because three seeded
		// embeddings models publish a zero OUTPUT rate. It has to be caught here,
		// where the document is read.
		//
		// Case is NOT a typo — encoding/json matches field names case-insensitively,
		// so `output_per_1M` still lands on OutputPer1M and keeps working.
		//
		// The WHOLE document is refused, which is #451's decision restated rather
		// than re-litigated: applyCatalogOverrides applies nothing when the document
		// does not parse, so "Lens could not read this" keeps one meaning.
		dec := json.NewDecoder(bytes.NewReader(e))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&base); err != nil {
			return nil, fmt.Errorf("element %d (id %q): %w — every key must be a field of the "+
				"override document; a key Lens cannot place is silently ignored, and when it is a "+
				"rate the model registers at 0.00 and bills as an exact price. Refusing the whole "+
				"document", i, probe.ID, err)
		}
		out = append(out, base)
	}
	return out, nil
}

func sortModels(ms []Model, byPrice bool) {
	sort.Slice(ms, func(i, j int) bool {
		if byPrice && ms[i].InputPer1M != ms[j].InputPer1M {
			return ms[i].InputPer1M < ms[j].InputPer1M
		}
		if !byPrice && ms[i].Provider != ms[j].Provider {
			return ms[i].Provider < ms[j].Provider
		}
		return ms[i].ID < ms[j].ID
	})
}

// ─── the global default catalog ───
// Package-level functions read this; the hot path (alerts pricing, modality
// capabilities) calls them directly.

var defaultRegistry = NewRegistry(seedModels())

func Get(id string) (Model, bool)                { return defaultRegistry.Get(id) }
func Price(id string) (in, out float64, ok bool) { return defaultRegistry.Price(id) }
func PriceDetailed(id string) (in, cachedIn, cacheWrite, out float64, ok bool) {
	return defaultRegistry.PriceDetailed(id)
}
func PriceDetailedAt(id string, promptTokens int) (in, cachedIn, cacheWrite, out float64, ok bool) {
	return defaultRegistry.PriceDetailedAt(id, promptTokens)
}
func CapabilitiesOf(id string) Capabilities { return defaultRegistry.CapabilitiesOf(id) }
func Resolve(id string) string              { return defaultRegistry.Resolve(id) }
func All() []Model                          { return defaultRegistry.All() }
func Listing() []Model                      { return defaultRegistry.Listing() }
func Retired(m Model) bool                  { return defaultRegistry.Retired(m) }
func ByProvider(provider string) []Model    { return defaultRegistry.ByProvider(provider) }
func Override(m Model)                      { defaultRegistry.Override(m) }
func LoadOverrides(models []Model)          { defaultRegistry.LoadOverrides(models) }
func DecodeOverrides(raw []byte) ([]Model, error) {
	return defaultRegistry.DecodeOverrides(raw)
}

// SetClock sets the time the default catalog prices at, so a test can serve a request on either side of a
// dated price (B38.2); restore puts the wall clock back.
func SetClock(now func() time.Time) (restore func()) {
	defaultRegistry.mu.Lock()
	defaultRegistry.now = now
	defaultRegistry.mu.Unlock()
	return func() { SetClock(nil) }
}
