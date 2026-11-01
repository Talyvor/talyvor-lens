package catalog

import "time"

// seedModels is the embedded default catalog. PRICING IS MIGRATED
// BYTE-FOR-BYTE from the previous sources — alerts.modelPrices for the rates
// and modality.capabilities for the vision/audio/document flags. Do not
// "tidy" a number here: the price-parity test (golden_test.go) asserts every
// existing model still prices identically, because a silent price drift
// corrupts every budget/forecast/anomaly/ROI figure (the cost moat).
//
// ContextTokens/MaxOutput are best-effort informational values (nothing gates
// on them yet); pricing + capabilities are authoritative.
//
// B18.12 — ReleaseDate is the day the provider made the model generally available in its API, from
// its own release notes (fetched 2026-09-29): developers.openai.com/api/docs/changelog,
// platform.claude.com/docs/en/release-notes/overview, ai.google.dev/gemini-api/docs/changelog, the
// Bedrock model cards, docs.mistral.ai's model pages (for a -latest alias, the version it points to
// now), and for Groq's open models the day Meta, Mistral or Google released them. A dated snapshot id
// is NOT a release date (claude-haiku-4-5-20251001 launched 2025-10-15). chat-latest is an alias the
// changelog never names a model for; its date is the day its current snapshot was set. Tier is the
// model's place in its provider's line-up: frontier (the most capable), balanced, fast, embedding.
func seedModels() []Model {
	vision := Capabilities{Vision: true}
	visionDoc := Capabilities{Vision: true, Document: true}
	visionAudioDoc := Capabilities{Vision: true, Audio: true, Document: true}
	sonnet45RetiresAt := time.Date(2026, 11, 30, 17, 0, 0, 0, time.UTC)

	return withCacheRates([]Model{
		// ─── OpenAI (vision) ───
		{ID: "gpt-4o", Provider: "openai", DisplayName: "GPT-4o", ReleaseDate: "2024-05-13", Tier: TierBalanced, InputPer1M: 2.50, OutputPer1M: 10.00, Capabilities: vision, ContextTokens: 128000, MaxOutput: 16384, Aliases: []string{"gpt-4o-2024-11-20", "gpt-4o-2024-08-06"}},
		{ID: "gpt-4o-mini", Provider: "openai", DisplayName: "GPT-4o mini", ReleaseDate: "2024-07-18", Tier: TierFast, InputPer1M: 0.15, OutputPer1M: 0.60, Capabilities: vision, ContextTokens: 128000, MaxOutput: 16384, Aliases: []string{"gpt-4o-mini-2024-07-18"}},
		// ⚠ EMBEDDINGS — absent until now, and their absence was not free. talyvor-code v0.1.0's
		// semantic index posts text-embedding-3-small, and with no catalog row ResolveRates fell
		// back for BOTH purposes:
		//   CHARGE → the catalog's cheapest model ($0.025/1M): 1.25x OVER for 3-small, but 5.2x
		//            UNDER for 3-large and 4x UNDER for ada-002 — Talyvor eating the difference.
		//   HOLD   → the catalog's most EXPENSIVE ($30/1M in, $180/1M out). An embeddings call has
		//            no max_tokens, so the hold was sized on a phantom output the endpoint never
		//            produces: ~$0.74 held per call against a true cost near $0.000002. On an agent
		//            sub-budget that blocks an index run after a dozen chunks while spending
		//            almost nothing — the feature budget-blocking itself.
		// Pricing them fixes both, because a KNOWN model uses its own rates for hold and charge
		// alike. OutputPer1M is 0 because the endpoint returns vectors, not completion tokens —
		// that zero is what collapses the phantom-output hold.
		// Rates from https://platform.openai.com/docs/pricing (fetched 2026-08-05), Standard tier.
		// No Capabilities (text in, vectors out) and MaxOutput 0 — nothing gates on either, but a
		// nonzero MaxOutput here would reintroduce the phantom-output hold this row removes.
		{ID: "text-embedding-3-small", Provider: "openai", DisplayName: "Embedding 3 small", ReleaseDate: "2024-01-25", Tier: TierEmbedding, InputPer1M: 0.02, OutputPer1M: 0, ContextTokens: 8191, MaxOutput: 0},
		{ID: "text-embedding-3-large", Provider: "openai", DisplayName: "Embedding 3 large", ReleaseDate: "2024-01-25", Tier: TierEmbedding, InputPer1M: 0.13, OutputPer1M: 0, ContextTokens: 8191, MaxOutput: 0},
		{ID: "text-embedding-ada-002", Provider: "openai", DisplayName: "Embedding ada 002", ReleaseDate: "2022-12-15", Tier: TierEmbedding, InputPer1M: 0.10, OutputPer1M: 0, ContextTokens: 8191, MaxOutput: 0},
		// B40.10 — speech, served on /v1/audio/transcriptions and /v1/audio/speech. Not chat models: no token
		// rates, so output_per_1m 0 keeps them out of the chat picker. Deepgram's pay-as-you-go rates from
		// https://deepgram.com/pricing (read 2026-10-23: Nova-3 pre-recorded $0.0043/min, Aura-2 $0.030 per 1k
		// characters); OpenAI's from https://developers.openai.com/api/docs/pricing (read 2026-10-23).
		{ID: "nova-3", Provider: "deepgram", DisplayName: "Deepgram Nova-3", ReleaseDate: "2025-02-12", Tier: TierSpeech, PerMinute: 0.0043, Capabilities: Capabilities{Audio: true}},
		{ID: "aura-2", Provider: "deepgram", DisplayName: "Deepgram Aura-2", ReleaseDate: "2025-04-15", Tier: TierSpeech, Per1MChars: 30.00},
		{ID: "whisper-1", Provider: "openai", DisplayName: "Whisper", ReleaseDate: "2023-03-01", Tier: TierSpeech, PerMinute: 0.006, Capabilities: Capabilities{Audio: true}},
		{ID: "tts-1", Provider: "openai", DisplayName: "TTS 1", ReleaseDate: "2023-11-06", Tier: TierSpeech, Per1MChars: 15.00},
		{ID: "tts-1-hd", Provider: "openai", DisplayName: "TTS 1 HD", ReleaseDate: "2023-11-06", Tier: TierSpeech, Per1MChars: 30.00},
		// B38.1: cached input $0.025 from the pricing page (read 2026-10-09) — see the gpt-4.1 rows below.
		// B17.277 — OpenAI answers gpt-4.1-nano with 404 model_not_found ("has been deprecated"; e2e run of
		// 2026-10-26): out of the picker, price kept for history. No successor: OpenAI's page names none.
		{ID: "gpt-4.1-nano", Provider: "openai", DisplayName: "GPT-4.1 nano", ReleaseDate: "2025-04-14", Tier: TierFast, InputPer1M: 0.10, OutputPer1M: 0.40, CachedInputPer1M: 0.025, Capabilities: vision, ContextTokens: 1000000, MaxOutput: 32768, Deprecated: true},
		// B38.3: its LongPrompt — see gpt-5.5 in the GPT-5.5 / 5.6 block below.
		{ID: "gpt-5.4", Provider: "openai", DisplayName: "GPT-5.4", ReleaseDate: "2026-03-05", Tier: TierBalanced, InputPer1M: 2.50, OutputPer1M: 15.00, CachedInputPer1M: 0.25, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000,
			LongPrompt: &LongPromptRates{AboveTokens: 272000, InputPer1M: 5.00, OutputPer1M: 22.50, CachedInputPer1M: 0.50}},
		{ID: "gpt-5.4-mini", Provider: "openai", DisplayName: "GPT-5.4 mini", ReleaseDate: "2026-03-17", Tier: TierFast, InputPer1M: 0.75, OutputPer1M: 4.50, CachedInputPer1M: 0.075, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},
		// ⚠ THE THREE BELOW WERE ON THE PRICING PAGE AND NOT IN THIS CATALOG. Every rate transcribed
		// from https://developers.openai.com/api/docs/pricing (fetched 2026-07-28), Standard tier.
		// Cached-input is the page's own "Cached" column in each case — 0.1x input for this generation,
		// NOT the 0.5x withCacheRates would otherwise derive for openai, so each is set explicitly.
		// ContextTokens/MaxOutput mirror their siblings and are informational only (nothing gates on
		// them); the PRICES are the authoritative part and are the only figures taken from the page.
		{ID: "gpt-5.4-nano", Provider: "openai", DisplayName: "GPT-5.4 nano", ReleaseDate: "2026-03-17", Tier: TierFast, InputPer1M: 0.20, OutputPer1M: 1.25, CachedInputPer1M: 0.02, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},
		// gpt-5.3-codex is the one that matters most in practice: talyvor-code sends source code, and a
		// coding-specialised model is what such a client reaches for. 35x under-recovery on output.
		{ID: "gpt-5.3-codex", Provider: "openai", DisplayName: "GPT-5.3 Codex", ReleaseDate: "2026-02-24", Tier: TierBalanced, InputPer1M: 1.75, OutputPer1M: 14.00, CachedInputPer1M: 0.175, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},
		// ⚠ chat-latest is priced by OpenAI as its own SKU, so this row bills what the page publishes
		// for that id — it is NOT an attempt to track whatever model the pointer currently resolves to.
		// It also has no "gpt-" prefix, so providerFromID could not infer its provider and the derived
		// floor was taken across the WHOLE catalog (mistral-nemo, $0.025/1M): a measured 1200x
		// under-recovery on output, the largest of any id here. That fallback is behaving as designed —
		// a CHARGE deliberately falls back low so a guessed rate never over-bills — which is exactly
		// why the remedy is to price the model rather than to change the fallback.
		{ID: "chat-latest", Provider: "openai", DisplayName: "GPT chat-latest", ReleaseDate: "2026-08-06", Tier: TierBalanced, InputPer1M: 5.00, OutputPer1M: 30.00, CachedInputPer1M: 0.50, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},
		// B38.1 — https://developers.openai.com/api/docs/pricing, Standard tier, read 2026-10-09 23:40 UTC
		// (input | cached input | cache writes | output):
		//   gpt-4.1       $2.00 | $0.50  | - | $8.00
		//   gpt-4.1-mini  $0.40 | $0.10  | - | $1.60
		//   gpt-4.1-nano  $0.10 | $0.025 | - | $0.40
		// Cached input is 0.25x input, not the 0.5x withCacheRates derives for openai, so it is set
		// explicitly; it had over-billed every cache read 2x. "-" cache writes: a written token is plain input.
		{ID: "gpt-4.1", Provider: "openai", DisplayName: "GPT-4.1", ReleaseDate: "2025-04-14", Tier: TierBalanced, InputPer1M: 2.00, OutputPer1M: 8.00, CachedInputPer1M: 0.50, Capabilities: vision, ContextTokens: 1000000, MaxOutput: 32768},
		{ID: "gpt-4.1-mini", Provider: "openai", DisplayName: "GPT-4.1 mini", ReleaseDate: "2025-04-14", Tier: TierFast, InputPer1M: 0.40, OutputPer1M: 1.60, CachedInputPer1M: 0.10, Capabilities: vision, ContextTokens: 1000000, MaxOutput: 32768},

		// ─── OpenAI GPT-5.5 / 5.6 ─────────────────────────────────────────────────────────────────
		//
		// ⚠ EVERY RATE BELOW IS TRANSCRIBED FROM https://developers.openai.com/api/docs/pricing
		//   (fetched 2026-07-26), Standard tier. Cached-input rates are the page's own "Cached" column —
		//   0.1x input for these generations, NOT the 0.5x withCacheRates assumes for openai, so they are
		//   set explicitly.
		// B38.1: the 5.6 rows were RE-PRICED on the same page, Standard tier, read 2026-10-09 23:40 UTC
		// (short context: input | cached input | cache writes | output):
		//   gpt-5.6-sol    $4.00 | $0.40 | $5.00 | $20.00   (catalog had 5.00 | 0.50 | 5.00 derived | 30.00)
		//   gpt-5.6-terra  $2.00 | $0.20 | $2.50 | $12.00   (catalog had 2.50 | 0.25 | 2.50 derived | 15.00)
		//   gpt-5.6-luna   $0.20 | $0.02 | $0.25 | $1.20    (catalog had 1.00 | 0.10 | 1.00 derived | 6.00)
		// Cache writes are now published (1.25x input), so they are set too; gpt-5.5 and below still show "-".
		// B38.7: the LongPrompt on each is the page's "Long context" row, Standard tier, re-read 2026-10-10 (>272K input tokens).
		{ID: "gpt-5.6-sol", Provider: "openai", DisplayName: "GPT-5.6 Sol", ReleaseDate: "2026-07-09", Tier: TierFrontier, InputPer1M: 4.00, OutputPer1M: 20.00, CachedInputPer1M: 0.40, CacheWritePer1M: 5.00, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000,
			LongPrompt: &LongPromptRates{AboveTokens: 272000, InputPer1M: 8.00, OutputPer1M: 30.00, CachedInputPer1M: 0.80, CacheWritePer1M: 10.00}},
		{ID: "gpt-5.6-terra", Provider: "openai", DisplayName: "GPT-5.6 Terra", ReleaseDate: "2026-07-09", Tier: TierBalanced, InputPer1M: 2.00, OutputPer1M: 12.00, CachedInputPer1M: 0.20, CacheWritePer1M: 2.50, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000,
			LongPrompt: &LongPromptRates{AboveTokens: 272000, InputPer1M: 4.00, OutputPer1M: 18.00, CachedInputPer1M: 0.40, CacheWritePer1M: 5.00}},
		{ID: "gpt-5.6-luna", Provider: "openai", DisplayName: "GPT-5.6 Luna", ReleaseDate: "2026-07-09", Tier: TierFast, InputPer1M: 0.20, OutputPer1M: 1.20, CachedInputPer1M: 0.02, CacheWritePer1M: 0.25, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000,
			LongPrompt: &LongPromptRates{AboveTokens: 272000, InputPer1M: 0.40, OutputPer1M: 1.80, CachedInputPer1M: 0.04, CacheWritePer1M: 0.50}},
		// B38.3 — above 272K input tokens (272,000: the 400K models' 400,000 less 128,000 output) OpenAI charges 2x
		//   input and cache rates and 1.5x output on the whole request — the "Long context" columns of
		//   https://developers.openai.com/api/docs/pricing, Standard tier, read 2026-10-10. The LongPrompt below and on
		//   the -pro rows is that page; "-" cache writes leave CacheWritePer1M unset (a written token is input), and
		//   the -pro rows' "-" cached input is the long input price, as in their short rows.
		{ID: "gpt-5.5", Provider: "openai", DisplayName: "GPT-5.5", ReleaseDate: "2026-04-24", Tier: TierBalanced, InputPer1M: 5.00, OutputPer1M: 30.00, CachedInputPer1M: 0.50, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000,
			LongPrompt: &LongPromptRates{AboveTokens: 272000, InputPer1M: 10.00, OutputPer1M: 45.00, CachedInputPer1M: 1.00}},
		// The -pro rows publish "—" in the Cached column: no cached-input discount is offered. Setting
		// CachedInputPer1M == InputPer1M encodes that honestly; leaving it 0 would let withCacheRates
		// invent a 0.5x discount that does not exist and under-bill every cache read.
		{ID: "gpt-5.5-pro", Provider: "openai", DisplayName: "GPT-5.5 Pro", ReleaseDate: "2026-04-24", Tier: TierFrontier, InputPer1M: 30.00, OutputPer1M: 180.00, CachedInputPer1M: 30.00, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000,
			LongPrompt: &LongPromptRates{AboveTokens: 272000, InputPer1M: 60.00, OutputPer1M: 270.00, CachedInputPer1M: 60.00}},
		{ID: "gpt-5.4-pro", Provider: "openai", DisplayName: "GPT-5.4 Pro", ReleaseDate: "2026-03-05", Tier: TierFrontier, InputPer1M: 30.00, OutputPer1M: 180.00, CachedInputPer1M: 30.00, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000,
			LongPrompt: &LongPromptRates{AboveTokens: 272000, InputPer1M: 60.00, OutputPer1M: 270.00, CachedInputPer1M: 60.00}},
		// B38.5 — GPT-5.2 and GPT-5.2 Pro, in the /v1/models capture of 2026-10-09 (verified_models_test.go).
		// https://developers.openai.com/api/docs/pricing, Standard tier, read 2026-10-10 (input | cached | output):
		//   gpt-5.2      $1.75  | $0.175 | $14.00    (cached 0.1x, set explicitly)
		//   gpt-5.2-pro  $21.00 | -      | $168.00   ("-": no discount, cached = input, as on the -pro rows above)
		// No cache-write or long-context price on the page for either. Model pages: 400K context, 128K out,
		// text and image in; the -pro answers only on /v1/responses (inference.ResponsesOnly). Released
		// 2025-12-11 (changelog). The other 11 of the item's 13 have a shutdown date on
		// https://developers.openai.com/api/docs/deprecations (read 2026-10-10), so they are not seeded:
		// o1, o1-pro, o3-mini, o4-mini, gpt-3.5-turbo on 2026-10-23; gpt-5.1 on 2027-04-01; and gpt-5,
		// gpt-5-mini, gpt-5-nano, gpt-5-pro, o3 on 2026-12-11 — each id's only snapshot, marked Deprecated on its page.
		{ID: "gpt-5.2", Provider: "openai", DisplayName: "GPT-5.2", ReleaseDate: "2025-12-11", Tier: TierBalanced, InputPer1M: 1.75, OutputPer1M: 14.00, CachedInputPer1M: 0.175, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},
		{ID: "gpt-5.2-pro", Provider: "openai", DisplayName: "GPT-5.2 Pro", ReleaseDate: "2025-12-11", Tier: TierFrontier, InputPer1M: 21.00, OutputPer1M: 168.00, CachedInputPer1M: 21.00, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},

		// ─── OpenAI GPT-6 (B15.4) ────────────────────────────────────────────────────────────────
		// Every rate transcribed from each model's page under https://developers.openai.com/api/docs/models/
		// (gpt-6-astra, gpt-6-sol, gpt-6-luna; fetched 2026-09-26), Standard tier, with the page's own
		// cached-input column — 0.1x again, so set explicitly. 1.05M context, 128K max output, same pages.
		// These are reasoning models: the proxy rewrites max_tokens/temperature for them (inference.AdaptReasoningParams).
		// B38.1: cache writes from https://developers.openai.com/api/docs/pricing, Standard tier, read 2026-10-09
		// 23:40 UTC — astra $12.50, sol $2.50, luna $0.125 (1.25x input; withCacheRates' 1.0x under-billed them).
		// Rows: astra $10.00 | $1.00 | $12.50 | $50.00; sol $2.00 | $0.20 | $2.50 | $10.00; luna $0.10 | $0.01 | $0.125 | $0.50.
		// B38.7: above 272K input tokens the whole request pays the page's "Long context" row, re-read 2026-10-10.
		{ID: "gpt-6-astra", Provider: "openai", DisplayName: "GPT-6 Astra", ReleaseDate: "2026-09-03", Tier: TierFrontier, InputPer1M: 10.00, OutputPer1M: 50.00, CachedInputPer1M: 1.00, CacheWritePer1M: 12.50, Capabilities: vision, ContextTokens: 1050000, MaxOutput: 128000,
			LongPrompt: &LongPromptRates{AboveTokens: 272000, InputPer1M: 20.00, OutputPer1M: 75.00, CachedInputPer1M: 2.00, CacheWritePer1M: 25.00}},
		{ID: "gpt-6-sol", Provider: "openai", DisplayName: "GPT-6 Sol", ReleaseDate: "2026-09-22", Tier: TierBalanced, InputPer1M: 2.00, OutputPer1M: 10.00, CachedInputPer1M: 0.20, CacheWritePer1M: 2.50, Capabilities: vision, ContextTokens: 1050000, MaxOutput: 128000,
			LongPrompt: &LongPromptRates{AboveTokens: 272000, InputPer1M: 4.00, OutputPer1M: 15.00, CachedInputPer1M: 0.40, CacheWritePer1M: 5.00}},
		{ID: "gpt-6-luna", Provider: "openai", DisplayName: "GPT-6 Luna", ReleaseDate: "2026-09-22", Tier: TierFast, InputPer1M: 0.10, OutputPer1M: 0.50, CachedInputPer1M: 0.01, CacheWritePer1M: 0.125, Capabilities: vision, ContextTokens: 1050000, MaxOutput: 128000,
			LongPrompt: &LongPromptRates{AboveTokens: 272000, InputPer1M: 0.20, OutputPer1M: 0.75, CachedInputPer1M: 0.02, CacheWritePer1M: 0.25}},
		// B38.4 — GPT-6.1 Sol, in the /v1/models capture of 2026-10-09 (verified_models_test.go). Rates from
		// https://developers.openai.com/api/docs/pricing and its model page (read 2026-10-10): $2.00 | $0.10 |
		// $2.50 | $10.00; cached is 0.05x, set explicitly. Above 272K input tokens the whole request pays
		// $4.00 | $0.20 | $5.00 | $15.00. 1.05M context (922K max input), 128K out; released 2026-09-29 (changelog).
		{ID: "gpt-6.1-sol", Provider: "openai", DisplayName: "GPT-6.1 Sol", ReleaseDate: "2026-09-29", Tier: TierBalanced, InputPer1M: 2.00, OutputPer1M: 10.00, CachedInputPer1M: 0.10, CacheWritePer1M: 2.50, Capabilities: vision, ContextTokens: 1050000, MaxOutput: 128000,
			LongPrompt: &LongPromptRates{AboveTokens: 272000, InputPer1M: 4.00, OutputPer1M: 15.00, CachedInputPer1M: 0.20, CacheWritePer1M: 5.00}},
		// B38.10 — GPT-6.1 Terra, in the /v1/models capture of 2026-10-27 (verified_models_test.go). Rates from
		// https://developers.openai.com/api/docs/pricing and its model page (read 2026-10-28): $1.00 | $0.05 |
		// $1.25 (30-minute write) | $5.00; cached is 0.05x, set explicitly. Above 272K input tokens the whole request
		// pays $2.00 | $0.10 | $2.50 | $7.50. 1.05M context (922K max input), 128K out, text and image in, text out;
		// released 2026-10-26 (changelog). Balanced like gpt-5.6-terra.
		{ID: "gpt-6.1-terra", Provider: "openai", DisplayName: "GPT-6.1 Terra", ReleaseDate: "2026-10-26", Tier: TierBalanced, InputPer1M: 1.00, OutputPer1M: 5.00, CachedInputPer1M: 0.05, CacheWritePer1M: 1.25, Capabilities: vision, ContextTokens: 1050000, MaxOutput: 128000,
			LongPrompt: &LongPromptRates{AboveTokens: 272000, InputPer1M: 2.00, OutputPer1M: 7.50, CachedInputPer1M: 0.10, CacheWritePer1M: 2.50}},
		// (B38.1: the gpt-4.1 family is on the page again as of 2026-10-09 and is priced from it above.)
		// ⚠ NOT TOUCHED: gpt-4o, gpt-4o-mini, gpt-4.1, gpt-4.1-mini, gpt-4.1-nano. They are ABSENT from
		// the current pricing page, which does not mean they are unavailable — it may mean the page lists
		// only current models. Their rates here are therefore UNVERIFIED against a published source as of
		// 2026-07-26. They are left exactly as they were rather than guessed at or deleted: no published
		// price, no edit. Re-verify before relying on them.

		// ─── Anthropic (vision + document) ───
		{ID: "claude-opus-4-5", Provider: "anthropic", DisplayName: "Claude Opus 4.5", ReleaseDate: "2025-11-24", Tier: TierFrontier, InputPer1M: 5.00, OutputPer1M: 25.00, CachedInputPer1M: 0.50, CacheWritePer1M: 6.25, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192, Aliases: []string{"claude-opus-4-5-20251101"}},
		// B38.11 — Anthropic retires it at 2026-11-30T17:00:00Z (/v1/models capture 2026-10-31: deprecated_at
		// 2026-09-30, retires_at 2026-11-30T17:00:00Z); https://platform.claude.com/docs/en/about-claude/model-deprecations
		// (read 2026-11-01) names claude-sonnet-5-5 as the replacement. Offered with its retirement date until then.
		{ID: "claude-sonnet-4-5", Provider: "anthropic", DisplayName: "Claude Sonnet 4.5", ReleaseDate: "2025-09-29", Tier: TierBalanced, InputPer1M: 3.00, OutputPer1M: 15.00, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192, Aliases: []string{"claude-sonnet-4-5-20250929"}, RetiredSuccessor: "claude-sonnet-5-5", RetiresAt: &sonnet45RetiresAt},
		{ID: "claude-haiku-4-5", Provider: "anthropic", DisplayName: "Claude Haiku 4.5", ReleaseDate: "2025-10-15", Tier: TierFast, InputPer1M: 1.00, OutputPer1M: 5.00, CachedInputPer1M: 0.10, CacheWritePer1M: 1.25, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192, Aliases: []string{"claude-haiku-4-5-20251001"}},

		// ⚠ THE DATED IDS ABOVE ARE THE PROVIDER'S PRIMARY IDS, NOT VARIANTS. Anthropic's model table
		// lists the dated form as the "Claude API ID" and the bare name as a convenience alias;
		// GET /v1/models returns the dated form, and Anthropic's docs recommend pinning a snapshot.
		// Without these aliases a client following that advice was billed on the derived floor — 5x
		// under on Opus 4.5, 3x under on Sonnet 4.5 — while its requests looked entirely ordinary.
		// The alias mechanism was already here and already used for OpenAI's dated snapshots
		// (gpt-4o-2024-11-20); it had simply never been applied to Anthropic's.
		// https://platform.claude.com/docs/en/about-claude/models/overview (fetched 2026-07-28)

		// Claude Opus 4.1 — RETIRED 2026-08-05 (B38.8): https://platform.claude.com/docs/en/about-claude/model-deprecations
		// (read 2026-10-10) lists claude-opus-4-1-20250805 as Retired, deprecated June 5, 2026, recommended replacement
		// claude-opus-4-8; Anthropic's /v1/models stopped listing it (captures 2026-10-09 and 2026-10-10). Out of the
		// picker, price kept so a past request still prices, and a new request is refused naming claude-opus-4-8.
		// $15/$75 against a $1/$5 floor is the largest single under-recovery in the Anthropic set (15x).
		// Rates transcribed from https://platform.claude.com/docs/en/about-claude/pricing (2026-07-28):
		// base input $15, 5m cache write $18.75, cache hit $1.50, output $75.
		{ID: "claude-opus-4-1", Provider: "anthropic", DisplayName: "Claude Opus 4.1", ReleaseDate: "2025-08-05", Tier: TierFrontier, InputPer1M: 15.00, OutputPer1M: 75.00, CachedInputPer1M: 1.50, CacheWritePer1M: 18.75, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 32768, Aliases: []string{"claude-opus-4-1-20250805"}, Deprecated: true, RetiredSuccessor: "claude-opus-4-8"},
		{ID: "claude-opus-4-6", Provider: "anthropic", DisplayName: "Claude Opus 4.6", ReleaseDate: "2026-02-05", Tier: TierFrontier, InputPer1M: 5.00, OutputPer1M: 25.00, CachedInputPer1M: 0.50, CacheWritePer1M: 6.25, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192},
		{ID: "claude-sonnet-4-6", Provider: "anthropic", DisplayName: "Claude Sonnet 4.6", ReleaseDate: "2026-02-17", Tier: TierBalanced, InputPer1M: 3.00, OutputPer1M: 15.00, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192},
		// NOTE: there is deliberately NO "claude-haiku-4-6" — no Haiku 4.6 exists at any version
		// (verified against GET /v1/models). The real cheapest Anthropic model is claude-haiku-4-5
		// above; a phantom entry here 404s the first cost-routed request. Guarded by
		// verified_models_test.go.
		// Claude Opus 4.8 (Upgrade 16) — verified $5 in / $25 out per 1M,
		// vision-capable, 200K context. Selectable but NOT wired into any
		// routing default (router tiers unchanged), so it's never
		// auto-selected — a caller must pin it.
		{ID: "claude-opus-4-8", Provider: "anthropic", DisplayName: "Claude Opus 4.8", ReleaseDate: "2026-05-28", Tier: TierFrontier, InputPer1M: 5.00, OutputPer1M: 25.00, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192},

		// ─── The Claude 5 family + Opus 4.7 ────────────────────────────────────────────────────────
		//
		// ⚠ EVERY RATE BELOW IS TRANSCRIBED FROM THE PUBLISHED TABLE AT
		//   https://platform.claude.com/docs/en/about-claude/pricing   (fetched 2026-07-26)
		// Nothing here is inferred from the -4 series, interpolated, or taken second-hand. A guessed rate
		// in this table is WORSE than the unknown-model fallback, because the fallback is honest about
		// being a floor while a wrong entry here is silently authoritative.
		//
		// Cache columns come from the same table's "Cache Hits & Refreshes" (read) and "5m Cache Writes"
		// (write) columns, set EXPLICITLY so withCacheRates' provider multipliers do not derive them.
		// The 1h cache write (2x input) is not modelled: cache_creation_input_tokens cannot be split by
		// TTL, so the existing convention prices all writes at the 5m rate — see withCacheRates.
		{ID: "claude-opus-5", Provider: "anthropic", DisplayName: "Claude Opus 5", ReleaseDate: "2026-07-24", Tier: TierFrontier, InputPer1M: 5.00, OutputPer1M: 25.00, CachedInputPer1M: 0.50, CacheWritePer1M: 6.25, Capabilities: visionDoc, ContextTokens: 1000000, MaxOutput: 8192, Aliases: []string{"claude-opus-5[1m]"}},
		{ID: "claude-opus-4-7", Provider: "anthropic", DisplayName: "Claude Opus 4.7", ReleaseDate: "2026-04-16", Tier: TierFrontier, InputPer1M: 5.00, OutputPer1M: 25.00, CachedInputPer1M: 0.50, CacheWritePer1M: 6.25, Capabilities: visionDoc, ContextTokens: 1000000, MaxOutput: 8192},
		// Sonnet 5 is $2 in / $10 out (cache read $0.20, 5m write $2.50), and that is its STANDARD price.
		// It launched as introductory pricing "through August 31, 2026" with a rise to $3 / $15 scheduled for
		// 1 September; the official table (fetched 2026-09-29) now says the $2 / $10 is standard and "the
		// previously scheduled increase … will not occur". Do NOT raise it: 3.00 / 15.00 would over-bill
		// every Sonnet 5 request by 1.5x. Nothing in the code can detect a price change on an id that
		// already exists (see model_detection.go's stated limits), so re-check the table if it changes.
		{ID: "claude-sonnet-5", Provider: "anthropic", DisplayName: "Claude Sonnet 5", ReleaseDate: "2026-06-30", Tier: TierBalanced, InputPer1M: 2.00, OutputPer1M: 10.00, CachedInputPer1M: 0.20, CacheWritePer1M: 2.50, Capabilities: visionDoc, ContextTokens: 1000000, MaxOutput: 8192, Aliases: []string{"claude-sonnet-5[1m]"}},
		{ID: "claude-fable-5", Provider: "anthropic", DisplayName: "Claude Fable 5", ReleaseDate: "2026-06-09", Tier: TierFrontier, InputPer1M: 10.00, OutputPer1M: 50.00, CachedInputPer1M: 1.00, CacheWritePer1M: 12.50, Capabilities: visionDoc, ContextTokens: 1000000, MaxOutput: 8192},
		// B15.4 — Opus 5.5 and Fable 5.1, transcribed from https://platform.claude.com/docs/en/about-claude/pricing
		// (fetched 2026-09-26). ⚠ THEIR CACHE READS ARE NOT 0.1x: the page's footnotes price Opus 5.5 hits
		// at 0.05x ($0.20) and Fable 5.1 hits at 0.025x ($0.25), so both are set explicitly — the derived
		// 0.1x would over-bill every cache read. IDs, 1M context and 128K max output are from
		// https://platform.claude.com/docs/en/about-claude/models/overview (same date).
		{ID: "claude-opus-5-5", Provider: "anthropic", DisplayName: "Claude Opus 5.5", ReleaseDate: "2026-09-22", Tier: TierFrontier, InputPer1M: 4.00, OutputPer1M: 20.00, CachedInputPer1M: 0.20, CacheWritePer1M: 5.00, Capabilities: visionDoc, ContextTokens: 1000000, MaxOutput: 128000},
		{ID: "claude-fable-5-1", Provider: "anthropic", DisplayName: "Claude Fable 5.1", ReleaseDate: "2026-09-01", Tier: TierFrontier, InputPer1M: 10.00, OutputPer1M: 50.00, CachedInputPer1M: 0.25, CacheWritePer1M: 12.50, Capabilities: visionDoc, ContextTokens: 1000000, MaxOutput: 128000},
		// B37.15 — Sonnet 5.5 and Haiku 5.5, in the /v1/models capture of 2026-10-09 (verified_models_test.go),
		// which also gives 1M input and 128K output; release dates are its created_at. Rates from
		// https://platform.claude.com/docs/en/about-claude/pricing (read 2026-10-10). Sonnet 5.5 hits are 0.05x
		// ($0.10), set explicitly as for Opus 5.5. Haiku 5.5 is priced by prompt length: above 100,000 tokens
		// (all input, cache reads and writes included) the whole request pays $0.50 / $2.50, write $0.625, hit $0.05.
		{ID: "claude-sonnet-5-5", Provider: "anthropic", DisplayName: "Claude Sonnet 5.5", ReleaseDate: "2026-09-28", Tier: TierBalanced, InputPer1M: 2.00, OutputPer1M: 10.00, CachedInputPer1M: 0.10, CacheWritePer1M: 2.50, Capabilities: visionDoc, ContextTokens: 1000000, MaxOutput: 128000},
		{ID: "claude-haiku-5-5", Provider: "anthropic", DisplayName: "Claude Haiku 5.5", ReleaseDate: "2026-10-07", Tier: TierFast, InputPer1M: 0.10, OutputPer1M: 0.50, CachedInputPer1M: 0.01, CacheWritePer1M: 0.125, Capabilities: visionDoc, ContextTokens: 1000000, MaxOutput: 128000,
			LongPrompt: &LongPromptRates{AboveTokens: 100000, InputPer1M: 0.50, OutputPer1M: 2.50, CachedInputPer1M: 0.05, CacheWritePer1M: 0.625}},
		// claude-mythos-5 IS DELIBERATELY NOT SEEDED. Its rate IS published ($10/$50, same as Fable 5),
		// so this is not a missing-price case — it is a missing-EXISTENCE case. The page marks it
		// "limited availability" and it is absent from the pinned /v1/models capture that
		// verified_models_test.go guards, so I cannot show it is dispatchable. Seeding a model that may
		// not exist is the phantom-claude-haiku-4-6 bug, which 404'd a live request. Unpriced is now
		// SAFE (the fallback charges a floor and alerts) whereas phantom is not, so the honest order is
		// alert-then-price. Add it here the moment a /v1/models capture shows it.
		// NO SEPARATE 1M-CONTEXT ENTRY, deliberately. The same page states: "Claude 4.6 and later models
		// include the full 1M token context window at standard pricing. (A 900k-token request is billed at
		// the same per-token rate as a 9k-token request.)" So a "[1m]" suffix is not a distinct SKU. A
		// client that sends the literal id "claude-opus-5[1m]" still misses this table — resolved by the
		// alias below rather than by a duplicate priced row.

		// ─── Google Gemini (vision + audio + document) ───
		// VERIFIED CORRECT for prompts <= 200k tokens (1.25 / 10.00), same source, same date.
		// B37.12 — above 200k tokens Google charges 2.50 / 15.00, context caching 0.25, on the whole request
		// (https://ai.google.dev/gemini-api/docs/pricing, paid tier, read 2026-10-09). Before LongPrompt,
		// long prompts to this model under-billed 2x.
		{ID: "gemini-2.5-pro", Provider: "google", DisplayName: "Gemini 2.5 Pro", ReleaseDate: "2025-06-17", Tier: TierFrontier, InputPer1M: 1.25, OutputPer1M: 10.00, Capabilities: visionAudioDoc, ContextTokens: 1000000, MaxOutput: 8192,
			LongPrompt: &LongPromptRates{AboveTokens: 200000, InputPer1M: 2.50, OutputPer1M: 15.00, CachedInputPer1M: 0.25}},
		// B15.4 — Google limits the 2.5 models to accounts that already used them, so these are what a
		// NEW customer can call. Rates and exact API ids from https://ai.google.dev/gemini-api/docs/pricing
		// (fetched 2026-09-26), Standard paid tier. ContextTokens/MaxOutput mirror the siblings above and
		// are informational only.
		// gemini-3.1-pro-preview: $2.00/$12.00 is the <=200k-token tier; above 200k Google charges
		//   $4.00/$18.00, context caching $0.40 (B37.12, same page, read 2026-10-09) — its LongPrompt.
		// gemini-3.8-flash: B38.2 — the page prices it "$0.75 through December 31, 2026" and "$1.50 starting
		//   January 1, 2027" (input), $3.75 → $7.50 (output), context caching $0.075 → $0.15 (read 2026-10-10,
		//   no timezone stated: UTC midnight). NewPrice charges the later rates from that date and not a day before.
		{ID: "gemini-3.1-pro-preview", Provider: "google", DisplayName: "Gemini 3.1 Pro", ReleaseDate: "2026-02-19", Tier: TierFrontier, InputPer1M: 2.00, OutputPer1M: 12.00, Capabilities: visionAudioDoc, ContextTokens: 1000000, MaxOutput: 8192,
			LongPrompt: &LongPromptRates{AboveTokens: 200000, InputPer1M: 4.00, OutputPer1M: 18.00, CachedInputPer1M: 0.40}},
		{ID: "gemini-3.8-flash", Provider: "google", DisplayName: "Gemini 3.8 Flash", ReleaseDate: "2026-09-02", Tier: TierFast, InputPer1M: 0.75, OutputPer1M: 3.75, Capabilities: visionAudioDoc, ContextTokens: 1000000, MaxOutput: 8192,
			NewPrice: &DatedRates{From: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), InputPer1M: 1.50, OutputPer1M: 7.50, CachedInputPer1M: 0.15}},
		{ID: "gemini-3.5-flash-lite", Provider: "google", DisplayName: "Gemini 3.5 Flash-Lite", ReleaseDate: "2026-07-21", Tier: TierFast, InputPer1M: 0.30, OutputPer1M: 2.50, Capabilities: visionAudioDoc, ContextTokens: 1000000, MaxOutput: 8192},
		// ⚠ CORRECTED 2026-07-26 — UNDER-BILLING BADLY. Held 0.075/0.30 (an older Flash generation's
		// rate); published is 0.30 in / 2.50 out — 4x under on input and 8.3x under on OUTPUT.
		// Source: https://ai.google.dev/gemini-api/docs/pricing (fetched 2026-07-26), paid tier.
		// The $1.00 audio-input tier is NOT modelled — Lens has no per-modality input rate, so
		// audio prompts to this model still bill at the text rate. Flagged, not guessed.
		{ID: "gemini-2.5-flash", Provider: "google", DisplayName: "Gemini 2.5 Flash", ReleaseDate: "2025-06-17", Tier: TierFast, InputPer1M: 0.30, OutputPer1M: 2.50, Capabilities: visionAudioDoc, ContextTokens: 1000000, MaxOutput: 8192},
		{ID: "gemini-2.0-flash", Provider: "google", DisplayName: "Gemini 2.0 Flash", ReleaseDate: "2025-02-05", Tier: TierFast, InputPer1M: 0.10, OutputPer1M: 0.40, Capabilities: visionAudioDoc, ContextTokens: 1000000, MaxOutput: 8192},
		{ID: "gemini-1.5-pro", Provider: "google", DisplayName: "Gemini 1.5 Pro", ReleaseDate: "2024-05-23", Tier: TierFrontier, InputPer1M: 1.25, OutputPer1M: 5.00, Capabilities: visionAudioDoc, ContextTokens: 2000000, MaxOutput: 8192},
		{ID: "gemini-1.5-flash", Provider: "google", DisplayName: "Gemini 1.5 Flash", ReleaseDate: "2024-05-23", Tier: TierFast, InputPer1M: 0.075, OutputPer1M: 0.30, Capabilities: visionAudioDoc, ContextTokens: 1000000, MaxOutput: 8192},

		// ─── AWS Bedrock Claude (vision + document; ~15% markup) ───
		// ⚠ CORRECTED 2026-07-26 — THESE WERE OVER-BILLING, AND THE MARKUP WAS INVENTED.
		// They held 17.25/86.25 and 3.45/17.25, which are EXACTLY 1.15x the direct Anthropic rates
		// (17.25 == 15.00 x 1.15, 86.25 == 75.00 x 1.15). Somebody assumed Bedrock charges a 15%
		// premium and derived these instead of reading AWS's price. Two errors compounded: the 15%
		// was fictional, AND the direct rate it was applied to was itself wrong (Opus 4.6 was
		// carrying Opus 4.1's 15/75 — see the corrections above).
		//
		// AWS publishes the SAME per-token rate as Anthropic direct — no premium at all:
		//   Opus 4.6:   $5 in / $25 out   https://aws.amazon.com/marketplace/pp/prodview-ssjdkfefxkn4i
		//   Sonnet 4.6: $3 in / $15 out   https://aws.amazon.com/marketplace/pp/prodview-o6w4hyizv7g64
		//   (AWS Marketplace "Bedrock Edition" listings, fetched 2026-07-26. The main
		//    aws.amazon.com/bedrock/pricing page does NOT list the 4.6 models at all.)
		// So Opus-on-Bedrock was billed 3.45x its real cost and Sonnet 1.15x.
		//
		// Cache rates stay DERIVED (withCacheRates treats bedrock with the anthropic multipliers).
		// The Marketplace listing shows separate cache and batch line items which I did not
		// transcribe, so those remain an approximation — flagged rather than guessed at.
		//
		// B23.7 — THE IDS ARE AWS'S OWN, from each model card's "Programmatic Access" table (fetched
		// 2026-09-29): docs.aws.amazon.com/bedrock/latest/userguide/model-card-anthropic-claude-opus-4-6.html
		// gives anthropic.claude-opus-4-6-v1 and …/model-card-anthropic-claude-sonnet-4-6.html gives
		// anthropic.claude-sonnet-4-6. The dated -20251101-v1:0 forms these rows carried match no AWS
		// model; they stay as aliases so a request already recorded under them still prices the same.
		{ID: "anthropic.claude-opus-4-6-v1", Provider: "bedrock", DisplayName: "Claude Opus 4.6 (Bedrock)", ReleaseDate: "2026-02-05", Tier: TierFrontier, InputPer1M: 5.00, OutputPer1M: 25.00, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192, Aliases: []string{"anthropic.claude-opus-4-6-20251101-v1:0"}},
		{ID: "anthropic.claude-sonnet-4-6", Provider: "bedrock", DisplayName: "Claude Sonnet 4.6 (Bedrock)", ReleaseDate: "2026-02-17", Tier: TierBalanced, InputPer1M: 3.00, OutputPer1M: 15.00, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192, Aliases: []string{"anthropic.claude-sonnet-4-6-20251101-v1:0"}},
		// B37.15 — ids from each model card's "Programmatic Access" table (fetched 2026-10-10):
		// …/model-card-anthropic-claude-sonnet-5-5.html and …/model-card-anthropic-claude-haiku-5-5.html. Priced
		// as the direct models above, the rates Nicolai approved for both (B37.12).
		{ID: "anthropic.claude-sonnet-5-5", Provider: "bedrock", DisplayName: "Claude Sonnet 5.5 (Bedrock)", ReleaseDate: "2026-09-28", Tier: TierBalanced, InputPer1M: 2.00, OutputPer1M: 10.00, CachedInputPer1M: 0.10, CacheWritePer1M: 2.50, Capabilities: visionDoc, ContextTokens: 1000000, MaxOutput: 128000},
		{ID: "anthropic.claude-haiku-5-5", Provider: "bedrock", DisplayName: "Claude Haiku 5.5 (Bedrock)", ReleaseDate: "2026-10-07", Tier: TierFast, InputPer1M: 0.10, OutputPer1M: 0.50, CachedInputPer1M: 0.01, CacheWritePer1M: 0.125, Capabilities: visionDoc, ContextTokens: 1000000, MaxOutput: 128000,
			LongPrompt: &LongPromptRates{AboveTokens: 100000, InputPer1M: 0.50, OutputPer1M: 2.50, CachedInputPer1M: 0.05, CacheWritePer1M: 0.625}},
		// NOTE: no Bedrock "claude-haiku-4-6" twin either — the underlying Haiku 4.6 does not exist.

		// ─── Mistral (text-only) ───
		{ID: "mistral-large-latest", Provider: "mistral", DisplayName: "Mistral Large", ReleaseDate: "2025-12-02", Tier: TierFrontier, InputPer1M: 2.00, OutputPer1M: 6.00, ContextTokens: 128000, MaxOutput: 8192},
		{ID: "mistral-small-latest", Provider: "mistral", DisplayName: "Mistral Small", ReleaseDate: "2026-03-16", Tier: TierFast, InputPer1M: 0.10, OutputPer1M: 0.30, ContextTokens: 128000, MaxOutput: 8192},
		// B23.7 — RETIRED AT MISTRAL, so Deprecated: they leave the chat picker and keep their price, so a
		// request already recorded under them still bills the same. docs.mistral.ai/getting-started/models
		// (fetched 2026-09-29), legacy table: open-mistral-7b deprecated 2024-11-30, retired 2025-03-30;
		// open-mistral-nemo-2407 (Mistral NeMo 24.07) deprecated 2026-05-22, retired 2026-07-31.
		{ID: "mistral-nemo", Provider: "mistral", DisplayName: "Mistral NeMo", ReleaseDate: "2024-07-18", Tier: TierFast, InputPer1M: 0.015, OutputPer1M: 0.045, ContextTokens: 128000, MaxOutput: 8192, Deprecated: true},
		{ID: "open-mistral-7b", Provider: "mistral", DisplayName: "Open Mistral 7B", ReleaseDate: "2023-12-11", Tier: TierFast, InputPer1M: 0.025, OutputPer1M: 0.025, ContextTokens: 32000, MaxOutput: 8192, Deprecated: true},

		// ─── Groq (text-only, hardware-accelerated open weights) ───
		{ID: "llama-3.3-70b-versatile", Provider: "groq", DisplayName: "Llama 3.3 70B (Groq)", ReleaseDate: "2024-12-06", Tier: TierBalanced, InputPer1M: 0.59, OutputPer1M: 0.79, ContextTokens: 128000, MaxOutput: 32768},
		{ID: "llama-3.1-8b-instant", Provider: "groq", DisplayName: "Llama 3.1 8B Instant (Groq)", ReleaseDate: "2024-07-23", Tier: TierFast, InputPer1M: 0.05, OutputPer1M: 0.08, ContextTokens: 128000, MaxOutput: 8192},
		{ID: "mixtral-8x7b-32768", Provider: "groq", DisplayName: "Mixtral 8x7B (Groq)", ReleaseDate: "2023-12-11", Tier: TierBalanced, InputPer1M: 0.24, OutputPer1M: 0.24, ContextTokens: 32768, MaxOutput: 8192},
		{ID: "gemma2-9b-it", Provider: "groq", DisplayName: "Gemma 2 9B (Groq)", ReleaseDate: "2024-06-27", Tier: TierFast, InputPer1M: 0.20, OutputPer1M: 0.20, ContextTokens: 8192, MaxOutput: 8192},
	})
}

