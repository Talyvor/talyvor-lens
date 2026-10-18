# Talyvor Lens — Helm chart

The reference Helm chart for deploying the **Lens AI gateway** to Kubernetes.
Track/Docs/Code charts follow this same structure.

> ### ⚠ NOT YET TESTED ON A LIVE CLUSTER
> This chart is validated by **static checks only** — `helm lint`,
> `helm template` (default + HA overrides), and `kubeconform` schema
> validation. It **lints clean, templates without errors, and is schema-valid**.
> It has **not** been `helm install`-ed against a real cluster. Running it on a
> kind/minikube/staging cluster and confirming the pods come up healthy is a
> separate manual step the operator must do before production use.

## What you get by default

A minimal, production-shaped install:

- **one** gateway `Deployment` replica (HA is opt-in),
- a `Service` (ClusterIP), `ConfigMap` (non-secret env), `ServiceAccount`,
- a **pre-install/pre-upgrade migration hook** `Job`,
- secrets supplied **by reference** (the chart creates none),
- external Postgres / Redis / NATS (the chart bundles no databases).

Everything else — HA, autoscaling, PodDisruptionBudget, in-cluster mining
nodes, embedded dev databases, chart-created secrets — is **opt-in, off by
default**.

## Prerequisites

- Kubernetes 1.23+ and Helm 3.8+ (or Helm 4).
- A reachable **Postgres**, **Redis**, and **NATS** (Lens requires all three).
- A pullable gateway image (`ghcr.io/talyvor/talyvor-lens:latest` by default).
  ⚠ `latest` is not reproducible — pin a commit sha for production, see `values.yaml`.
