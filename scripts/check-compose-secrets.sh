#!/usr/bin/env bash
#
# check-compose-secrets.sh — fail when a Docker Compose file SUPPLIES a value for a
# secret-shaped environment variable.
#
# WHY THIS EXISTS
#
# talyvor-docs and talyvor-track independently shipped the same defect: a compose file
# carrying `GATEWAY_AUTH_SECRET=${GATEWAY_AUTH_SECRET:-<a real placeholder>}`. A `:-`
# fallback SUPPLIES the value, so `docker compose up` with no operator input starts a
# service whose shared secret is published in a public repo — and any fail-closed check the
# service performs is then satisfied by that value, so it never fires on the one
# configuration that needed it. Docs' placeholder was 42 characters and passed its own
# ">= 16 chars" guard.
#
# Two repos, two separate audits, same shape. A per-repo test cannot catch the next repo, so
# this is written to be COPIED VERBATIM into every repo that ships a compose file. It
# contains no knowledge of any particular repository: no service names, no variable names
# beyond the generic patterns below, no paths.
#
# WHAT IT FLAGS
#
#   - ${NAME:-something}   a non-empty default → the value is supplied. FLAGGED.
#   - NAME=literal         a literal inline value, no interpolation at all. FLAGGED.
#
# WHAT IT DELIBERATELY ALLOWS — each of these supplies nothing:
#
#   - ${NAME:-}            empty default. The variable resolves to "", so the service's own
#                          required-value check still fires. This shape is load-bearing in
#                          talyvor-track (TRACK_LENS_API_KEY and friends) for optional
#                          integrations, and flagging it would make the check unusable there
#                          — which is how a check gets deleted instead of fixed.
#   - ${NAME:?message}     required: compose itself refuses to start, by name. The fix.
#   - ${NAME}              plain passthrough, resolves empty when unset.
#   - NAME=                explicitly blank.
#
# EXCEPTIONS are a DENY-LIST OF NAMED LINES with a written reason, never a path filter. An
# allow-list of directories or files can only ratify what was already reviewed; it cannot
# see the next miss. To exempt a line, put a `# compose-secret-ok: <reason>` comment on it.
#
# USAGE:  scripts/check-compose-secrets.sh [path ...]      (default: repo root, recursive)
# Exit 0 = clean, 1 = findings. No dependencies beyond bash + grep + find.

set -uo pipefail

# Variable-name shapes that denote a credential. Intentionally broad: a false positive costs
# one comment, a false negative cost two repos.
NAME_RE='[A-Za-z0-9_]*(SECRET|TOKEN|KEY|PASSWORD|PASSWD|CREDENTIAL)[A-Za-z0-9_]*'

roots=("$@")
if [ ${#roots[@]} -eq 0 ]; then roots=("."); fi

# Compose files, excluding vendored/dependency trees.
files=$(find "${roots[@]}" \
  \( -name node_modules -o -name .git -o -name vendor -o -name dist \) -prune -o \
  -type f \( -name 'docker-compose*.yml' -o -name 'docker-compose*.yaml' \
             -o -name 'compose*.yml' -o -name 'compose*.yaml' \) -print 2>/dev/null | sort)

if [ -z "$files" ]; then
  echo "check-compose-secrets: no compose files found under ${roots[*]} — nothing to check"
  exit 0
fi

findings=0
scanned=0

for f in $files; do
  scanned=$((scanned + 1))
  lineno=0
  while IFS= read -r line || [ -n "$line" ]; do
    lineno=$((lineno + 1))

    # Skip comments and explicitly exempted lines.
    case "$(printf '%s' "$line" | sed 's/^[[:space:]]*//')" in \#*) continue ;; esac
    if printf '%s' "$line" | grep -qE '#[[:space:]]*compose-secret-ok:'; then continue; fi

    # Shape 1: ${NAME:-value} with a NON-EMPTY value. The `[^}]` requires at least one
    # character after `:-`, so `${NAME:-}` is excluded by construction.
    if printf '%s' "$line" | grep -qE '\$\{'"$NAME_RE"':-[^}]' ; then
      var=$(printf '%s' "$line" | grep -oE '\$\{'"$NAME_RE"':-' | head -1 | sed 's/^\${//; s/:-$//')
      echo "$f:$lineno: ERROR: compose supplies a default for secret-shaped variable '$var'"
      echo "    $(printf '%s' "$line" | sed 's/^[[:space:]]*//')"
      echo "    A \`:-\` default SUPPLIES the value, so the service starts with a committed"
      echo "    secret and its own fail-closed check is satisfied by it. Use \`\${$var:?...}\`"
      echo "    so compose refuses to start, or \`\${$var:-}\` if the variable is optional."
      findings=$((findings + 1))
      continue
    fi

    # Shape 2: a literal inline value — `- NAME=value` or `NAME: value` — with no `${...}`
    # anywhere on the line. Anchored to an env-entry shape so prose and keys don't match.
    if printf '%s' "$line" | grep -qE '^[[:space:]]*(-[[:space:]]*)?'"$NAME_RE"'[[:space:]]*[:=][[:space:]]*[^[:space:]]' &&
       ! printf '%s' "$line" | grep -q '\${'; then
      var=$(printf '%s' "$line" | grep -oE "$NAME_RE"'[[:space:]]*[:=]' | head -1 | sed 's/[[:space:]]*[:=]$//')
      echo "$f:$lineno: ERROR: compose hardcodes a value for secret-shaped variable '$var'"
      echo "    $(printf '%s' "$line" | sed 's/^[[:space:]]*//')"
      echo "    A literal credential in a committed file is published. Read it from the"
      echo "    environment and make it required: \`\${$var:?set $var}\`."
      findings=$((findings + 1))
      continue
    fi
  done < "$f"
done

echo "check-compose-secrets: scanned $scanned compose file(s), $findings finding(s)"
if [ "$findings" -ne 0 ]; then
  echo ""
  echo "To exempt a line that is genuinely safe, append a reason on it:"
  echo "    # compose-secret-ok: <why this value is not a credential>"
  exit 1
fi
exit 0
