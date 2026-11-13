#!/usr/bin/env bash
# Provision the scenario workspaces for the routing-pattern earning trial.
# Mints a proxy-scoped tlv_ key per workspace on the workspace's own key route
# (a distinct workspace_id per scenario: "default" is excluded from earn) and
# opts in the ones that should earn. The admin key drives both (admin bypasses
# workspace isolation).
#
#   Run from the repo root:  LENS_API_KEY=... bash tools/trial/seed.sh
#
# Writes  tools/trial/keys.tsv  as  <workspace_id>\t<tlv_key>  for traffic.sh
# (gitignored).
set -euo pipefail
BASE=${BASE:-http://localhost:8080}
ADMIN=${LENS_API_KEY:?set LENS_API_KEY (must match the value in .env)}
OUT=${OUT:-tools/trial/keys.tsv}
: > "$OUT"

mintkey() { # <workspace_id> -> tlv_ key, appended to keys.tsv
  local ws="$1" key
  # POST /v1/api/keys is retired (410, B50.33): it minted scopeless keys, which pass no scope gate.
  key=$(curl -fsS -X POST "$BASE/v1/workspaces/$ws/api-keys" \
    -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
    -d '{"name":"trial","scopes":["proxy"]}' \
    | python3 -c 'import sys,json;print(json.load(sys.stdin)["key"])')
  printf '%s\t%s\n' "$ws" "$key" >> "$OUT"
  echo "$key"
}

optin() { # <workspace_id>  (admin-driven)
  curl -fsS -X POST "$BASE/v1/workspaces/$1/pattern-mining/opt-in" \
    -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' >/dev/null
  echo "  opted-in: $1"
}

register() { # <workspace_id>  (needed before SetLoggingPolicy / SetCachePoolable)
  # A minimal {id,name} body is sufficient since #128/#138 — the server defaults
  # allowed_models/allowed_providers to empty (allow-all) and active to true.
  # Tolerant of re-runs (an already-registered workspace is fine).
  if curl -fsS -X POST "$BASE/v1/workspaces" \
    -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
    -d "{\"id\":\"$1\",\"name\":\"$1\"}" >/dev/null 2>&1; then
    echo "  registered: $1"
  else
    echo "  registered: $1 (already existed / ok)"
  fi
}

setlogging() { # <workspace_id> <policy>
  curl -fsS -X PUT "$BASE/v1/workspaces/$1/logging" \
    -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
    -d "{\"logging_policy\":\"$2\"}" >/dev/null
  echo "  logging=$2: $1"
}

# --- PR 2a helpers (semantic-isolation + JWT scenarios) ---

cachepoolable() { # <workspace_id>  (admin-driven; opt into the pooled cache, scenario m)
  curl -fsS -X PUT "$BASE/v1/workspaces/$1/cache-poolable" \
    -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
    -d '{"cache_poolable":true}' >/dev/null
  echo "  cache_poolable=true: $1"
}

jwtmint() { # <workspace_id> -> mints a workspace JWT via the admin /v1/auth/token
  # route; appends "<ws>-jwt\t<jwt>" to keys.tsv for `traffic.sh send-jwt`.
  # Needs LENS_JWT_PRIVATE_KEY in the lens env; if unset the route 503s and we
  # SKIP (scenario r is then untested, exactly like the legacy (h) gap).
  local ws="$1" tok
  tok=$(curl -fsS -X POST "$BASE/v1/auth/token" \
    -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
    -d "{\"workspace_id\":\"$ws\",\"scopes\":[\"proxy\"]}" 2>/dev/null \
    | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])' 2>/dev/null) || true
  if [ -n "${tok:-}" ]; then
    printf '%s\t%s\n' "${ws}-jwt" "$tok" >> "$OUT"
    echo "  jwt minted: $ws (-> ${ws}-jwt in keys.tsv)"
  else
    echo "  jwt SKIPPED for $ws (LENS_JWT_PRIVATE_KEY unset?) — scenario r untested"
  fi
}

