# What Lens does while Postgres is down

Lens keeps answering while its database is unreachable (#174), but not every request is served
and not every served request is billed. This page states today's behaviour, each with the line
that causes it. It describes the code; it changes none of it.

How long a single request waits before it is answered is in B39.5 (#847): at most about 5 s
for a pre-serve hold or debit (`preServeHoldTimeout`, internal/proxy/agent_allocator.go), 10 s
for each post-answer write. The cross-replica windows that apply when the database is healthy
are in [deploy/helm/lens/README.md — Cross-node policy consistency](../deploy/helm/lens/README.md#cross-node-policy-consistency).

## At a glance

Two questions decide each request: does its credential still authenticate, and is it served and
billed once it does.

| Credential | Authenticates during the outage? |
|---|---|
| `tlv_` key (from `POST /v1/api/keys`), **revoked or not**, cached on this replica | **Yes**, until the cache entry is 5 min old; then 401 |
| `tlv_` key not cached on this replica | No — 401 "invalid API key" |
| Workspace key `tlv_ws_…` | No — 401; looked up in Postgres on every request |
| Chat session key `tlv_sk_…` | No — 503 with `Retry-After: 2` |
| JWT, global admin key | Yes — checked without Postgres |

| Once authenticated | Served? | Billed? |
|---|---|---|
| Workspace request — no key ID (JWT, Chat) | **Yes** — the credit check fails open | **No** — the debit fails, is logged, and nothing retries it |
| Agent request — a key ID, which every `tlv_` and `tlv_ws_` key carries | **No** — 503; the pre-serve hold or debit fails closed | nothing served, nothing charged |

So while `LENS_LXC_AGENT_ALLOCATION_ENABLED` is on (its default), a revoked `tlv_` key gets
through authentication for up to 5 minutes but its requests are refused 503 at the debit — except
those the estimate prices at zero (every embeddings request, any prompt under 4 bytes), which are
served without a debit ([agent_allocator.go:150](../internal/proxy/agent_allocator.go#L150),
[agent_allocator.go:311-312](../internal/proxy/agent_allocator.go#L311-L312)). With it off, a
`tlv_` key's request is a workspace request: served, unbilled.

On Helm, `/readyz` checks the database
([cmd/lens/ha_wiring.go:146-153](../cmd/lens/ha_wiring.go#L146-L153)), so every replica is taken
out of its Service while Postgres is down and nothing above is reached through it. On
docker-compose the healthcheck is `/healthz`, which does not check the database
([docker-compose.yaml:298](../docker-compose.yaml#L298)), so the tables apply.

## API keys (`tlv_…`, from `POST /v1/api/keys`) — a revoked key works for up to 5 minutes

- **The window: 5 minutes.** `cacheTTL = 5 * time.Minute` —
  [internal/auth/apikeys.go:25](../internal/auth/apikeys.go#L25). A cache hit returns the key
  without touching Postgres ([apikeys.go:160](../internal/auth/apikeys.go#L160)); the entry's age
  is counted from the last time it was read from the database, not from its last use
  ([apikeys.go:182](../internal/auth/apikeys.go#L182), checked at
  [apikeys.go:198](../internal/auth/apikeys.go#L198)).
- **A key revoked during the outage is not revoked.** `Revoke` writes `active = false` first and
  returns the error when that write fails, before it evicts anything
  ([apikeys.go:245](../internal/auth/apikeys.go#L245)). The key keeps working on every replica
  that has it cached, until that replica's entry is 5 minutes old.
- **A key revoked just before the outage** is evicted at once on the replica that revoked it, and
  honoured on any other replica that cached it for up to 5 minutes — the same window as when
  Postgres is healthy.
- **After that, good keys stop too.** When an entry expires the key is looked up again; the query
  fails and the answer is `invalid API key`
  ([apikeys.go:176-178](../internal/auth/apikeys.go#L176-L178)), which the middleware returns as
  401 ([internal/auth/middleware.go:93](../internal/auth/middleware.go#L93)). So each `tlv_` key
  authenticates on a replica for at most 5 minutes into an outage, revoked or not.

## Other credentials

- **Workspace keys (`tlv_ws_…`)** are read from Postgres on every request; a failed lookup is
  `ErrInvalidAuth`, a 401 ([internal/auth/manager.go:462-465](../internal/auth/manager.go#L462-L465)).
  There is no window: a revoked one stops at once, and so does every good one.
- **Chat session keys (`tlv_sk_…`)** answer 503 "your session could not be checked just now"
  when the lookup fails, so Chat does not sign the person out
  ([manager.go:419-425](../internal/auth/manager.go#L419-L425), B27.5).
- **JWTs and the global admin key** are checked without the database
  ([manager.go:385-400](../internal/auth/manager.go#L385-L400),
  [manager.go:477](../internal/auth/manager.go#L477)).

## Workspace requests — served, and not billed

A request with no agent key ID (JWT, Chat) is a workspace request.

- **Served.** The credit check reads the balance; when the read fails it logs
  `LXC gate balance read failed (failing open; request allowed)` and lets the request through —
  [internal/proxy/lxc_gate.go:234](../internal/proxy/lxc_gate.go#L234). The plan-allowance read
  fails open the same way ([internal/proxy/allowance.go:65](../internal/proxy/allowance.go#L65)),
  and so do Chat's admission reads ([internal/proxy/chat_billing.go:73](../internal/proxy/chat_billing.go#L73),
  [chat_billing.go:90](../internal/proxy/chat_billing.go#L90)).
- **Not billed.** After the answer, the debit is attempted once and its failure is logged:
  - a Chat request: `billing: chat request served UNBILLED — prepaid debit failed`
    ([internal/proxy/chat_billing.go:122-125](../internal/proxy/chat_billing.go#L122-L125); pooled
    serves at [chat_billing.go:184](../internal/proxy/chat_billing.go#L184));
  - any other request: `economy: shadow LXC debit failed (observational; serve unaffected)`
    ([internal/proxy/shadow_lxc.go:77-80](../internal/proxy/shadow_lxc.go#L77-L80)).

  Nothing queues or retries either debit, so the charge is not on the workspace's balance after
  Postgres returns. Its amount is in the log line (`ulxc` / `lxc_ulxc`).
- **Not yet decided.** Whether a workspace's requests should keep being served unbilled during
  an outage is Nicolai's decision (FOUND.md, 12 Oct). Until it is made, the behaviour above is
  the behaviour.

## Agent requests — refused, nothing charged

A request carrying a key ID, with `LENS_LXC_AGENT_ALLOCATION_ENABLED` on (its default), is held or
debited before it is served. A `tlv_` key from `POST /v1/api/keys` counts: it is the workspace's
own key, held and charged like an agent's (B17.121,
[agent_allocator.go:96-99](../internal/proxy/agent_allocator.go#L96-L99)).

- **Refused, failing closed.** When the pre-serve debit fails rather than being refused by a
  balance or a rule, it logs `agent debit failed (failing closed)` and the request is blocked —
  [internal/proxy/agent_allocator.go:186-189](../internal/proxy/agent_allocator.go#L186-L189); the
  pre-serve hold, when reservations are on, fails closed the same way
  ([agent_allocator.go:323-327](../internal/proxy/agent_allocator.go#L323-L327)).
  The answer is 503 "Lens could not check this agent's budget just now", not a 402
  ([agent_allocator.go:238-242](../internal/proxy/agent_allocator.go#L238-L242)).
- **Nothing charged.** The upstream model is never called, so there is nothing to bill.

Both paths — streaming and buffered — pass through the same gates before the request forks
([internal/proxy/proxy.go:1029-1090](../internal/proxy/proxy.go#L1029-L1090)), so the
table holds for both.
