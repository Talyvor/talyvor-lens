# Remote host — putting Lens safely on the internet

The verified sequence to take the `docker compose` stack from a laptop to a **public VM reachable over
HTTPS**, with nothing exposed that shouldn't be. Lens self-authenticates its own `tlv_ws_` keys against
its database and reads **no** gateway-injected identity headers, so it is safe to sit directly behind a
plain reverse proxy — there is no header-spoofing trust to get wrong. Three things had to change from the
local posture; this doc is the result.

**Scope.** The internet-facing stack is `docker-compose.yaml`. `docker-compose.dev.yaml` and the
`tools/trial/*` overlays are **local-only** — do not run them on a public VM.

## What the front door looks like

```
internet ──443──▶  Caddy  ──http──▶  lens:8080        (Caddy terminates TLS; lens never sees the internet)
             ──80──▶  Caddy  (301 → 443)
                                lens :8080 also published on 127.0.0.1 only (loopback: local dev + admin)
                                postgres / pgbouncer / redis / nats  — internal compose network only
```

- **Caddy** terminates TLS on `:443` and reverse-proxies to `lens:8080` over the internal network. It
  provisions and auto-renews a Let's Encrypt certificate for your domain automatically. Chosen over
  Lens's built-in autocert (`cmd/lens/serve.go`, `LENS_TLS_DOMAIN`) because autocert needs a *writable*
  cert cache on the lens container, which would mean relaxing `read_only: true` — a hardening property
  worth keeping. Caddy owns its own cert store (the `caddy_data` volume); lens stays locked down.
- **Lens** is published on `127.0.0.1:8080` **only** — reachable from the VM's loopback (local checks,
  admin bootstrap over SSH) but never from the internet. The only internet path to Lens is through Caddy.
- **NATS, Postgres, PgBouncer, Redis** publish **no** host ports — they talk to Lens over the compose
  network. (NATS is an unauthenticated bus; before this it was published on `0.0.0.0:4222`.)

## 1. What you must supply