# --- PR 2b helpers (DISTILL economy scenarios n/o/p) ---

distillpolicy() { # <workspace_id> — DistillPolicy=always so any document distills (no header)
  curl -fsS -X PUT "$BASE/v1/workspaces/$1/distill" \
    -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
    -d '{"distill_policy":"always"}' >/dev/null
  echo "  distill_policy=always: $1"
}

distillpoolable() { # <workspace_id> <true|false> — cross-tenant distill-share consent
  curl -fsS -X PUT "$BASE/v1/workspaces/$1/distill-poolable" \
    -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
    -d "{\"distill_poolable\":${2:-true}}" >/dev/null
  echo "  distill_poolable=${2:-true}: $1"
}

echo "== scenario (a) base =="
mintkey ws-base       >/dev/null; optin ws-base

echo "== scenario (b) corroboration (4 opted-in) =="
for i in 1 2 3 4; do mintkey "ws-corr-$i" >/dev/null; optin "ws-corr-$i"; done

echo "== scenario (c) claim =="
mintkey ws-claim      >/dev/null; optin ws-claim

echo "== scenario (d) cap =="
mintkey ws-cap        >/dev/null; optin ws-cap

echo "== scenario (e1) non-opted-in control (NO opt-in) =="
mintkey ws-noopt      >/dev/null

echo "== scenario (e2) LoggingNone (registered + none + opted-in) =="
mintkey ws-none       >/dev/null
register ws-none
setlogging ws-none none
optin ws-none

# --- PR 2a: semantic-isolation + authz + JWT workspaces ---
# These need NO pattern opt-in (they exercise the cache/authz/jwt paths, not earn).

echo "== scenarios (k)(l) private semantic isolation — NO cache_poolable (private rows only) =="
mintkey ws-sem-a      >/dev/null   # owner: caches a private TLVCOLLIDE row
mintkey ws-sem-b      >/dev/null   # requester: must MISS ws-sem-a's row (#142 filter)

echo "== scenario (m) pooled control — BOTH opt into cache_poolable =="
# cache-poolable consent lives in the workspace manager, so register first
# (same prerequisite as setlogging).
mintkey ws-pool-a     >/dev/null; register ws-pool-a; cachepoolable ws-pool-a  # contributes a poolable row
mintkey ws-pool-b     >/dev/null; register ws-pool-b; cachepoolable ws-pool-b  # may receive it cross-tenant

echo "== scenario (q) authz smoke — two unrelated tenants =="
mintkey ws-authz-a    >/dev/null   # will try to read ws-authz-b's object (-> 404)
mintkey ws-authz-b    >/dev/null   # owns the object

echo "== scenario (r) JWT fallback (h-closure) — mint a workspace JWT =="
jwtmint ws-jwt                     # writes ws-jwt-jwt -> keys.tsv (skips if no signing key)

# --- PR 2b: DISTILL economy workspaces (n/o/p). Needs the trial-distill overlay
# (LENS_DISTILL_POOLABLE_ENABLED). distill_policy=always so docs distill; the
# (n) matrix toggles distill_poolable per leg. ws-distill-c is the owner-off leg.
echo "== scenarios (n)(o)(p) distill economy — a=owner, b=requester (both consented) =="
mintkey ws-distill-a  >/dev/null; register ws-distill-a; distillpolicy ws-distill-a; distillpoolable ws-distill-a true
mintkey ws-distill-b  >/dev/null; register ws-distill-b; distillpolicy ws-distill-b; distillpoolable ws-distill-b true
echo "== (n) owner-off leg — ws-distill-c distills but does NOT share (distill_poolable=false) =="
mintkey ws-distill-c  >/dev/null; register ws-distill-c; distillpolicy ws-distill-c; distillpoolable ws-distill-c false

echo
echo "keys -> $OUT"
cat "$OUT"
