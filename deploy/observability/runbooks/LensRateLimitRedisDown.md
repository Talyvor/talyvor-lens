# Runbook: LensRateLimitRedisDown

**Alert:** the rate limiter's Redis calls failed in the last 5m, sustained for 2m. **Severity:** warning.

> Starting point, not exhaustive.

## Symptom
`lens_rate_limit_redis_errors_total` is rising. Each Lens instance is counting rate limits in its own memory, so limits still hold and callers past them still get 429, but a caller can get up to one limit per instance.

## Likely causes
- Redis is down, restarting or out of memory.
- A network or DNS problem between Lens and Redis.
- `LENS_REDIS_URL` points at the wrong host after a config change.

## First diagnostics
```sh
# How fast are the calls failing?
sum(rate(lens_rate_limit_redis_errors_total[5m]))
# What does Lens see?
kubectl -n <ns> logs deploy/<release>-lens --tail=200 | grep "ratelimit: redis script failed"
```

## Mitigation
- Restore Redis; the limiter goes back to Redis on the next request, with no restart.
- If Redis will be down a while and you run many instances, watch `LensRateLimitSpike` and the upstream spend: the per-instance limits let more through in total.
