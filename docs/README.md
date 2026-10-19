# Talyvor Lens Documentation

Index of every doc shipped with the repo. The main [project README](../README.md) lives at the repo root.

## Agent Wallets

Every AI agent gets a wallet — a balance, spending rules, approvals, a card and a live statement — and
Lens enforces the rules before the model call or payment.

- [Agent wallet quickstart](../README.md#agent-wallet-quickstart) — create an agent, fund it, give it rules and its own key, read its statement.
- [Sending and requesting money between agents](agent-transfers.md)
- [Escrow](escrow.md) — credits held until the deal is done.
- [Pots](agent-pots.md) — goals, budgets and reserves inside one agent's wallet.
- [Agent cards](agent-cards.md) — a virtual card whose every purchase is judged by the agent's rules (test mode).
- [Cash-out](cash-out.md) — credits back into money, behind a licensed partner.

## The LENS token economy

- [The LENS token economy](token-economy.md) — the token, mining, the miner binaries, and what a fresh deployment has on by default. A closed test underneath the wallets.

## Migration guides

- [Migrate from Helicone](migrate-from-helicone.md) — 1-line change. Helicone wire format (URL + headers) accepted as-is via the compatibility middleware.
- [Migrate from LiteLLM](migrate-from-litellm.md) — `base_url` flip + new API key. Drops Python supply-chain risk and ~8 GB of memory.

## SDKs

- [Python SDK](../sdk/python/README.md) — 3-line install, drop-in `openai` / `anthropic` wrapper.
- [TypeScript SDK](../sdk/typescript/README.md) — 3-line install, drop-in `openai` wrapper.

## Operations

- [Local standup runbook](local-standup-runbook.md) — bring Lens up standalone and take it from zero to a first real served request that mints a `lens_token_ledger` row; documents every silent-zero trap (stale image, both provider keys, the two pattern flags, `earn_verified`, the LXC bootstrap grant).
- [Remote host](remote-host.md) — put the compose stack on a public VM safely: Caddy TLS front door on :443, lens bound to loopback, NATS closed, and the `LENS_API_KEY`-unset (fail-closed) admin posture. Covers DNS/domain prerequisites, bring-up, TLS verification, and the three-act colleague onboarding (workspace → proxy-scoped key → LXC grant, via `scripts/onboard-trial-user.sh`).
- [Benchmarks](../benchmarks/README.md) — performance suite + reproducible numbers vs LiteLLM / Portkey.
- [Status page](../README.md#status) — public health surface at `/status` and `/status.json`.

## API surface

The full API is documented inline in `internal/api/server.go` (`MountAuthenticated`). High-traffic endpoints:

- `POST /v1/proxy/{openai,anthropic,google,bedrock}/*` — provider proxies.
- `POST /oai/*`, `POST /anthropic/*` — Helicone-compatible URL prefixes.
- `GET  /v1/api/spend/summary` — workspace spend rollup.
- `GET  /v1/audit/export?format=…` — streaming audit log.
- `GET  /v1/api/anomalies/scan` — current cost anomalies.
- `POST /v1/guardrails/check` — pre-flight prompt scan.
- `POST /v1/eval/run` — synchronous eval suite run.
- `GET  /v1/api/keys/pool` — API key pool stats.
- `POST /mcp` — the MCP server (Streamable HTTP; protocol versions 2024-11-05 to 2026-07-28) for Claude, ChatGPT, Cursor and the MCP SDKs; send the agent's key as `Authorization: Bearer`.
- `GET  /mcp/sse` — the same server over the older HTTP+SSE transport (2024-11-05).

## Schema migrations

`migrations/00NN_*.sql` files apply in numeric order. New migrations are idempotent (`IF NOT EXISTS` / `ADD COLUMN IF NOT EXISTS`) so reapplication is safe for blue-green deploys.

## Contributing

- `make test` runs the full Go suite.
- `make vet` runs `go vet ./...`.
- `make bench` runs the performance suite (gated behind `-tags=bench`).
- Python SDK: `cd sdk/python && pytest tests/`.
- TypeScript SDK: `cd sdk/typescript && npm test`.
