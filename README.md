<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/brand/talyvor-logo-dark-notag.svg">
    <img alt="Talyvor" src="docs/brand/talyvor-logo-light-notag.svg" width="360">
  </picture>
</p>

<p align="center">Talyvor — money and markets for AI agents.</p>

# Talyvor Lens

**The gateway that enforces every wallet rule before the call.**

Talyvor gives every AI agent a wallet — a balance, spending rules, approvals, a card and a live
statement. Lens is the part that makes the rules hold. Each agent calls its models through Lens
with a key of its own, and Lens judges every request against that agent's wallet **before the
provider is called**: a request the rules refuse never reaches the provider, one above the
approval amount waits for a person, and every charge lands on the agent's statement.

Lens is an API. The wallet console is the Talyvor app ([app.talyvor.com](https://app.talyvor.com),
or self-host `talyvor-suite`); the Lens host itself serves only a small service page at `/` and
component health at `/status`.

## Agent wallet quickstart

Create an agent, fund it, give it rules and a key, let it call a model, read its statement.

You need a running Lens whose workspace holds some credit: [Quick start](#quick-start-2-commands)
below, then steps 3–4 of [docs/quickstart.md](docs/quickstart.md) (the admin key and the
workspace's first LXC). These calls use that admin key, `LENS_API_KEY`, which is why step 1 names
the agent's owner; signed in to the app as the workspace's owner, the same calls run on your
session and the agent is yours.

Amounts are in µLXC: 1 LXC = 1,000,000 µLXC = $0.10, so $1 is `10000000`.

```bash
export LENS=http://localhost:8080
export WS=default

# 1. Create the agent. Every agent has an owner: the person answerable for what it spends.
AGENT=$(curl -s -X POST $LENS/v1/workspaces/$WS/agents \
  -H "Authorization: Bearer $LENS_API_KEY" -H "Content-Type: application/json" \
  -d '{"name":"researcher","owner_user_id":"you@example.com"}' | jq -r .id)

# 2. Fund it: $2 of the workspace's credit moves into the agent's wallet.
curl -s -X POST $LENS/v1/workspaces/$WS/agents/$AGENT/fund \
  -H "Authorization: Bearer $LENS_API_KEY" -H "Content-Type: application/json" \
  -d '{"amount_ulxc":20000000}'
# → {"agent_id":"agt_…","balance_ulxc":20000000}

# 3. Give it rules: $0.50 a day, the small model only, and anything above $0.10 asks you first.
curl -s -X PUT $LENS/v1/workspaces/$WS/agents/$AGENT/rules \
  -H "Authorization: Bearer $LENS_API_KEY" -H "Content-Type: application/json" \
  -d '{"daily_limit_ulxc":5000000,"approval_above_ulxc":1000000,"allowed_models":["gpt-4o-mini"]}'

# 4. Issue the agent its own key. It is shown once.
AGENT_KEY=$(curl -s -X POST $LENS/v1/workspaces/$WS/agents/$AGENT/keys \
  -H "Authorization: Bearer $LENS_API_KEY" -H "Content-Type: application/json" \
  -d '{"name":"researcher key"}' | jq -r .key)

# 5. The agent calls its model through Lens, with its own key. Any OpenAI client works the same
#    way: change base_url to $LENS/v1/proxy/openai/v1 and use the agent's key.
curl -s $LENS/v1/proxy/openai/v1/chat/completions \
  -H "Authorization: Bearer $AGENT_KEY" -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello!"}]}'

# 6. Read its statement: the funding, then every charge, newest first.
#    Add ?from=2026-10-01&to=2026-11-01&format=csv for a month an auditor can open.
curl -s $LENS/v1/workspaces/$WS/agents/$AGENT/statement \
  -H "Authorization: Bearer $LENS_API_KEY"

# 7. The requests waiting for your approval.
curl -s $LENS/v1/workspaces/$WS/agents/approvals \
  -H "Authorization: Bearer $LENS_API_KEY"
```

What Lens does with the agent's request in step 5, before the provider sees it:

| The agent's wallet says | Lens answers |
|---|---|
| within its rules and its balance | forwards it and charges the agent — buffered and streamed alike |
| a rule refuses it (`gpt-4o` here, or the day's $0.50 spent) | `403`, naming the rule. The provider is never called |
| it could cost more than `approval_above_ulxc` | `403`, naming the approval it filed. The owner approves it once (`POST /v1/workspaces/{wsID}/agents/approvals/{id}/approve`) and the retry goes through |
| its balance cannot cover it | `402` |

## What an agent's wallet holds

All under `/v1/workspaces/{wsID}/agents`. Moving money, creating agents and issuing keys take the
workspace's owner or an admin; reading takes any of the workspace's credentials.

| | | |
|---|---|---|
| **Balance** | fund and withdraw; a top-up that refills it below a floor | `/{id}/fund`, `/{id}/withdraw`, `/{id}/topup` |
| **Rules** | per request, per day, per month; an approval amount; allowed models, providers and marketplace listings; active hours in a timezone; pause on unusual spend | `/{id}/rules` |
| **Approvals** | approve or deny, signed with a passkey once the workspace has one, with a web push when one is filed | `/approvals`, `/approvals/{id}/approve`, `/approvals/{id}/deny` |
| **Payments** | agent to agent, scheduled payments, requests, escrow | `/{id}/pay`, `/{id}/schedules` — [transfers](docs/agent-transfers.md), [escrow](docs/escrow.md) |
| **Card** | a virtual card whose every purchase Lens approves or declines by the agent's rules (test mode) | `/{id}/card` — [agent cards](docs/agent-cards.md) |
| **Pots** | credits set aside as a goal, a budget or a reserve, lockable until a date | `/{id}/pots` — [pots](docs/agent-pots.md) |
| **Statement** | one agent's, or every wallet in the workspace, for any period, JSON or CSV | `/{id}/statement`, `/statement` |
| **Controls** | unusual-spend alerts, pause one agent or all of them, the month-end forecast | `/alerts`, `/{id}/pause`, `/pause-all`, `/forecast` |

The routes are listed with their bodies at the top of
[`cmd/lens/agent_accounts_handler.go`](cmd/lens/agent_accounts_handler.go).

## The gateway underneath

Under the wallet, Lens is a drop-in gateway, and it also cuts what each call costs: an exact and
semantic response cache, opt-in cross-tenant reuse of cached answers (pooling), cross-provider
routing, and document distillation. **We do not publish a headline savings percentage, because we
have not finished measuring one** — see
[What we can and cannot tell you about savings](#what-we-can-and-cannot-tell-you-about-savings).

Drop-in replacement for OpenAI, Anthropic, Google Gemini, AWS Bedrock, Mistral, Groq, and vLLM. Change one URL. Get the wallet's rules, caching, routing, attribution, guardrails, audit, and fallback.

## Why Talyvor Lens?

| | Talyvor Lens | LiteLLM | Helicone |
|---|---|---|---|
| Language | Go | Python | Node.js |
| Single-binary deploy | ✅ | ❌ | ❌ |
| Semantic cache (pgvector) | ✅ | ❌ | ❌ |
| Exact cache (Redis) | ✅ | ✅ | ✅ |
| Idle memory | < 50 MB | ~300 MB | n/a (SaaS) |
| Supply chain | Clean | Compromised Mar 2026 | Acquired by Mintlify |
| Self-hosted | ✅ | ✅ | ❌ |
| Open source | ✅ (core) | ✅ | ✅ |
| Guardrails (PII / injection / topic / regex) | ✅ | partial | ❌ |
| MCP server | ✅ | ❌ | ❌ |
| Prompt versioning + rollback | ✅ | ❌ | ❌ |
| A/B model testing | ✅ | ❌ | ❌ |
| Cost anomaly detection | ✅ | ❌ | ❌ |
| AWS Bedrock (SigV4) | ✅ | ✅ | ❌ |

Comparison from public benchmarks and vendor docs. `make bench` reproduces the *latency and
allocation* figures on this page (it benchmarks the proxy against a mocked upstream); it does
not measure cost.

## What we can and cannot tell you about savings

Every gateway in this category advertises a savings range. Ours used to say 60–80%. Nothing in
this repository computes that number, so it is gone rather than restated.

**The mechanisms are real and you can inspect each one:**

| Mechanism | What it avoids | Where |
|---|---|---|
| Exact cache (Redis) | The whole call, on a repeat request | `internal/cache` |
| Semantic cache (pgvector) | The whole call, on a near-duplicate | `internal/cache/semantic.go` |
| Cross-tenant pooled reuse | The whole call, using another workspace's cached answer (opt-in, both sides) | `internal/cache_pooling` |
| Cross-provider routing | The price difference between a model you asked for and a cheaper one that measured equal on your traffic (opt-in per workspace) | `internal/routing` |
| Document distillation | Re-sending the same document as raw tokens | `internal/distill` |

**What we will not do is multiply a cache-hit rate by your bill and call it a saving.** How much
you actually save depends on how repetitive your traffic is and how well you were already
choosing models — and for a workload that is already tightly specified, the honest answer for
some of these mechanisms is close to zero.

### The baseline has to be an already-optimised one

This is the part most savings claims get wrong, including ours. **Providers now discount cached
input by roughly 90%** (Anthropic bills a cache read at 0.1× input; OpenAI at roughly 0.5× for
the GPT-4o generation). That discount is free — you get it whether or not Lens is in the path.

A percentage measured against naive, full-price, no-caching spend therefore **counts a discount
you already had as if we produced it**. Any figure we eventually publish will be measured against
a baseline that already includes provider-side caching.

Lens's own cost basis already works this way: `alerts.CostUSDDetailed` prices cached-input and
cache-write tokens at the provider's real multipliers (`catalog.withCacheRates`), and the rates
are deliberately set on the conservative side — where a provider's discount is steeper than we
model, we under-state it, so a savings figure derived from this basis errs low.

### Measuring it on your own traffic

**Your workspace's saving is measured, request by request.** Every spend row records what that
request would have cost at the model it asked for with no Talyvor cache — priced on the cache
breakdown the provider actually reported, so the discount above stays in the baseline — and what it
was charged. This month's sum of the difference is

```
GET /v1/workspaces/{wsID}/savings/current-month
```

and the app's Spend screen shows it as "Saved this month". It is those rows added up and nothing
else: no rate is multiplied and nothing is projected. A request served at the model it asked for saves
exactly zero; a cache hit saves the call it replaced, less what the hit was charged (priced flat on
the token estimate the hit itself is priced on — no provider call means no cache breakdown); a routed
request saves the price difference on the same tokens.

The routing path also records a per-request counterfactual: what the call cost, and what the model you
originally asked for would have cost. Admins can read the aggregate:

```
GET /v1/admin/routing-decisions/summary
```

It returns request volume, how often routing overrode your model choice, and the estimated cost
delta. **It is an estimate, not money** — the counterfactual call never happened, so its price is
modelled, not billed. Treat it as the shape of the effect on your traffic, not as an invoice.

`GET /v1/admin/usage/summary` gives the fleet's requests, tokens, provider cost, serves by source
(model, own cache, shared pool) and workspace counts over `?window=` (default 24h). Every admin
total leaves synthetic test workspaces out; add `?synthetic=only` to any of them to read the test
harness's traffic alone.

A published figure across customers — against an already-optimised baseline, on real workloads —
is intended. It is not in this README because it does not exist yet, and the point of this section
is that a number nobody computed should not be the first thing you read. Your own is the one above.

## Quick start (2 commands)

```bash
# 1. Copy the env template and fill in at least one provider key
cp .env.production.example .env

# 2. Bring up the full stack (Lens + Postgres + Redis + NATS)
docker compose up -d
```

Lens is now running at `http://localhost:8080`.

> **Image access:** `ghcr.io/talyvor/talyvor-lens` is a **private**
> package by decision — the binary embeds the full migration SQL (the
> pre-launch token-economy schema), so it is not published anonymously.
> Deploying hosts either authenticate once
> (`docker login ghcr.io -u <user>` with a PAT carrying `read:packages`)
> or build locally from a checkout (`docker compose build`, which the
> compose file supports out of the box).

Open `http://localhost:8080/` for the service page (what this host is, live health,
where to go next), or `http://localhost:8080/status` for per-component health.

The dashboard is a separate app — [app.talyvor.com](https://app.talyvor.com), or
self-host [`talyvor-suite`](https://github.com/Talyvor/talyvor-suite). Lens
itself is an API and serves no account UI.

For a step-by-step walkthrough including issuing your first API key and making your first request, see [docs/quickstart.md](docs/quickstart.md).

## Connect your app (1 line change)

### Python — change only the `base_url`

```python
# Before
client = OpenAI(api_key="sk-...")

# After
client = OpenAI(
    base_url="http://localhost:8080/v1/proxy/openai/v1",
    api_key="tlv_your_lens_key",
)
```

### Python — using the native SDK (3 lines)

```python
from talyvor_lens import LensClient
client = LensClient(lens_url="http://localhost:8080", api_key="tlv_...")
response = client.openai.chat.completions.create(model="gpt-4o", messages=[...])
```

See [`sdk/python/README.md`](sdk/python/README.md) and [`sdk/typescript/README.md`](sdk/typescript/README.md).

### Other providers

| Provider | URL path |
|---|---|
| OpenAI | `/v1/proxy/openai/v1/chat/completions` |
| Anthropic | `/v1/proxy/anthropic/v1/messages` |
| Google Gemini | `/v1/proxy/google/*` |
| AWS Bedrock | `/v1/proxy/bedrock/*` |
| Mistral | `/v1/proxy/mistral/chat/completions` |
| Groq | `/v1/proxy/groq/chat/completions` |
| vLLM | `/v1/proxy/vllm/chat/completions` |
| Helicone-compat | `/oai/v1/chat/completions`, `/anthropic/v1/messages` |

## Seeing your data

Lens is an API: it serves no account UI of its own. These reads are all authenticated
API endpoints — point the dashboard app at them, or call them directly:

| | |
|---|---|
| Spend by model / workspace | `/v1/api/spend/summary`, `/v1/api/spend/by-model` |
| Cache hit rate + top patterns | `/v1/api/cache/stats`, `/v1/api/cache/top-patterns` |
| Per-model usage + serve source | `/v1/api/usage` |
| Circuit-breaker status | `/v1/api/alerts/circuits` |
| Local model availability | `/v1/api/local/status` |
| Live cost anomalies (`>3σ`) | `/v1/api/anomalies/scan` |

The dashboard is [app.talyvor.com](https://app.talyvor.com) (or self-hosted
[`talyvor-suite`](https://github.com/Talyvor/talyvor-suite)), which holds your
key server-side and calls these for you. Unauthenticated, this host serves only `/`
(service page) and `/status` (component health).

## Migrating from another gateway

- **From Helicone** — see [docs/migrate-from-helicone.md](docs/migrate-from-helicone.md). One-line URL change; the `Helicone-Auth` and `Helicone-Property-*` headers keep working through the compatibility layer.
- **From LiteLLM** — see [docs/migrate-from-litellm.md](docs/migrate-from-litellm.md). One `base_url` flip; no Python supply-chain risk.

## SDKs

- Python: `pip install talyvor-lens` — [README](sdk/python/README.md)
- TypeScript: `npm install talyvor-lens` — [README](sdk/typescript/README.md)

## Operations

- Status page: `GET /status` (HTML) or `/status.json`.
- Health probe: `GET /healthz`.
- Audit export: `GET /v1/audit/export?format=json|csv|ndjson` (streams).
- Anomaly scan: `GET /v1/api/anomalies/scan`.
- Run benchmarks: `make bench`.

### Status

`/status` and `/status.json` are public and answer from a snapshot refreshed every 60 seconds. `/status.json` holds
exactly these fields, and no secret, reference id, error text, host name or workspace detail:

- `status`, `version`, `uptime_hours`, `updated_at`
- `components[]`: `name`, `status`, `latency_ms`, `measured`, `message` (a fixed phrase, when there is one), `checked_at`
- `providers[]`: `name`, `status`, `latency_ms`, `checked_at`
- `rails[]`: `service`, `name`, `mode`, `status`, `last_success`, `last_failure`, `capabilities[]` (`key`, `cleared`), and on
  the screening rail `lists_age_hours`: how many whole hours ago the sanctions lists last downloaded
- `rails_summary`: `up`, `down`, `idle`, `down_names`

A new field is added here and to `documentedKeys` in `internal/status/contract_test.go` together.

## Documentation

Full index at [`docs/README.md`](docs/README.md). Highlights:

- [Quickstart](docs/quickstart.md)
- [Migration guides](docs/README.md#migration-guides)
- [Benchmarks](benchmarks/README.md)

## Architecture

Single Go binary, no Python or Node runtime. PostgreSQL (with pgvector) for state, Redis for the hot exact cache + rate-limit ledger, NATS for the learner / anomaly event bus.

Test coverage: 88 of 92 Go packages carry tests, all green in CI (real-Postgres, `-race`). The
count in this line was stale at 37 for some time — it is now derived from the tree, so treat it
as accurate at the commit you are reading and re-derive with
`find . -name '*_test.go' | sed 's|/[^/]*$||' | sort -u | wc -l` if it matters to you.

## License

[Business Source License 1.1](LICENSE) (BUSL-1.1). **Not an open-source licence today.**

You may read, modify and self-host Talyvor Lens, including in production, for your own
organisation's purposes without limit, and an integrator may run it for up to **three clients
at a time**, each on its own deployment. You may **not** run one deployment serving two or more
unrelated organisations. Beyond three concurrent client engagements, or for multi-tenant use,
that is a commercial licence rather than a refusal — `hello@talyvor.com`. See the `Additional Use Grant` in [LICENSE](LICENSE) for the exact
boundary, and the `Change Date`, on which this converts to Apache License 2.0.

**The client SDKs are MIT, not BSL.** `sdk/typescript/` and `sdk/python/` are licensed under the
[MIT License](sdk/typescript/LICENSE) and are explicitly excluded from the Licensed Work above.
They are thin clients you embed in your own application and contain no Talyvor server logic, so
they should never put a licence review in the way of an integration.
