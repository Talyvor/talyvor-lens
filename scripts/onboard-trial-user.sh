#!/usr/bin/env bash
# onboard-trial-user.sh — onboard ONE trial user end to end, in one invocation:
#
#     create workspace  →  mint proxy-scoped key  →  grant LXC
#
# the three admin acts of docs/remote-host.md §5. A fresh workspace has 0 LXC, so
# without the third act the minted key's very first proxy request is rejected 402
# by the agent allocator (docs/local-standup-runbook.md, Trap 7).
#
# Usage:
#     LENS_API_KEY=…  scripts/onboard-trial-user.sh <workspace>
#
#     <workspace>      workspace id AND display name (lowercase letters, digits,
#                      '-' and '_'; must not already exist — see the guard below).
#
#     LENS_API_KEY     the admin god-key — must equal the value temporarily armed
#                      in the server's .env (docs/remote-host.md §4/§5). Read it
#                      with `read -r -s LENS_API_KEY && export LENS_API_KEY` so it
#                      never lands in shell history. This script never prints it.
#     TALYVOR_OPERATOR who you are, for the audit row: the LXC grant is behind
#                      step-up (B28.264), so the script asks for the current
#                      code from your authenticator app just before it grants.
#     LENS_ADMIN_URL   admin base URL. Default http://127.0.0.1:8080 — the VM
#                      loopback, reached over `ssh -N -L 8080:127.0.0.1:8080 …`
#                      (admin never goes through the public door).
#
# Server prerequisite: LENS_ADMIN_LXC_GRANT_ENABLED=true at lens boot — without it
# the grant route is never registered (bare 404). Preflighted below, side-effect
# free, BEFORE anything is created.
#
# Output: progress and next steps on stderr. stdout carries EXACTLY ONE line —
# the minted tlv_ws_ key. It is shown once and never again (the server stores
# only a hash); copy it into a password manager, never chat.

set -Eeuo pipefail

trap 'printf "onboard-trial-user.sh: unexpected failure at line %s\n" "$LINENO" >&2' ERR

log() { printf '%s\n' "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

usage() {
  # The header comment above is the manual; print the invocation line only.
  printf 'usage: LENS_API_KEY=… %s <workspace>\n' "$0" >&2
  exit "${1:-1}"
}

# ── Grant size ────────────────────────────────────────────────────────────────
# 50 LXC = 50,000,000 µLXC — deliberately EQUAL to DefaultAgentCeilingLXC
# (internal/economy/agent_subbudget.go), the per-scoped-key agent sub-budget
# ceiling (50 LXC = $5 at the 1 LXC = $0.10 peg). The allocator caps a key's
# lifetime metered spend at its ceiling (spent_lxc is monotonic — it never
# resets) and there is no HTTP route to raise a ceiling (SetAgentCeiling is
# unexposed), so on this one-key trial workspace the ceiling binds before any
# larger grant would: every µLXC above 50 LXC would be stranded balance the key
# can never spend. Bigger buys nothing; this funds exactly one key's ceiling.
readonly GRANT_ULXC=50000000

# ── Arguments + environment ───────────────────────────────────────────────────
[[ $# -eq 1 ]] || usage
case $1 in -h|--help) usage 0 ;; esac

# God-key check EARLY — before workspace validation or any network use, so a
# keyless invocation refuses with the ritual spelled out rather than a terse
# mid-flight failure. (Deliberately after -h|--help: help works keyless.)
if [[ -z "${LENS_API_KEY:-}" ]]; then
  cat >&2 <<'EOF'
ERROR: LENS_API_KEY is not set (or is empty) — refusing before touching anything.

This script performs three ADMIN acts and needs the god-key temporarily armed in
the server .env (docs/remote-host.md §5, "The full ritual"):
  on the VM :  add LENS_API_KEY=… and LENS_ADMIN_LXC_GRANT_ENABLED=true to .env,
               then recreate:  docker compose up -d lens
  tunnel    :  ssh -N -L 8080:127.0.0.1:8080 user@<host>
  here      :  read -r -s LENS_API_KEY && export LENS_API_KEY   # stays out of history
EOF
  exit 1
fi

[[ -n "${TALYVOR_OPERATOR:-}" ]] \
  || die "TALYVOR_OPERATOR is not set — the LXC grant names the operator who takes it (B28.264)"

WS=$1
[[ $WS =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] \
  || die "workspace '$WS' — use lowercase letters, digits, '-' or '_' (max 63 chars)"
readonly WS
readonly KEY_NAME="trial"

LENS_ADMIN_URL=${LENS_ADMIN_URL:-http://127.0.0.1:8080}
LENS_ADMIN_URL=${LENS_ADMIN_URL%/}
readonly LENS_ADMIN_URL

for cmd in curl jq; do
  command -v "$cmd" >/dev/null 2>&1 || die "missing dependency: $cmd"
done

# ── HTTP helper ───────────────────────────────────────────────────────────────
# http METHOD PATH [JSON_BODY] → sets HTTP_STATUS + HTTP_BODY. Transport failure
# dies (tunnel down); HTTP status is the caller's to branch on. NEVER echoes the
# admin key; success bodies are parsed with jq, not printed (the mint response
# contains the raw workspace key).
# STEP_UP_CODE, when set, goes with the request as the step-up code.
HTTP_STATUS='' HTTP_BODY='' STEP_UP_CODE=''
http() {
  local method=$1 path=$2 body=${3:-} out
  local -a args=(-sS --max-time 30 -X "$method" "$LENS_ADMIN_URL$path"
    -H "Authorization: Bearer $LENS_API_KEY"
    -H 'Content-Type: application/json'
    -H "X-Talyvor-Operator: $TALYVOR_OPERATOR"
    -w $'\n%{http_code}')
  [[ -n $body ]] && args+=(-d "$body")
  [[ -n $STEP_UP_CODE ]] && args+=(-H "X-Talyvor-Step-Up: $STEP_UP_CODE")
  out=$(curl "${args[@]}") \
    || die "cannot reach $LENS_ADMIN_URL ($method $path) — is the SSH tunnel to the VM loopback up?"
  HTTP_STATUS=${out##*$'\n'}
  HTTP_BODY=${out%$'\n'*}
}

# ── Preflights (all side-effect free, so a failed run creates NOTHING) ────────
http GET /healthz
[[ $HTTP_STATUS == 200 ]] || die "lens at $LENS_ADMIN_URL is unhealthy (HTTP $HTTP_STATUS on /healthz)"

# Probe the grant route with no step-up code: 403 ("step-up required") proves the
# route is registered AND the admin key works — nothing is granted, no code used.
http POST /v1/admin/lxc/grant '{}'
case $HTTP_STATUS in
  403) ;; # route present, admin accepted, step-up asked for — the expected probe outcome
  503) die "the grant route answers 503: $HTTP_BODY" ;;
  404) die "grant route absent — set LENS_ADMIN_LXC_GRANT_ENABLED=true in the server's .env and 'docker compose up -d lens' (docs/remote-host.md §5)" ;;
  401) die "admin rejected (401) — LENS_API_KEY here must equal the value currently armed in the server's .env (unset there ⇒ admin fails closed; docs/remote-host.md §4)" ;;
  *)   die "unexpected HTTP $HTTP_STATUS probing the grant route: $HTTP_BODY" ;;
