#!/usr/bin/env bash
# B28.262: protect main. Run once by a repository admin (the builder bot has no admin rights):
#
#   scripts/protect-main.sh            # creates or updates the "protect-main" ruleset
#   scripts/protect-main.sh --dry-run  # prints the ruleset it would send
#
# After it, main takes changes only through a pull request whose CI checks passed, and cannot be
# force-pushed or deleted. Each required check is a ci.yaml job that runs on every pull request; a
# path-filtered workflow (images.yaml, benchmark.yaml) is never required, because a check that does not
# run never reports and would hold the pull request forever. When ci.yaml gains or renames a job, add it
# to CHECKS and run this again.
set -euo pipefail

REPO="${REPO:-Talyvor/talyvor-lens}"
NAME="protect-main"
GITHUB_ACTIONS_APP_ID=15368

CHECKS=(
  "test (cache_statement)"
  "test (simple_protocol)"
  "tare-model"
  "devnet"
  "lint"
  "sdk"
  "mcp-sdk"
  "vuln"
  "semgrep"
  "compose-secrets"
  "build"
)

checks_json=$(printf '%s\n' "${CHECKS[@]}" |
  jq -R --argjson app "$GITHUB_ACTIONS_APP_ID" '{context: ., integration_id: $app}' | jq -s .)

body=$(jq -n --arg name "$NAME" --argjson checks "$checks_json" '{
  name: $name,
  target: "branch",
  enforcement: "active",
  conditions: {ref_name: {include: ["~DEFAULT_BRANCH"], exclude: []}},
  bypass_actors: [],
  rules: [
    {type: "deletion"},
    {type: "non_fast_forward"},
    {type: "pull_request", parameters: {
      required_approving_review_count: 0,
      dismiss_stale_reviews_on_push: false,
      require_code_owner_review: false,
      require_last_push_approval: false,
      required_review_thread_resolution: false
    }},
    {type: "required_status_checks", parameters: {
      strict_required_status_checks_policy: false,
      required_status_checks: $checks
    }}
  ]
}')

if [[ "${1:-}" == "--dry-run" ]]; then
  echo "$body"
  exit 0
fi

id=$(gh api "repos/$REPO/rulesets" --jq ".[] | select(.name == \"$NAME\") | .id")
if [[ -n "$id" ]]; then
  echo "$body" | gh api -X PUT "repos/$REPO/rulesets/$id" --input - >/dev/null
  echo "updated ruleset $NAME ($id) on $REPO"
else
  echo "$body" | gh api -X POST "repos/$REPO/rulesets" --input - >/dev/null
  echo "created ruleset $NAME on $REPO"
fi
gh api "repos/$REPO/rules/branches/main" --jq '.[].type'
