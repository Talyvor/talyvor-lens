# Runbook: LensHighErrorRate

**Alert:** 5xx error rate > 1% for 5m. **Severity:** critical.

> Starting point, not exhaustive — adapt to what you see.

## Symptom
A meaningful fraction of requests return 5xx. Users see failures / degraded AI features.

## Likely causes
- An upstream provider is failing (check `LensProviderDown`, provider error panels).
- A dependency is down: Postgres, Redis, or NATS unreachable.
- A bad deploy (error rate jumped right after a rollout).
- Resource exhaustion (OOM/CPU throttling) — pods restarting.

## First diagnostics
```sh
# Which routes + statuses?
sum by (route,status) (rate(lens_http_requests_total{status=~"5.."}[5m]))
# Recent restarts / OOM?
kubectl -n <ns> get pods -l app.kubernetes.io/name=lens
kubectl -n <ns> logs deploy/<release>-lens --tail=200 | grep -iE "error|panic|refused"
# Dependency health:
kubectl -n <ns> exec deploy/<release>-lens -- wget -qO- localhost:8080/readyz
```

## Mitigation
- Bad deploy → `helm rollback <release>` (or `kubectl rollout undo`).
- Dependency down → restore Postgres/Redis/NATS; `/readyz` will recover.
  While Postgres is down, which requests are refused, served, and served unbilled is in
  [docs/database-outage.md](../../../docs/database-outage.md).
- Single provider → rely on breaker/fallback; consider disabling that provider.
- Capacity → scale replicas (HA) or raise resource limits.
