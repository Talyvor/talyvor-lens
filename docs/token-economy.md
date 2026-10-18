# The LENS token economy (closed test)

Talyvor's product is **Agent Wallets** — see the [project README](../README.md). This page is the
token layer underneath it: the LENS token, how it is mined, and the switches that arm it. It used
to sit in the README; it moved here so the README can lead with what a user actually does. Nothing
below changed in the move.

## What is on by default

A self-hoster's most reasonable question, and one this README previously answered wrongly.

| Switch | Default | Effect |
|---|---|---|
| `LENS_ECONOMY_ENABLED` | **true** | Master economy switch. Registers the economy route surface and permits mint/earn/stake/marketplace state. Set `false` for a pure fiat-SaaS deployment — it then force-offs every economy gate regardless of their own env values. |
| Pool-royalty / distill / pattern mints | **on** (under the master switch) | The three live traffic mints, default-on for the closed test. |
| `LENS_ANNOTATION_MINTING_ENABLED` | false | Annotation mint — spendable-immediate, so explicit opt-in. |
| `LENS_TRUSTFUL_COMPUTE_MINT_ENABLED` | false | Legacy receipt-less compute mint. An unprotected mint path is opt-in, never on by accident. |
| `LENS_MINT_RATE_CAP_LENS_24H` | 1000 | Per-workspace ceiling on minted LENS across **all** mint types in a rolling 24h. `0` disables. |
| `LENS_BILLING_ENABLED` | false | Stripe billing / LXC purchase. Off means no one can buy LXC on this deployment. |

**The important consequence:** economy-on does *not* mean a fresh workspace earns anything. Every
mint-type credit passes the **verified-to-earn** gate below, which requires a completed real-money
LXC purchase or an admin vouch. A default deployment has the economy *armed* and mints *nothing*
until a workspace is verified. That is a meaningfully different statement from "off by default",
and the previous wording obscured it.

## LENS Token Economy

LENS is an internal unit on Lens's own Postgres ledger, used only in the closed test underneath
the wallets. There are **two units** and they behave differently — the distinction matters before
any number below makes sense:

- **LXC** is the billing credit, and its peg is **fixed**: 1 LXC = $0.10 of compute credit,
  never computed or adjusted (`economy.LXCUSDValue`). This is what inference is billed against.
- **LENS** is the mined token. `1 LENS = $0.10` is its **published nominal** peg
  (`marketplace.LENSPerUSD`), but LENS does not convert to LXC at a fixed rate: the conversion
  runs through a **floating** rate engine (`economy.RateEngine.ComputeFairRate`) derived from
  supply and backing, plus a 5% spread, bounded to ±10% movement per approval and floored until
  a live marketplace price exists. **Treat $0.10/LENS as a unit of account, not a redemption
  guarantee.**