// withCacheRates fills each model's prompt-caching rates (CachedInputPer1M,
// CacheWritePer1M) from its provider's PUBLISHED multiplier on the base input
// rate, leaving InputPer1M/OutputPer1M byte-for-byte untouched (the price-parity
// gate). Rates as verified against the live provider docs on 2026-07-24:
//
//   - anthropic / bedrock (Claude economics — platform.claude.com prompt-caching):
//     cache READ = 0.1x input; 5-minute cache WRITE = 1.25x input. (A 1-hour
//     write is 2x, but the aggregate cache_creation_input_tokens field can't be
//     split by TTL, so we price at the default-TTL 1.25x; a 1-hour-cached write
//     is therefore slightly under-priced — the safe direction for a savings claim.)
//   - openai (developers.openai.com prompt-caching): cache READ ~0.5x input for
//     the GPT-4o generation. (GPT-4.1-gen is actually 0.25x, so 0.5x UNDER-states
//     the discount — deliberately conservative so we never over-claim savings.)
//     No separate write charge before GPT-5.6, and no catalog model is 5.6+.
//   - everything else (google/mistral/groq): prompt caching is not billed through
//     this cost path (and our usage parser reads no cache counts for them), so we
//     apply NO discount — cache read == input rate. Never under-bills.
//
// A model may still override these by carrying explicit non-zero values.
func withCacheRates(models []Model) []Model {
	for i := range models {
		m := &models[i]
		var cachedMult, writeMult float64
		switch m.Provider {
		case "anthropic", "bedrock":
			cachedMult, writeMult = 0.10, 1.25
		case "openai":
			cachedMult, writeMult = 0.50, 1.00
		default:
			cachedMult, writeMult = 1.00, 1.00
		}
		if m.CachedInputPer1M == 0 {
			m.CachedInputPer1M = m.InputPer1M * cachedMult
		}
		if m.CacheWritePer1M == 0 {
			m.CacheWritePer1M = m.InputPer1M * writeMult
		}
	}
	return models
}
