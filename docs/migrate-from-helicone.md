# Migrating from Helicone to Talyvor Lens

## Why migrate?

Helicone was acquired by Mintlify in early 2026 and entered maintenance mode shortly afterward — the public roadmap stopped, and new feature work was paused. Teams running production on Helicone are now looking for a successor that gives them the same observability shape without sitting on a frozen product.

Talyvor Lens accepts Helicone's exact wire format. Migration is one line of code — and you can stay on the wire-compatible URL forever if you want.

## Migration: 1 line of code

### Python (OpenAI client)

Before (Helicone):

```python
from openai import OpenAI
client = OpenAI(
    base_url="https://oai.helicone.ai/v1",
    default_headers={"Helicone-Auth": "Bearer sk-helicone-..."},
)
```

After (Talyvor Lens — wire-compatible URL, headers unchanged):

```python
client = OpenAI(
    base_url="http://your-lens:8080/oai/v1",   # was oai.helicone.ai
    default_headers={"Helicone-Auth": "Bearer tlv_..."},   # was sk-helicone-...
)
```

The Helicone-* headers continue to work — the compatibility layer translates them transparently.

### Recommended (cleaner — native Talyvor Lens SDK)

```python
from talyvor_lens import LensClient
client = LensClient(lens_url="http://your-lens:8080", api_key="tlv_...").openai
```

Three lines, no header gymnastics, full access to Lens-native features (sessions, branch attribution, guardrails).

### TypeScript

```typescript
// Before (Helicone)
const client = new OpenAI({
  baseURL: "https://oai.helicone.ai/v1",
  defaultHeaders: { "Helicone-Auth": "Bearer sk-helicone-..." },
});

// After (Talyvor Lens, native SDK)
import { LensClient } from "talyvor-lens";
const ai = new LensClient({ lensUrl: "http://your-lens:8080", apiKey: "tlv_..." }).openai();
```

## Header mapping

The Helicone compatibility middleware translates every Helicone header to its Lens equivalent before the proxy handler runs. Helicone headers are stripped after translation so the LLM upstream never sees them.

| Helicone header | Talyvor Lens equivalent | Notes |
|---|---|---|
| `Helicone-Auth: Bearer …` | `Authorization: Bearer tlv_…` | Replaces any incoming Authorization |
| `Helicone-User-Id` | `X-Talyvor-Session` | Groups requests into agent sessions |
| `Helicone-Property-<Name>` | `X-Talyvor-Feature` | First property becomes the feature bucket |
| `Helicone-Cache-Enabled` | (always on) | Lens caches by default; header stripped |
| `Helicone-Retry-Enabled` | (always on) | Lens retries by default; header stripped |

## URL mapping

| Helicone path | Lens-compatible path | Lens-native path |
|---|---|---|
| `oai.helicone.ai/v1/chat/completions` | `your-lens:8080/oai/v1/chat/completions` | `your-lens:8080/v1/proxy/openai/v1/chat/completions` |
| `oai.helicone.ai/v1/messages` | `your-lens:8080/anthropic/v1/messages` | `your-lens:8080/v1/proxy/anthropic/v1/messages` |

The `/oai/*` and `/anthropic/*` paths are first-class routes — they're not a deprecated alias. Use them indefinitely if you prefer.

## Feature mapping

| Helicone | Talyvor Lens |
|---|---|
| Helicone-Auth | `Authorization: Bearer tlv_...` |
| Helicone-User-Id | `X-Talyvor-Session` |
| Helicone-Cache-Enabled | On by default |
| Helicone-Retry-Enabled | On by default with exponential backoff |
| Cost tracking dashboard | `/v1/api/spend/summary` (browser view: the dashboard app) |
| Request logs export | `/v1/audit/export?format=json\|csv\|ndjson` |
| Custom properties | `X-Talyvor-Feature`, `X-Talyvor-Team` |
| Rate limiting | Per-key + per-workspace via `/v1/api/keys/pool` |

## What Talyvor Lens adds vs Helicone

- **Semantic caching** — pgvector-backed similarity matching catches near-duplicate prompts that an exact cache misses. We do not publish a typical-reduction figure: we have not measured one across real customer traffic, and it depends entirely on how repetitive your traffic is. Read your own hit rate off `/v1/api/usage` once you are running.
- **Model routing** — `internal/router` picks the cheapest model that can handle the prompt's complexity. Opt-in per workspace.
- **Prompt compression** — a SEAM, shipped OFF, and we would rather say so than let you find out. `internal/compressor` strips conversational filler with a fixed regex list. Measured against our own committed corpora it changed 0 of 308 prompts (0.000%), and it changed 8 of 8 prompts in the corpus that models coding-agent traffic — by collapsing the leading indentation of code that is not inside a ``` fence. Since migration 0117 it is per-workspace and defaults to `disabled` (`PUT /v1/workspaces/{id}/compression`, or `X-Talyvor-Compress: true` under `opt_in`). Do not migrate here for this; migrate for the rest of the list.
- **Quality scoring** — pure-Go heuristics gate cache writes so a low-quality response doesn't get served to the next caller.
- **A/B model testing** — shadow-traffic comparison between models on the same prompts.
- **Prompt versioning** — `lens:prompt:<name>` references resolve to versioned, rollback-able prompts.
- **Session / agent tracking** — multi-turn agent traces with `X-Talyvor-Session` + `X-Talyvor-Agent`.
- **MCP server** — agent frameworks (Claude Desktop, etc.) consume Lens analytics via JSON-RPC.
- **AWS Bedrock support** — Claude via IAM auth, single-invoice billing.
- **Google Gemini support** — translated transparently to/from OpenAI shape.
- **Guardrails** — PII redaction, prompt-injection scoring, blocked-topic filtering, banned-word filtering, custom regex rules — per-workspace policy.
- **Anomaly detection** — `>3σ` cost spikes published to NATS, surfaced on the dashboard.
- **Audit export** — JSON/CSV/NDJSON streams for SIEM ingestion.
- **Self-hosted** — single Go binary, < 50 MB memory, your data never leaves your infra.

## Migration steps

1. **Stand up Lens** alongside Helicone (no traffic yet). See the [main README](../README.md) for `docker compose up` or systemd setup.
2. **Issue a Talyvor key** via `POST /v1/workspaces/{wsID}/api-keys` with `{"name": "...", "scopes": ["proxy"]}`, signed in as the workspace owner.
3. **Flip one client's `base_url`** to the Lens-compatible `/oai/v1` path. Leave the `Helicone-Auth` header — the compat layer handles it.
4. **Verify** the request appears in `/v1/api/spend/summary`.
5. **Roll out** the URL flip to remaining clients.
6. **Optional** — over time, switch to the native `LensClient` SDK or the `/v1/proxy/openai/*` URLs to drop the compat overhead (it's microseconds, but the headers get cleaner).

## Rollback

The compat layer is read-only — it never modifies upstream state. Flipping `base_url` back to `oai.helicone.ai` returns full Helicone behaviour instantly. No data is locked into Lens.
