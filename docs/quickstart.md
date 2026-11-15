# Talyvor Lens — Quick Start

End-to-end setup in under five minutes. By the end of this guide you'll have Lens running locally, an issued API key, a funded workspace, and a verified working proxy request against OpenAI.

## Prerequisites

- Docker and Docker Compose installed (Docker Desktop on macOS / Windows; `docker.io` + `docker-compose-plugin` on Linux).
- **Both** an OpenAI **and** an Anthropic API key — Lens requires `LENS_OPENAI_API_KEY` **and** `LENS_ANTHROPIC_API_KEY` to start (see Step 1). If you only use one provider, a dummy non-empty value satisfies the boot check for the other. The remaining providers (Google, Mistral, Groq, AWS Bedrock) are optional.
- A terminal with `curl` and `jq` (optional — for pretty-printing JSON responses).

## Step 1 — Configure

```bash
cp .env.production.example .env
```

Edit `.env`. The minimum to bring up Lens:

```bash
LENS_OPENAI_API_KEY=sk-...            # real — this guide proxies to OpenAI in Step 5
LENS_ANTHROPIC_API_KEY=sk-dummy       # required to boot; a dummy non-empty value is fine if you don't use it
POSTGRES_PASSWORD=changeme-in-production
LENS_DOMAIN=localhost                 # required; localhost for a local run (real hostname on a public host)
LENS_API_KEY=paste-openssl-rand-hex-32 # LOCAL admin key — used in Steps 3–4 (mint a key, fund the workspace)
LENS_ADMIN_LXC_GRANT_ENABLED=true     # exposes the admin funding endpoint used in Step 4 (read once, at boot)
```

**Both `LENS_OPENAI_API_KEY` and `LENS_ANTHROPIC_API_KEY` are mandatory.** `config.Load` (`internal/config/config.go`) hard-requires both and refuses to start with `ErrMissingEnv` if *either* is empty — this is **not** "at least one". And because `docker-compose.yaml` defaults these two with `:-` (empty) rather than `:?`, an unset key does **not** fail `docker compose up`; instead the `lens` container comes up and **crash-loops**. The symptom is a `lens` service stuck restarting — `docker compose logs lens` shows `missing required environment variables: [LENS_ANTHROPIC_API_KEY]`. A dummy non-empty string satisfies the boot check for a provider you don't actually call.

The *other* providers (Google, Mistral, Groq, AWS Bedrock) really are optional: a missing key there only disables that provider's `/v1/proxy/*` route (503) and does not block startup.

`LENS_DOMAIN` is also required. Unlike the provider keys, `docker-compose.yaml` passes it with `:?`, so an unset value makes `docker compose up` **abort immediately** — `error … required variable LENS_DOMAIN is missing a value` — before any container starts. Use `localhost` for a local run; on a public host set your real hostname, and the bundled Caddy service provisions the TLS certificate for it automatically (see [remote-host.md](remote-host.md)).

`LENS_API_KEY` is the admin credential. Minting keys, creating workspaces, and funding them are admin operations, so **locally you set it** — generate one with `openssl rand -hex 32` and paste it; you use it in Steps 3 and 4 (mint a key, fund the workspace). This local-admin posture is deliberate and is the **opposite** of the remote recommendation: on a public host you leave `LENS_API_KEY` **unset** so the admin surface fails closed (see [remote-host.md](remote-host.md)). Don't conflate the two.

`LENS_ADMIN_LXC_GRANT_ENABLED=true` registers the admin funding endpoint (`POST /v1/admin/lxc/grant`) that Step 4 uses. It is read **once, at boot** — without it the route is never registered at all (a bare `404`, not a `403`). Same public-host caveat: leave it off there except while actively onboarding ([remote-host.md](remote-host.md) §5).

For the full set of first-boot traps, see [local-standup-runbook.md](local-standup-runbook.md) (Trap 3, which documents this exact requirement).

## Step 2 — Start

```bash
docker compose up -d
```

Wait ~10 seconds for healthchecks to settle. Verify:

```bash
docker compose ps
```

The long-running services — `lens`, `postgres`, `pgbouncer`, `redis`, `nats`, and `autoheal` — should show `healthy`; `caddy` runs no healthcheck, so it shows plain `Up`. The `migrate` service is a one-shot (`restart: "no"`): it applies the schema and exits 0 (shown as `Exited (0)`); that's normal, not a failure.

Verify the proxy is up:

```bash
curl -s http://localhost:8080/healthz
# (keys are alphabetical — Go marshals the map sorted)
# {
#   "checks": {
#     "database":     {"latency_ms": 1, "status": "healthy"},
#     "local_models": {"latency_ms": 0, "status": "healthy"},
#     "read_replica": {"latency_ms": 0, "status": "healthy"},
#     "redis":        {"latency_ms": 0, "status": "healthy"}
#   },
#   "status": "healthy",
#   "uptime_seconds": 42,
#   "version": "0.1.0"
# }
```