Everything in this section is gated on the economy master switch (see
[What is on by default](#what-is-on-by-default)), and staking or trading requires a workspace
that already holds LENS.

### Mining Types

| # | Track | What you contribute | Earn rate |
|---|---|---|---|
| 1 | **Pool royalty** | A cached answer of yours reused by *another* workspace (opt-in both sides) | `s` × the provider cost that call avoided; `s` = 0.5 by default (`LENS_POOL_ROYALTY_SHARE`) |
| 2 | **Compute mining** | GPU inference capacity (Ollama / vLLM / llama.cpp) | 0.025–0.150 LENS / 1k tokens (by GPU class) |
| 3 | **Embedding mining** | CPU-friendly embedding generation | 0.002–0.004 LENS / 1k embeddings |
| 4 | **Quality oracle** | Stake-gated annotation of LLM responses | 0.100 LENS / annotation + agreement bonus |
| 5 | **Pattern mining** | Anonymised routing patterns (opt-in) | 0.001 LENS × (1 + rarity × 4), but see the reachable ceiling below |

**Row 1 changed, and the old row was wrong.** This table used to list a *cache-mining* track at
"0.001–0.010 LENS / hit". That track (`CacheMiner`) was **retired**, not renamed: it duplicated
pool-royalty at the same serve point (and would have double-minted if both were wired), and its
one unique path — minting for a hit on your *own* cache — is self-inflation, since no second
party received anything. The cache moat is real; it is delivered by pool-royalty, which pays only
when the value actually goes to someone else. The `talyvor-cachenode` binary is unaffected: you
still contribute cache capacity with it, you now earn through row 1.

**Pattern mining's ceiling is 2×, not 5×.** The formula reads as though rarity 1.0 pays a 5×
multiplier. It cannot: the anti-gaming corroboration floor caps reachable rarity at 0.25, so the
reachable ceiling is `1 + 0.25×4 = 2.0`. The public `/v1/tokens/rates` API was corrected to
advertise the reachable 2× some time ago; this table was not, until now.

### Node Software

| Binary | Default port | Purpose |
|---|---|---|
| `talyvor-lens` | 8080 | The Lens proxy itself |
| `talyvor-node` | 9090 | GPU inference mining |
| `talyvor-cachenode` | 9091 | Cache contribution mining |
| `talyvor-embednode` | 9092 | Embedding farm mining |

Build all four: `make binaries` (drops them into `./bin/`).

### Token Economics — built and wired

Each of these is implemented, wired to a route, and gated on the economy master switch. All of
them additionally need a workspace that already holds LENS.

- **Staking** and the **Marketplace** (peer-to-peer LENS trading) are **not served** since B18.1:
  buying credited the buyer without debiting anyone, and stake yield had no ceiling. Their routes
  are unregistered until Nicolai decides whether to retire or fix them; the logic remains in
  `internal/economy`.
- **Quality oracle stake**: **10 LENS** minimum lockup before an annotation is accepted
  (`mining.StakeRequirement`), Sybil-resistant.
- **LXC peg**: 1 LXC = $0.10, fixed (see above).

### Token Economics — roadmap, not built

**These are deliberate future work for the agent-service marketplace**, where agents transact in
LENS with each other directly rather than a human paying a fiat invoice. They are listed here so
they are not mistaken for shipped features — and so nobody deletes them later as dead weight,
because the burn primitive is a real, tested part of the eventual design.

- **LENS-burn discount — NOT BUILT.** The intent is that spending LENS on inference costs less
  than paying fiat, with the burned LENS leaving circulation permanently. **No discount
  multiplier exists anywhere in the codebase today**; the only trace is a comment in
  `economy/marketplace.go` naming the future path. Paying with LENS currently gets you no
  discount, because there is no LENS-payment path for inference at all.
- **Burn in circulation — PRIMITIVE ONLY.** `mining.LedgerStore.Burn` is implemented and tested,
  and `GetTotalBurned` is wired into the economy-stats readout — but **it has no production
  caller**. Nothing in a running Lens ever burns LENS, so total-burned reads zero and will keep
  reading zero until the discount path above exists. The primitive is kept deliberately: it is
  the supply-side half of the burn-and-mint design the marketplace needs.

Why they are not simply deleted: the agent marketplace is the reason the token is a token rather
than a loyalty-points balance. Removing the burn machinery would mean rebuilding it, and the
tested primitive is the cheap half.

### Sybil resistance (verified-to-earn)

**Correction:** this section used to say the token economy "ships dark (off by default)". It does
not. `LENS_ECONOMY_ENABLED` **defaults TRUE** (`config.go`: `c.EconomyEnabled = true`, explicit
opt-out), and three traffic mints — pool-royalty, distill and pattern — are default-on under it.
What actually keeps a fresh deployment from minting value is not the master switch but the **U6
Sybil floor** below, which is wired unconditionally and which the master switch cannot lift:

- **Verified-to-earn gate.** A workspace may mint / accrue royalty only when it is **verified-to-earn**: it has a **completed real-money LXC purchase** (derived at read time) OR an admin-set `earn_verified` flag (the enterprise / self-host vouch). Refunded / anomalous purchases do **not** count (closes the buy→refund→stay-verified loop). The gate is enforced at the **ledger chokepoint** (`applyTx` + `heldInner`): every mint-type credit — cache, compute, embedding, annotation, pattern, PoVI receipt, and the pool-royalty held mint — passes through it; conservation moves (marketplace, unstake, LENS→LXC convert) are never gated. The gate is wired **unconditionally** — a safety restriction the economy master-switch cannot lift.
- **Idempotent mints.** The previously-unprotected compute / cache / embedding tracks now claim a `(request_id, workspace_id, mint_type)` row before crediting (the pattern track's proven shape). `request_id` must be **server-derived** work-product content; an empty id mints nothing.
- **Legacy trust-mint off by default.** The receipt-less compute mint (`LENS_TRUSTFUL_COMPUTE_MINT_ENABLED`) now **defaults false** — an unprotected mint path is opt-in, not on-by-accident.

The **PR2 wash-hardening** then bounds the steady-state yield (verification raised the *entry* bar but not the *steady-state* yield, which a determined operator amortizes):

- **Per-identity rate cap (the universal bound).** A per-workspace rolling **24h** ceiling on **minted LENS across all mint types**, enforced at the same ledger chokepoint (`LENS_MINT_RATE_CAP_LENS_24H`, default **1000** LENS/24h, `0` = off). It sums every mint type together — an attacker can't evade by splitting across tracks — and is exact under concurrency (the SUM rides the balance `FOR UPDATE`). Held mints count at the mint moment; the finalize settlement is **not** double-counted. Conservation moves are never throttled.
- **Card-fingerprint owner-linkage (the cheap bonus).** The Stripe webhook captures a **hash** of the card fingerprint (never the raw value) **best-effort, after the credit commits** — a capture failure can never drop the payment. A pool-royalty mint between two workspaces that share a fingerprint (one operator, one card) is denied; **default-allow on missing** (an absent fingerprint never blocks honest cross-actor reuse). Catches the lazy one-card washer.

**Residual (honest):** a determined operator can still wash **under the rate cap** across **many cards** (rotating cards evades the fingerprint linkage). The rate cap bounds the per-identity yield; deeper owner-linkage (e.g. network/behavioral signals) carries a high privacy cost and is deferred. The verification cost + the rate cap + the cheap linkage together make casual washing unprofitable and bound the determined case.

### Quick start (GPU miner)

```bash
export LENS_URL=https://lens.talyvor.com
export LENS_API_KEY=tlv_...
export LENS_WORKSPACE_ID=your-workspace
export NODE_URL=https://your-server.com
export NODE_PROVIDER=ollama
export NODE_MODELS=llama3.1,mistral
export NODE_GPU_TYPE=rtx4090
./bin/talyvor-node start
```

### Quick start (cache miner)

```bash
export LENS_URL=https://lens.talyvor.com
export LENS_API_KEY=tlv_...
export LENS_WORKSPACE_ID=your-workspace
export CACHE_NODE_URL=https://your-cache.example.com
export CACHE_NODE_REDIS_URL=redis://localhost:6379/0
export CACHE_NODE_MAX_GB=100
./bin/talyvor-cachenode start
```

### Quick start (embedding miner — CPU-friendly)

```bash
export LENS_URL=https://lens.talyvor.com
export LENS_API_KEY=tlv_...
export LENS_WORKSPACE_ID=your-workspace
export EMBED_NODE_URL=https://your-embed.example.com
export EMBED_NODE_MODEL=nomic-embed-text
export EMBED_NODE_DIMENSIONS=768
./bin/talyvor-embednode start
```

### Reading the economy

There are no built-in browser pages for these; the reads are API endpoints:

- `/v1/workspaces/{ws}/tokens/balance`, `.../tokens/mining/*` — balance and mining (authenticated)
- `/v1/economy/stats`, `/v1/tokens/rates` — global supply and rates (any API key)
- `/v1/oracle/stats`, `/v1/insights/routing` — the oracle queue and routing patterns pooled across
  workspaces (admin key only)
- `/v1/economy/conversion-rate` — the LENS/LXC rate (public, no credential)

All of these are present only when the economy is enabled.
