# Migrating from LiteLLM to Talyvor Lens

## Why migrate?

LiteLLM suffered a supply-chain compromise in March 2026 affecting versions `1.82.7` and `1.82.8`. The Python ecosystem's reliance on `pip install` from PyPI makes that class of attack hard to fully prevent. Teams running LiteLLM in production now have to audit every dependency update before applying it.

Talyvor Lens is written in Go and ships as a single statically-linked binary. There is no per-deploy `pip install` step; the Go module graph is pinned in `go.mod` + verified against `go.sum`. The supply-chain attack surface is dramatically smaller.

Memory is also dramatically smaller. LiteLLM's Python process holds 8 GB+ under sustained load. Lens runs in under 50 MB.

## Migration

LiteLLM's proxy exposes an OpenAI-compatible endpoint. Talyvor Lens does the same. The migration is exactly one configuration change.

```diff
- base_url = "http://localhost:4000/v1"        # LiteLLM
+ base_url = "http://your-lens:8080/v1/proxy/openai/v1"   # Talyvor Lens
```

Authentication shifts from LiteLLM's master key to a Talyvor key:

```python
client = OpenAI(
    base_url="http://your-lens:8080/v1/proxy/openai/v1",
    api_key="tlv_...",   # Talyvor key, issued via POST /v1/workspaces/{wsID}/api-keys
)
```

For native session / branch / workspace headers, use the Talyvor SDK:

```python
from talyvor_lens import LensClient
client = LensClient(lens_url="http://your-lens:8080", api_key="tlv_...").openai
```

## Feature comparison

| Feature | LiteLLM | Talyvor Lens |
|---|---|---|
| Language / runtime | Python (CPython, GIL) | Go (compiled binary) |
| Memory at idle | ~300 MB | < 50 MB |
| Memory under load | 8 GB+ documented in community reports | Bounded; tied to cache + workspace count |
| Single-binary deploy | No (Python venv) | Yes |
| Semantic cache (pgvector) | No | Yes |
| Exact cache (Redis) | Yes | Yes |
| Quality scoring | No | Yes |
| Prompt compression | No | Seam only — shipped OFF (see below) |
| Prompt versioning | No | Yes |
| A/B model testing | No | Yes (shadow traffic) |
| MCP server | No | Yes |
| AWS Bedrock | Yes | Yes (SigV4, no AWS SDK) |
| Google Gemini | Yes | Yes (with response translation) |
| Provider fallback chains | Yes | Yes |
| Guardrails (PII, injection, topic, custom) | Partial | Yes (unified engine) |
| Anomaly detection | No | Yes (`>3σ` z-score) |
| Audit export | No | Yes (JSON / CSV / NDJSON) |
| Status page | No | Yes (public `/status`) |
| Public benchmarks | No | Yes (`benchmarks/`) |
| March 2026 supply-chain compromise | Yes (1.82.7, 1.82.8) | No |

Notes: LiteLLM "RPS struggles past ~2,000" and "memory under load can exceed 8 GB" come from community reports and stress-test write-ups — not vendor marketing. Reproduce against any current LiteLLM release; numbers vary by Python version and async backend.

**Prompt compression** used to read "Yes" in that table and it was not earning it. `internal/compressor` is a fixed regex list for conversational filler; measured against this repo's own committed corpora it modified **0 of 308** prompts (2717 → 2717 estimated tokens, 0.000%), and it modified **8 of 8** prompts in the corpus that stands in for real coding-agent traffic — because its space-run collapse rewrites leading indentation outside a ``` fence, so two Python nesting levels arrive at the provider as one. As of migration 0117 the request path is gated per workspace (`compression_policy`, default `disabled`; `PUT /v1/workspaces/{id}/compression`), so nothing is rewritten unless you ask. The seam is kept because a real reduction layer will live there; the current technique is what measured worthless. Streaming requests were never compressed at all.

## Migration steps

1. **Stand up Lens** in parallel. The container image is single-process; no Python build chain required.
2. **Issue a Talyvor key** via `POST /v1/workspaces/{wsID}/api-keys` with `{"name": "...", "scopes": ["proxy"]}`, signed in as the workspace owner.
3. **Verify connectivity** — `curl http://your-lens:8080/status.json`. Components panel should show every dep operational.
4. **Flip `base_url`** for one client. Send one request. Confirm it appears in `/v1/api/usage` (or in the dashboard app).
5. **Roll out** to remaining clients.
6. **Decommission LiteLLM** — Lens runs on a fraction of the memory budget.

## What you gain on day one

- **Semantic caching** catches near-duplicate prompts that an exact cache misses, which is a direct reduction in upstream calls on repetitive workloads (FAQ / support-bot shapes benefit most). We do not publish a hit-rate figure: we have not measured one across real customer traffic, and the honest number depends entirely on how repetitive yours is. Read yours off `/v1/api/usage` once you are running.
- **Quality scoring** stops you serving low-quality responses from cache.
- **MCP server** opens up agent-framework integrations that LiteLLM doesn't surface.
- **Audit export** gives SIEM ingestion an obvious answer — `NDJSON` over a long-poll URL — without bolting on a sidecar.
- **Status page** at `/status` gives customers a "is it me or them" answer without filing a ticket.

## What you give up

- LiteLLM has a longer history of `model_alias` configuration patterns. Lens uses `lens:prompt:<name>` references for prompt versioning and per-workspace allow-lists for model gating; the model-aliasing flow is similar but the syntax is different.

## Rollback

`base_url` flips are atomic and reversible per-client. Pointing back at the LiteLLM URL restores the previous behaviour. No data is locked into Lens.