Returns HTTP `200`. A read replica that is not configured reads `healthy` (it is off, not broken). A check that works with a warning reads `degraded`; only an actual dependency outage flips a check to `unhealthy` and the response to HTTP `503`. With the admin key (`Authorization: Bearer $LENS_API_KEY`) the answer also carries each check's `detail` and the `database_pool` and `requests` sections.

And the public status page:

```bash
curl -s http://localhost:8080/status.json | jq .status
# "operational"
```

## Step 3 — Mint your first workspace key

Minting a key is an admin operation, so it uses the `LENS_API_KEY` from Step 1 as the bearer token. Lens seeds a `default` workspace at boot, so you can mint against it directly — there is no workspace to create first. Give the key the `proxy` scope so it can call `/v1/proxy/*`: that scope is enforced, and a key without it is refused on the proxy path with `403`.

```bash
curl -X POST http://localhost:8080/v1/workspaces/default/api-keys \
  -H "Authorization: Bearer $LENS_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-first-key","scopes":["proxy"]}'
```

On success it returns `201 Created` with the raw key **once** — save it, it's never shown again (the `warning` field says as much):

```json
{
  "id": "b3f1c2a4-8e7d-4c1a-9f2b-0a1b2c3d4e5f",
  "key": "tlv_ws_1a2b3c4d5e6f7a8b9c0d1e2f30",
  "name": "my-first-key",
  "prefix": "tlv_ws_1a2b3c4d",
  "scopes": ["proxy"],
  "warning": "Store this key securely. It will not be shown again."
}
```

Export the `key` value for the next steps:

```bash
export LENS_KEY=tlv_ws_paste_yours_here
```

> Prefer a dedicated tenant over `default`? Create the workspace first (also admin), then mint against it — and in Step 4 fund **that** workspace (`"workspace_id":"acme"`):
> ```bash
> curl -X POST http://localhost:8080/v1/workspaces \
>   -H "Authorization: Bearer $LENS_API_KEY" -H "Content-Type: application/json" \
>   -d '{"id":"acme","name":"Acme"}'
> # then POST /v1/workspaces/acme/api-keys with the same body as above
> ```
> Or run all three acts (create → mint → fund) in one go with `scripts/onboard-trial-user.sh acme` — its default `LENS_ADMIN_URL` of `http://127.0.0.1:8080` **is** the local stack; no tunnel involved.

## Step 4 — Fund the workspace (or your first request fails 402)

Every workspace starts at **0 LXC** — the boot-seeded `default` included — and the **agent allocator** (default-on, fail-closed) pre-debits each scoped-key request's input-cost LXC estimate *before* the upstream call. At 0 LXC that first debit fails and the proxy answers `402 {"error":"agent LXC sub-budget exceeded or insufficient balance"}`.

This is deterministic for the exact request in Step 5, not a corner case: `gpt-4o-mini` is in the price catalog (`internal/catalog/seed.go`, $0.15/1M input), and `lxcEstimate` (`internal/proxy/lxc_gate.go`) rounds any non-zero estimate **up** to whole µLXC — even `"Hello!"` (6 bytes ⇒ 1 estimated token ⇒ 2 µLXC) exceeds a balance of 0. The only inputs that skip the debit entirely (estimate 0) are a model **absent from the catalog** or a prompt **under 4 bytes**; neither applies here, or to any normal request.

Fund `default` with the admin grant (the route exists because Step 1 set `LENS_ADMIN_LXC_GRANT_ENABLED=true`; it is read at boot — if you skipped it, add it to `.env` and run `docker compose up -d lens` first):

```bash
curl -X POST http://localhost:8080/v1/admin/lxc/grant \
  -H "Authorization: Bearer $LENS_API_KEY" \
  -H "Content-Type: application/json" \
  -H "X-Talyvor-Operator: $USER" \
  -H "X-Talyvor-Step-Up: <the current code from the authenticator app holding LENS_OPERATOR_STEP_UP_SECRET>" \
  -d '{"workspace_id":"default","amount_ulxc":50000000,"reason":"quickstart funding"}'
# → {"workspace_id":"default","granted_ulxc":50000000,"new_balance_ulxc":50000000}
```

`50000000` µLXC = 50 LXC ≈ $5 of estimate headroom — deliberately equal to the per-key agent sub-budget ceiling, so a larger grant buys nothing (see "Why 50 LXC" in [remote-host.md](remote-host.md) §5).

**Local vs public posture — don't conflate.** Locally, hitting an admin endpoint on `localhost` with `LENS_API_KEY` set in `.env` is the normal way of working. On a **public host** these same three acts (workspace → key → grant) are a deliberate ritual: `LENS_API_KEY` armed temporarily, admin reached only over a loopback SSH tunnel, both disarmed afterwards. That tunnel discipline belongs to the public host — [remote-host.md](remote-host.md) §5 — not here.

## Step 5 — Make your first proxied request