| You provide | Notes |
|---|---|
| A **VM** with Docker + Docker Compose | 2 vCPU / 4 GB is plenty to start. Ubuntu/Debian fine. |
| A **domain** (e.g. `lens.example.com`) | Any registrar. A subdomain is fine. |
| A **DNS A record** → the VM's public IP | Must resolve **before** first boot — Let's Encrypt validates over HTTP-01 on :80. |
| Firewall/security-group open on **:80 and :443** | :80 is needed for the ACME challenge + the 301 redirect. Nothing else needs to be open inbound. |
| A **provider key** (`LENS_OPENAI_API_KEY` and/or others) | At least one, or the proxy has nothing to forward. Both OpenAI **and** Anthropic keys must be present to boot (a dummy value satisfies the boot check for a provider you don't use). |
| A strong **`POSTGRES_PASSWORD`** | `openssl rand -hex 32`. The stack refuses to start without it. |

## 2. Bring-up

```bash
git clone <repo> && cd talyvor-lens
cp .env.production.example .env
# edit .env: set LENS_DOMAIN=lens.example.com, POSTGRES_PASSWORD, and a provider key.
docker compose up -d
```

That's it. On first boot Caddy obtains the certificate (a few seconds once DNS resolves) and migrations
apply automatically before lens starts. Leave `LENS_API_KEY` **unset** — see §4.

**What to check**

```bash
docker compose ps                     # every service Up/healthy; migrate Exited (0)
docker compose logs caddy | grep -i certificate   # "certificate obtained successfully"
```

## 3. Verify TLS

From anywhere:

```bash
curl -s https://lens.example.com/healthz            # 200 + a JSON health body
curl -s -o /dev/null -w '%{http_code}\n' http://lens.example.com/healthz   # 308 → https
```

Confirm the certificate is a real Let's Encrypt one and Lens is **not** reachable in the clear:

```bash
echo | openssl s_client -connect lens.example.com:443 -servername lens.example.com 2>/dev/null \
  | openssl x509 -noout -issuer            # issuer=C=US, O=Let's Encrypt, ...
curl -s --max-time 5 http://<VM-public-IP>:8080/healthz   # must FAIL (connection refused) — :8080 is loopback-only
```

(Locally you can dry-run the whole thing with `LENS_DOMAIN=localhost`; Caddy then serves an internal-CA
cert and `curl -k https://localhost/healthz` returns 200.)

## 4. The admin god-key (`LENS_API_KEY`) — leave it unset

Every `requireAdmin` route (`/v1/admin/*`, `/metrics`, workspace provisioning, …) is gated on
`LENS_API_KEY`. **Recommendation: leave it unset on the public host.** Admin then *fails closed* — with
no key, no request can ever be admin (`internal/auth/manager.go`: admin is granted only when the request
key exactly matches a **non-empty** `LENS_API_KEY`). This is a single, robust property that holds for
**every** admin route regardless of path — unlike a reverse-proxy path allowlist, which would have to
enumerate a surface (`/v1/admin/*`, `/metrics`, `POST /v1/workspaces`, `/ha/status`, …) and would
silently miss one.

We deliberately did **not** add a second internal-only listener for admin: that is a Go change, and the
loopback publish already gives a safe admin path (below).

**When you must run an admin action** (creating a workspace, minting a key, granting trial LXC —
the §5 onboarding), do it over
the loopback, never the public door:

```bash
# from your laptop, tunnel to the VM's loopback :8080
ssh -N -L 8080:127.0.0.1:8080 user@lens.example.com &
# temporarily set LENS_API_KEY in .env, then: docker compose up -d lens
curl -s -X POST http://127.0.0.1:8080/v1/workspaces \
  -H "Authorization: Bearer $LENS_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"id":"acme","name":"Acme"}'
# when done, remove LENS_API_KEY from .env and `docker compose up -d lens` again → admin inert.
```

If you would rather keep a persistent admin key, uncomment the `@admin` block in
`deploy/caddy/Caddyfile` so the admin prefix is refused at the public door (defense-in-depth; note the
caveat there that a couple of admin routes live outside `/v1/admin/*`).

## 5. Onboard a colleague (workspace → key → LXC grant)

Workspace keys are per-tenant `tlv_ws_…` credentials; a colleague uses one as a normal bearer token
against `https://lens.example.com`. Onboarding is **three admin acts, not two**: a fresh workspace
has **0 LXC**, so after *create workspace* and *mint key* alone the key's **very first proxy request
fails with 402**. Funding is the third act — it is what `POST /v1/admin/lxc/grant` exists for.

1. **Create the workspace** — `POST /v1/workspaces` (admin-only).
2. **Mint a proxy-scoped key** — `POST /v1/workspaces/{id}/api-keys` with `"scopes":["proxy"]`.
3. **Grant LXC** — `POST /v1/admin/lxc/grant` (admin-only; the ledger row is `admin_grant`, never
   `purchase`, so a comp is always distinguishable from paid revenue).

### That 402 is the agent allocator — not the LXC gate

The rejection is `402 {"error":"agent LXC sub-budget exceeded or insufficient balance"}`, from the
**agent allocator** (`internal/proxy/agent_allocator.go`). It is **default-on**
(`LENS_LXC_AGENT_ALLOCATION_ENABLED`, default TRUE) and **fail-closed**: every **API-key-authed**
proxy request pre-debits its input-cost LXC estimate against a per-key sub-budget *before* the
upstream call, and a 0-LXC workspace cannot cover the first debit. It fires **only on the
scoped-key lane** — JWT- and admin-authed requests carry no API-key ID and structurally never enter
it (which is why *your* admin smoke tests pass while the colleague's key 402s). It is **not** the
LXC gate (`LENS_LXC_GATING_ENABLED` + `LENS_LXC_SHADOW_SPEND_ENABLED`, which sit in the same
pre-serve block but are **both default-off**) — do not chase those flags. The same wall — including
why `/lxc/convert` cannot bootstrap around it — is
[Trap 7 of the local standup runbook](local-standup-runbook.md#the-traps-each-cost-real-time).

### The full ritual

The grant route **does not exist** unless lens booted with `LENS_ADMIN_LXC_GRANT_ENABLED=true`
(default off ⇒ the route is never registered — you get a bare `404`, not a `403`). All three acts
need the §4 god-key, over the loopback tunnel — never the public door.

**On the VM** — arm admin and the grant route, then recreate lens (env applies only on recreate):

```bash
# .env — add BOTH lines, temporarily (both are in .env.production.example):
#   LENS_API_KEY=<openssl rand -hex 32>
#   LENS_ADMIN_LXC_GRANT_ENABLED=true
docker compose up -d lens
```

**From your laptop** — tunnel to the VM's loopback `:8080`, then run the bundled script, which
performs the three acts end to end and prints the key exactly once:

```bash
ssh -N -L 8080:127.0.0.1:8080 user@lens.example.com &
read -r -s LENS_API_KEY && export LENS_API_KEY   # paste the .env value; stays out of history
scripts/onboard-trial-user.sh acme               # workspace id/name = "acme"
```

Or by hand — the same three acts:

```bash
# 1. create the workspace (admin-only; 201 {"id":"acme"})
curl -s -X POST http://127.0.0.1:8080/v1/workspaces \
  -H "Authorization: Bearer $LENS_API_KEY" -H 'Content-Type: application/json' \
  -d '{"id":"acme","name":"Acme"}'

# 2. mint a scoped key for it (201). Give it the proxy scope so it can call /v1/proxy/*.
curl -s -X POST http://127.0.0.1:8080/v1/workspaces/acme/api-keys \
  -H "Authorization: Bearer $LENS_API_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"alice-laptop","scopes":["proxy"]}'
# → {"key":"tlv_ws_…", …}  ← returned ONCE. Hand THIS to your colleague (a password manager, not chat).

# 3. fund it — WITHOUT this the key's first request is the 402 above (200 on success)
curl -s -X POST http://127.0.0.1:8080/v1/admin/lxc/grant \
  -H "Authorization: Bearer $LENS_API_KEY" -H 'Content-Type: application/json' \
  -d '{"workspace_id":"acme","amount_ulxc":50000000,"reason":"trial onboarding"}'
# → {"workspace_id":"acme","granted_ulxc":50000000,"new_balance_ulxc":50000000}
```

Do **not** re-run the acts against an existing workspace: `POST /v1/workspaces` is an **upsert**,
so a re-run "succeeds" and a second grant silently double-funds the tenant. One workspace per trial
user, one run per workspace (the script refuses to touch an id that already exists).

**Then disarm** (on the VM): remove `LENS_API_KEY` from `.env` — and remove
`LENS_ADMIN_LXC_GRANT_ENABLED` too once onboarding is done (with the god-key gone the grant route
merely 401s, but removing both restores the exact §4 fail-closed baseline) — then
`docker compose up -d lens` again.

### Why 50 LXC

`amount_ulxc: 50000000` (= 50 LXC = $5 at the 1 LXC = $0.10 peg) is deliberate: it equals
`DefaultAgentCeilingLXC` (`internal/economy/agent_subbudget.go`), the allocator's **per-key**
sub-budget ceiling. A key's metered spend is capped at its ceiling, the spent counter never resets,
and there is no HTTP route to raise a ceiling — so on a one-key trial workspace anything granted
above 50 LXC is stranded balance the key can never spend. A bigger number buys nothing.

### The colleague's side

They point any OpenAI/Anthropic client at the proxy:

```bash
curl https://lens.example.com/v1/proxy/openai/v1/chat/completions \
  -H "Authorization: Bearer tlv_ws_…" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

The key never grants admin, and a workspace can only ever see its own data.

## Port exposure — the full sweep

| File | Service | Host publish (after) | Why |
|---|---|---|---|
| `docker-compose.yaml` | **caddy** | `0.0.0.0:80`, `0.0.0.0:443` | the only internet-facing surface (TLS + ACME) |
| `docker-compose.yaml` | **lens** | `127.0.0.1:8080` | loopback only — local dev, `make status`, `tools/trial`, admin over SSH; **never** the internet |
| `docker-compose.yaml` | nats | *(none)* | was `0.0.0.0:4222` — an unauthenticated bus; now internal-only (`nats://nats:4222`) |
| `docker-compose.yaml` | postgres / pgbouncer / redis / autoheal | *(none)* | internal compose network only |
| `docker-compose.dev.yaml` | postgres / redis / nats | `127.0.0.1:5432 / 6379 / 4222 / 8222` | **local dev only** — loopback-bound so host tooling works but nothing is exposed if run on a VM. Do not run this file on a public host. |
| `docker-compose.trial*.yaml` | (overlays base) | inherits the base's `127.0.0.1:8080` | local test harness (`tools/trial`); publishes no ports of its own |

## Not covered here

The Helm chart under `deploy/helm/lens/` is **not exercised by this compose-based path**. Its
default image is `ghcr.io/talyvor/talyvor-lens:latest` (until 12 Oct 2026, when the repository moved
to the Talyvor organisation, it was `ghcr.io/gaboracnicolai/talyvor-lens`), but no live-cluster
install has been run from it — treat it as static-validated only. Use
compose for a single-VM host.