- A **Secret** containing the required env (see [Secrets](#secrets)).
- The image must expose the HA endpoints **`/livez`** and **`/readyz`** (added
  by the Lens HA upgrade) — the probes target them.

## Install / upgrade / uninstall

```sh
# 1. Create the required Secret (see Secrets below), e.g.:
kubectl create secret generic lens-env \
  --from-literal=LENS_DATABASE_URL='postgres://…' \
  --from-literal=LENS_REDIS_URL='redis://…' \
  --from-literal=LENS_NATS_URL='nats://…' \
  --from-literal=LENS_OPENAI_API_KEY='sk-…' \
  --from-literal=LENS_ANTHROPIC_API_KEY='sk-ant-…'

# 2. Install (point at your Secret).
helm install lens deploy/helm/lens \
  --set secret.existingSecret=lens-env

# Upgrade
helm upgrade lens deploy/helm/lens --set secret.existingSecret=lens-env

# Uninstall
helm uninstall lens
```

HA example (renders 3 replicas + HPA + PDB + in-cluster nodes):

```sh
helm template lens deploy/helm/lens -f deploy/helm/lens/examples/values-ha.yaml
```

## Values

| Key | Default | Description |
|-----|---------|-------------|
| `replicaCount` | `1` | Gateway replicas. Keep at 1 unless `config.ha.enabled`. |
| `image.repository` | `ghcr.io/talyvor/talyvor-lens` | Gateway image. Published by `.github/workflows/images.yaml`. |
| `image.tag` | `latest` | ⚠ Not reproducible. Pin a 40-char commit sha for production; no semver tag is ever published. |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | Private-registry pull secrets. |
| `containerPort` | `8080` | Gateway listen port (match `config.listenAddr`). |
| `service.type` / `service.port` | `ClusterIP` / `8080` | Gateway Service. |
| `resources` | 100m/128Mi → 1/512Mi | Requests/limits — tune to traffic. |
| `podSecurityContext` | non-root uid 65532, RuntimeDefault seccomp | Pod security. |
| `securityContext` | readOnlyRootFS, drop ALL caps, no priv-esc | Container security. |
| `probes.liveness.path` | `/livez` | Liveness endpoint (HA). |
| `probes.readiness.path` | `/readyz` | Readiness endpoint (HA, drain-aware). |
| `config.listenAddr` | `0.0.0.0:8080` | `LENS_LISTEN_ADDR`. |
| `config.logLevel` | `info` | `LENS_LOG_LEVEL`. |
| `config.ha.enabled` | `false` | `LENS_HA_ENABLED`. Required before scaling >1. |
| `config.ha.{heartbeatSec,instanceTtlSec,drainTimeoutSec}` | 5/15/30 | HA timers. |
| `config.extraEnv` | `{}` | Extra **non-secret** `LENS_*` env. |
| `secret.existingSecret` | `""` | **Recommended.** Name of a pre-created Secret. |
| `secret.create` | `false` | **Dev only.** Create a Secret from `secret.data`. |
| `secret.data` | `{}` | **Dev only.** Never commit real secrets. |
| `postgres.embedded` / `redis.embedded` | `false` | Dev-only embedded DBs (see below). |
| `migrations.enabled` | `true` | Run the migration hook Job (defaults to the image's `lens migrate`). |
| `migrations.image` / `command` / `args` | `""` / `[]` / `[]` | Override the migration mechanism (see below). |
| `backup.enabled` | `false` | Scheduled-backup CronJob (opt-in; see below). |
| `backup.schedule` | `0 2 * * *` | Cron schedule (UTC) for the backup. |
| `backup.scriptConfigMap` | `""` | ConfigMap with `pg_backup.sh` (default `<release>-lens-backup-scripts`). |
| `backup.image` / `retention.*` / `env` / `storage` | see `values.yaml` | Backup image, retention, S3 env, volume. |
| `autoscaling.enabled` | `false` | HPA (only with HA). |
| `autoscaling.{min,max}Replicas` | 2 / 10 | |
| `autoscaling.target{CPU,Memory}UtilizationPercentage` | 70 / 80 | |
| `podDisruptionBudget.enabled` | `false` | PDB for the HA case. |
| `nodes.enabled` | `false` | Master switch for in-cluster mining nodes. |
| `nodes.{node,cachenode,embednode}.*` | see `values.yaml` | Per-node-type workload config. |

Full, commented defaults live in [`values.yaml`](./values.yaml); bad values are
caught at lint time by [`values.schema.json`](./values.schema.json).

## Secrets

The gateway reads these env via `envFrom` (a ConfigMap for non-secret values +
a Secret for the rest). The Secret must contain at least:

```
LENS_DATABASE_URL   LENS_REDIS_URL   LENS_NATS_URL
LENS_OPENAI_API_KEY   LENS_ANTHROPIC_API_KEY
```

…and optionally `LENS_JWT_SECRET`, `LENS_GOOGLE_API_KEY`, and other provider
keys.

**The chart creates no secrets by default.** Three supported patterns:

1. **By reference (recommended):** create the Secret yourself and set
   `secret.existingSecret=<name>`.
2. **External secret managers (recommended for prod):** use the
   [External Secrets Operator](https://external-secrets.io/) or
   [Sealed Secrets](https://sealed-secrets.netlify.app/) to materialise a
   Secret, then reference it via `secret.existingSecret`. (`serviceAccount.annotations`
   is the place for IRSA / Workload Identity if your secret store needs it.)
3. **Dev/test only:** `secret.create=true` + `secret.data={…}` renders a Secret
   from values. **Never commit real secrets to values.**

If neither `existingSecret` nor `create` is set, the gateway expects a Secret
named `<release>-lens-env` to exist — create it or the pods won't start.

## Embedded databases (dev only)

Production deployments point at **external** Postgres/Redis/NATS. For local
dev you may want in-cluster Bitnami subcharts. They are **intentionally not
declared** in `Chart.yaml` (that would force `helm dependency build` — a
network fetch — on every lint/template). To use them, add this to `Chart.yaml`
and run `helm dependency build`:

```yaml
dependencies:
  - name: postgresql
    version: "~15"
    repository: https://charts.bitnami.com/bitnami
    condition: postgres.embedded
  - name: redis
    version: "~19"
    repository: https://charts.bitnami.com/bitnami
    condition: redis.embedded
```

then set `postgres.embedded=true` / `redis.embedded=true` and point
`LENS_DATABASE_URL` / `LENS_REDIS_URL` at the subchart Services. **Not for
production.**

## Migrations

`migrations.enabled=true` installs a **pre-install/pre-upgrade hook Job** that
runs **before** new gateway pods roll out (hook weight `-5`). If the Job fails
(non-zero exit), Helm fails the install/upgrade — so a server is never rolled
out against an unmigrated DB.

By default the hook runs the gateway image's built-in **`lens migrate`**
subcommand, which applies the embedded SQL migrations idempotently (tracked in
a `schema_migrations` table) using the same `LENS_DATABASE_URL` the server
reads. No separate migrations image or tool is required. Alternatives:

- override `migrations.command` to use a different mechanism (a dedicated
  migrations image, `migrate`/Flyway, …) and optionally `migrations.image`, or
- set `migrations.enabled=false` and run migrations out of band (the
  `lens migrate` subcommand still works if you invoke it yourself).

### Migrations must connect direct to Postgres (PgBouncer)

Migrations run **DDL** and must connect **directly to Postgres** — never through
a transaction-mode pooler. PgBouncer in transaction mode rejects the extended /
prepared-statement protocol and cannot safely run multi-statement DDL
(`BEGIN/COMMIT`, partitioning). So when **`pgbouncer.enabled=true`** (which
points the app's `LENS_DATABASE_URL` at the pooler), **set
`migrations.databaseURL` to the DIRECT Postgres URL**:

```yaml
pgbouncer:
  enabled: true            # app connects through the pooler
migrations:
  databaseURL: "postgres://lens:<pass>@<postgres-host>:5432/talyvor_lens"
```

`migrations.databaseURL` overrides `LENS_DATABASE_URL` (and disables the
PgBouncer flag) **for the migration Job only**; the gateway Deployment keeps
using the pooled connection. Leave `migrations.databaseURL` unset when PgBouncer
is not in front (the Job then uses the Secret's `LENS_DATABASE_URL` as before).

## Scheduled backups

`backup.enabled=true` installs a **CronJob** that runs the canonical
`deploy/backup/scripts/pg_backup.sh` on `backup.schedule` (default daily
02:00 UTC). **Disabled by default** — backups need a real destination and
credentials, so the chart won't run a CronJob against nothing.

The chart **references** the backup script rather than vendoring a copy: mount
it from a ConfigMap you create from the canonical script (single source of
truth), e.g.

```sh
kubectl create configmap <release>-lens-backup-scripts \
  --from-file=deploy/backup/scripts/pg_backup.sh
```

(the default ConfigMap name is `<release>-lens-backup-scripts`; override with
`backup.scriptConfigMap`). The CronJob uses `postgres:16` (has `pg_dump`/`gzip`;
matches the DR runbook + restore drill), maps the Secret's `LENS_DATABASE_URL`
into the `DATABASE_URL` the script reads, and honours `backup.retention.*`. Set
`backup.env` for S3 upload (`BACKUP_S3_BUCKET`/`BACKUP_S3_ENDPOINT`/
`BACKUP_S3_PREFIX`) and `backup.storage` for a durable `BACKUP_DIR` volume (the
default is an ephemeral `emptyDir`, intended to be paired with S3 upload). See
`deploy/backup/DR-RUNBOOK.md`.

## High availability

Set `config.ha.enabled=true` (turns on `LENS_HA_ENABLED`, which the gateway
needs before it's safe to run multiple replicas), then raise `replicaCount` or
enable `autoscaling`/`podDisruptionBudget`. See `examples/values-ha.yaml`.

### Cross-node policy consistency

Policy changes propagate across replicas by **TTL / periodic refresh / restart — not a
real-time bus.** There is no policy-invalidation pub/sub (only the circuit-breaker and
node-liveness channels are bus-backed). Current per-surface windows:

| Change | Cross-replica visibility |
|---|---|
| Token budget (`max_tokens`) | immediate — read from Postgres per request |
| Spend-cap hard-block | ≈60 s (spend-cap refresh interval) |
| API-key revocation | ≈5 min (key-cache TTL) — a key revoked on one replica may still be honored on another replica for up to ~5 minutes until that node's cache entry expires |
| Workspace config (logging policy, cache-pooling flags) | ≤ `LENS_WORKSPACE_RELOAD_INTERVAL` (default 30s) — each replica rebuilds its workspace cache from Postgres on this interval |
| Guardrail custom policies | ≤ `LENS_GUARDRAILS_RELOAD_INTERVAL` (default 30s) — persisted to Postgres (`guardrail_policies`), restart-surviving; each replica rebuilds its policy cache on this interval. The PII-redact + injection-block default stays always-on. |

Workspace-config and guardrail-policy propagation are both **test-proven** with two-instance-over-one-pool reload tests (a change on instance A becomes visible on instance B after its reload; a deletion reverts to default rather than lingering). The remaining windows (spend-cap ≈60 s, key-revocation ≈5 min) reflect current refresh/TTL constants and are **not yet enforced by tests**. Plan multi-replica rollouts accordingly.

These windows assume a reachable database. What Lens does while Postgres is down — how long a revoked key keeps working, and whether workspace and agent requests are served and billed — is in [docs/database-outage.md](../../../docs/database-outage.md).

**Guardrail fail-safe (security control).** Guardrails are a security control, so the load path fails *safe*, never *open*. If custom policies cannot be loaded at **startup** (DB unreachable or a corrupt row), the engine serves the locked-down default — PII redact + injection block stay ON for every workspace — and raises a loud `Degraded()` + ERROR, so the downgrade is visible, never silent. A **reload** failure *after* a prior good load retains the last-good policies (build-then-swap never publishes a partial map) and stays non-degraded, so a transient DB blip can never loosen a stricter-than-default workspace. A single corrupt row fails the whole rebuild rather than skipping just that row — skipping would silently downgrade that one tenant to the (possibly looser) default.

### Read scaling — optional read-replica (U8/U9)

Set **`LENS_DB_REPLICA_URL`** to a read-replica DSN to offload a small, explicitly
vetted set of **analytics/display reads** (spend forecasting, cost-anomaly
detection, the admin distill-attribution view) to a replica. **Off by default** —
unset, every read uses the primary, byte-identical to single-pool behaviour. A
malformed DSN or an unreachable replica at boot **falls back to the primary with
a WARN**, never a crash (the replica is an optimization, never a boot
dependency). When `LENS_DB_PGBOUNCER` is set, the replica pool uses simple
protocol too.

**The invariant (enforced by wiring, not vigilance): no money/authz read that
feeds a write decision is ever routed to a replica.** API-key validation,
budget/spend enforcement, the LXC balance gate, Stripe credit, workspace config,
and every read inside a write transaction stay on the **primary** by
construction — those structs are never handed the replica pool. Only four
read-only-by-construction structs receive it; a CI guard fails the build if a
money/authz constructor is ever wired to the replica, or if one of the four
gains a write method. A lagging replica therefore cannot cause a revoked key to
be honoured or a payment to be mis-credited.

Replica replay lag is exported as the Prometheus gauge
**`lens_db_replica_lag_seconds`** (0 when no replica is configured) and surfaced
on **`/healthz`** as the `read_replica` check (a healthy no-op when unconfigured).

> **CI caveat (same honesty as the U7 staleness note):** the integration suite
> runs a single Postgres, so it proves the **routing decision** (analytics→replica,
> money/authz→primary) via two pools over one DB. Routing is a static config-time
> choice and **lag gates nothing** — it is observability-only (the gauge + health
> detail above), so there is no lag-threshold/fallback path to cover. The only thing
> a single Postgres can't exercise is the **non-zero lag measurement** (the standby
> `pg_is_in_recovery()` branch); that is a monitoring-fidelity limitation, **not** a
> correctness gap — a wrong lag reading can only mislabel a metric, never cause a
> misroute. A streaming-standby CI is not warranted.

## In-cluster mining nodes (advanced, optional)

`nodes.enabled=true` renders optional `Deployment`/`DaemonSet` workloads for the
`talyvor-node` / `-cachenode` / `-embednode` mining binaries. These are **not**
part of a standard gateway deploy and each needs its **own published image**
(the default gateway image contains only the gateway binary). Off by default.

## Metrics & scraping

The gateway always serves Prometheus metrics at **`/metrics`** (port 8080). Two
ways to scrape:

- **Prometheus Operator:** set `metrics.serviceMonitor.enabled=true` to render a
  `ServiceMonitor` (`monitoring.coreos.com/v1`). Off by default so the chart
  installs on clusters without the Operator CRDs. Add `metrics.serviceMonitor.labels`
  (e.g. `{ release: kube-prometheus-stack }`) so your Prometheus selects it:

  ```sh
  helm install lens deploy/helm/lens \
    --set metrics.serviceMonitor.enabled=true \
    --set metrics.serviceMonitor.labels.release=kube-prometheus-stack
  ```

- **Plain Prometheus (no Operator):** use the example scrape config at
  `deploy/observability/prometheus/scrape-config.example.yaml`.

Alerting + recording rules and Grafana dashboards live in
[`deploy/observability/`](../../observability/).

## Raw manifests (no Helm)

[`deploy/k8s/`](../../k8s/) holds plain YAML (a kustomize base) rendered from
this chart with default values, for operators who don't use Helm. **Helm is the
source of truth** — regenerate with `make k8s-manifests`, don't hand-edit.

## Validation

```sh
make helm-lint        # helm lint
make helm-template    # render default + HA override
make helm-validate    # render | kubeconform (schema validation)
make k8s-manifests    # regenerate deploy/k8s from the chart
```