```bash
curl http://localhost:8080/v1/proxy/openai/v1/chat/completions \
  -H "Authorization: Bearer $LENS_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [{"role":"user","content":"Hello!"}]
  }'
```

Lens forwards the request to OpenAI using `LENS_OPENAI_API_KEY` from your `.env`, caches the response, scores it, and records the cost. The response is OpenAI-format unchanged.

## Step 6 — See what it recorded

Lens is an API and serves no account UI, so read it with your key:

```bash
curl -H "Authorization: Bearer $LENS_KEY" \
  http://localhost:8080/v1/api/usage
```

You'll see one request, and a cache hit rate of 0% — one request, one miss.

Send the same request again and re-run the command: the hit rate jumps to 50%, because
the second request was served from the exact cache with no upstream API call.

For a browser view, run the dashboard app ([app.talyvor.com](https://app.talyvor.com), or
self-host `talyvor-suite`) — it holds your key server-side and calls these endpoints for
you. `http://localhost:8080/` serves only a small service page, and `/status` component
health.

## Step 7 — Check your savings

```bash
curl -H "Authorization: Bearer $LENS_KEY" \
  http://localhost:8080/v1/api/spend/summary
```

After a few requests the summary shows total cost, cached request count, and the dollars Lens saved you by serving from cache.

## Next steps

- **Give an agent a wallet** — the [agent wallet quickstart](../README.md#agent-wallet-quickstart): an agent of its own, funded, with rules and its own key, and a statement of every charge.
- **Set up per-workspace logging policy** — `PUT /v1/workspaces/{wsID}/logging` to switch a workspace to `metadata` (no prompt text persisted) or `none` (privacy mode).
- **Configure guardrails** — `PUT /v1/guardrails/policy` to enable PII redaction, prompt-injection blocking, blocked topics, or custom regex rules per workspace.
- **Add a prompt template** — `POST /v1/prompts` to register a versioned prompt; reference it from your application as `lens:prompt:<name>`.
- **Wire fallback chains** — `PUT /v1/api/fallback/chains/{provider}` to control which providers Lens falls over to when the primary fails.
- **Export audit logs** — `GET /v1/audit/export?format=ndjson` streams the audit log directly into your SIEM.
- **Migrate existing clients** — see [migrate-from-helicone.md](migrate-from-helicone.md) or [migrate-from-litellm.md](migrate-from-litellm.md).

## Stopping + cleanup

```bash
make down     # stop services, keep data volumes
make reset    # stop services and DELETE data volumes (fresh start)
```

Or directly:

```bash
docker compose down              # stop, keep volumes
docker compose down -v           # stop and wipe volumes
```

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `lens` container restarting / crash-looping | `LENS_OPENAI_API_KEY` or `LENS_ANTHROPIC_API_KEY` unset — `config.Load` requires **both** (compose defaults them with `:-`, so `up` succeeds but lens exits) | `docker compose logs lens` → `missing required environment variables`; set both in `.env`, then `docker compose up -d` |
| `lens` container restarting | Postgres init failed | `docker compose logs migrate` |
| `503 Service Unavailable` from `/v1/proxy/openai/*` | `LENS_OPENAI_API_KEY` is invalid or a dummy value (a *missing* one crash-loops the container instead — see the row above) | Set a real key, `docker compose up -d` |
| `401 Unauthorized` from proxy | Key not recognized (wrong or expired) | mint a fresh one (Step 3); list a workspace's keys with `GET /v1/workspaces/default/api-keys` (admin or that workspace's key) |
| `403 Forbidden` from `/v1/proxy/*` | Key authenticated but lacks the `proxy` scope (enforced since #329) | re-mint with `"scopes":["proxy"]` (Step 3) |
| `402 Payment Required` from `/v1/proxy/*` (`agent LXC sub-budget exceeded or insufficient balance`) | The **agent allocator** (`LENS_LXC_AGENT_ALLOCATION_ENABLED`, default **on**, fail-closed; fires **only on the scoped-key lane** — admin/JWT traffic never enters it) pre-debits the request's LXC estimate, and the workspace can't cover it — fresh workspaces, the seeded `default` included, start at 0 LXC. **Not** the LXC gate (`LENS_LXC_GATING_ENABLED` + `LENS_LXC_SHADOW_SPEND_ENABLED`, both default-off). Same wall as [Trap 7](local-standup-runbook.md#the-traps-each-cost-real-time) | Fund the workspace (Step 4). The allocator flag isn't in `docker-compose.yaml`'s env list, so it can't be disabled from `.env` — fund, don't fight it |
| `503 Service Unavailable` from `/v1/proxy/*` (`Lens could not take the hold for this request; nothing was charged, and it can be sent again`) | Lens could not take the agent's hold this time, after retrying. It is never a shortfall: nothing was held or charged | Send the request again. If it keeps happening, file it: a funded agent's simultaneous requests should never see it |
| Dashboard shows no data | First request hasn't fired yet | Send a test request via curl |
| Status page shows red | One of postgres/redis/nats is down | `docker compose ps`, restart the offender |