esac

# Re-run guard: POST /v1/workspaces is an UPSERT, so re-running these acts
# against an existing workspace would silently double-fund it with a second
# grant. One workspace per trial user, one run per workspace — refuse otherwise.
http GET "/v1/workspaces/$WS"
case $HTTP_STATUS in
  404) ;; # fresh id — proceed
  200) die "workspace '$WS' already exists — refusing to re-run (a second grant would double-fund it). Pick a fresh workspace id." ;;
  *)   die "unexpected HTTP $HTTP_STATUS checking workspace '$WS': $HTTP_BODY" ;;
esac

# ── Act 1: create the workspace ───────────────────────────────────────────────
http POST /v1/workspaces "$(jq -cn --arg id "$WS" '{id:$id, name:$id}')"
[[ $HTTP_STATUS == 201 ]] || die "workspace create failed (HTTP $HTTP_STATUS): $HTTP_BODY"
log "✔ workspace '$WS' created"

# ── Act 2: mint the proxy-scoped key ──────────────────────────────────────────
http POST "/v1/workspaces/$WS/api-keys" \
  "$(jq -cn --arg name "$KEY_NAME" '{name:$name, scopes:["proxy"]}')"
[[ $HTTP_STATUS == 201 ]] || die "key mint failed (HTTP $HTTP_STATUS): $HTTP_BODY"
KEY=$(jq -er '.key' <<<"$HTTP_BODY") \
  || die "key mint returned HTTP 201 but no .key field — response not printed (it may carry the credential)"
KEY_ID=$(jq -er '.id' <<<"$HTTP_BODY") || KEY_ID='?'
KEY_PREFIX=$(jq -er '.prefix' <<<"$HTTP_BODY") || KEY_PREFIX='?'
log "✔ key '$KEY_NAME' minted (id $KEY_ID, prefix $KEY_PREFIX, scopes [proxy])"

# ── Act 3: grant LXC ──────────────────────────────────────────────────────────
read -r -p "step-up code from your authenticator app: " STEP_UP_CODE </dev/tty
http POST /v1/admin/lxc/grant "$(jq -cn --arg ws "$WS" --argjson amt "$GRANT_ULXC" \
  '{workspace_id:$ws, amount_ulxc:$amt, reason:"trial onboarding"}')"
[[ $HTTP_STATUS == 200 ]] || die "LXC grant failed (HTTP $HTTP_STATUS): $HTTP_BODY — workspace '$WS' and key $KEY_ID exist but are UNFUNDED (first request will 402); fix the cause and grant by hand (docs/remote-host.md §5), do NOT re-run this script"
NEW_BAL=$(jq -er '.new_balance_ulxc' <<<"$HTTP_BODY") || NEW_BAL='?'
log "✔ granted $GRANT_ULXC µLXC (50 LXC) — new balance: $NEW_BAL µLXC"

# ── Done ──────────────────────────────────────────────────────────────────────
log ""
log "Onboarded '$WS'. Next:"
log "  • hand the key below to the trial user via a password manager (never chat);"
log "    they use it as a bearer token against https://<LENS_DOMAIN>/v1/proxy/…"
log "  • disarm admin on the VM: remove LENS_API_KEY (and, when onboarding is done,"
log "    LENS_ADMIN_LXC_GRANT_ENABLED) from .env, then 'docker compose up -d lens'"
log ""
log "The key (shown ONCE, stdout's only line):"
printf '%s\n' "$KEY"
