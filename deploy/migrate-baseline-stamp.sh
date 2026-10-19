#!/usr/bin/env bash
# migrate-baseline-stamp.sh — one-time schema_migrations backfill for databases
# bootstrapped by the OLD compose psql-loop sidecar (which executed
# migrations/*.sql directly and never wrote version records).
#
# WHY THIS VERIFIES BEFORE STAMPING — read before running:
#   `lens migrate` skips any version recorded in schema_migrations. STAMPED =
#   SKIPPED FOREVER. Blindly stamping 0001-0049 onto a database that actually
#   stopped mid-chain (e.g. the pre-fix psql loop died at 0037, leaving the DB
#   really at 0036) would mark the missing migrations as applied — they would
#   silently never run, and the schema would be permanently short with no error
#   anywhere. So this script probes a marker object for EVERY version (a
#   table/column/index/constraint that migration creates, chosen to survive all
#   later migrations) and stamps ONLY the verified prefix. Any gap in the
#   verified sequence (a version missing while a later one verifies) means the
#   database was hand-modified out of chain order — the script ABORTS and a
#   human must reconcile.
#
# Scope: the frozen psql-loop era, versions 0001-0049 exactly. Databases
# created by `lens migrate` never need this (they are version-tracked from
# birth); on such a DB every version is already recorded and this script
# no-ops. New migrations (0050+) only ever apply via `lens migrate` and must
# NOT be added here.
#
# Usage:
#   deploy/migrate-baseline-stamp.sh "postgres://user:pass@host:5432/db?sslmode=disable" [--dry-run]
#
# --dry-run prints the verification table and the stamp plan without writing.
set -euo pipefail

URL="${1:?usage: migrate-baseline-stamp.sh <postgres-url> [--dry-run]}"
DRY="${2:-}"

PSQL=(psql "$URL" -v ON_ERROR_STOP=1 -tA)

q() { "${PSQL[@]}" -c "$1"; }

tbl()  { q "SELECT (to_regclass('public.$1') IS NOT NULL)::text"; }
col()  { q "SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='$1' AND column_name='$2')::text"; }
cons() { q "SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='$1')::text"; }

probe() {
  case "$1" in
    0001) tbl prompt_embeddings ;;
    0002) tbl prompt_templates ;;
    0003) tbl ab_tests ;;
    0004) tbl branch_spend ;;
    0005) tbl workspaces ;;
    0006) tbl api_keys ;;
    0007) col token_events prompt_text ;;
    0008) tbl batch_jobs ;;
    0009) tbl sessions ;;
    0010) tbl prompts ;;
    0011) col token_events session_id ;;
    0012) tbl eval_test_cases ;;
    # 0013's only artifact (idx_token_events_anomaly) is destroyed by 0034's
    # token_events rebuild and never recreated — if 0034's effect is present,
    # 0013 necessarily ran first (the chain applies in order).
    0013) q "SELECT ((to_regclass('public.idx_token_events_anomaly') IS NOT NULL) OR EXISTS (SELECT 1 FROM pg_class WHERE relname='token_events' AND relkind='p'))::text" ;;
    0014) tbl guardrail_policies ;;
    0015) tbl quality_scores ;;
    0016) tbl experiments ;;
    0017) tbl request_attribution ;;
    0018) tbl workspace_configs ;;
    0019) tbl lens_token_ledger ;;
    0020) tbl inference_nodes ;;
    0021) tbl embedding_nodes ;;
    0022) tbl annotation_tasks ;;
    0023) tbl routing_patterns ;;
    0024) tbl marketplace_listings ;;
    0025) col inference_nodes node_secret_hash ;;
    0026) tbl cache_nodes ;;
    0027) tbl lxc_balances ;;
    0028) tbl budgets ;;
    0029) col token_events modality ;;
    0030) tbl eval_datasets ;;
    0031) tbl povi_receipts ;;
    0032) tbl povi_stakes ;;
    0033) tbl povi_challenges ;;
    0034) q "SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname='token_events' AND relkind='p')::text" ;;
    0035) col embedding_nodes node_secret_hash ;;
    0036) cons chk_token_balance_gte_zero ;;
    # 0037 DROPS an index that exists from 0001 onward: applied = absent.
    # (On a pre-fix DB that died AT 0037 the index is still present, so this
    # correctly reads unapplied and the stamp stops at 0036.)
    0037) q "SELECT (to_regclass('public.idx_token_events_prompt_hash') IS NULL)::text" ;;
    0038) tbl idx_marketplace_trades_listing ;;
    0039) col workspaces distill_policy ;;
    0040) col token_events distill_method ;;
    0041) col workspaces cache_poolable ;;
    0042) col prompt_embeddings contributor_workspace_id ;;
    0043) tbl pool_royalty_mints ;;
    0044) tbl pool_royalty_margin ;;
    0045) col pool_royalty_mints answer_sha256 ;;
    0046) col lens_token_balances held_balance ;;
    0047) tbl idx_pool_royalty_mints_entry ;;
    0048) tbl pool_royalty_adjudications ;;
    0049) tbl pattern_mine_credits ;;
    *) echo "false" ;;
  esac
}

# version -> filename, frozen for the psql-loop era (matches ls migrations/).
NAMES="0001_init.sql 0002_templates.sql 0003_ab_tests.sql 0004_branch_spend.sql 0005_workspaces.sql 0006_api_keys.sql 0007_warmer.sql 0008_batch_jobs.sql 0009_sessions.sql 0010_prompts.sql 0011_audit.sql 0012_eval.sql 0013_anomalies.sql 0014_guardrails.sql 0015_quality_scores.sql 0016_experiments.sql 0017_attribution.sql 0018_tenants.sql 0019_token_ledger.sql 0020_inference_nodes.sql 0021_embedding_nodes.sql 0022_annotations.sql 0023_patterns.sql 0024_marketplace.sql 0025_node_heartbeat.sql 0026_cache_nodes.sql 0027_lxc_credits.sql 0028_budgets.sql 0029_modality.sql 0030_eval_datasets.sql 0031_povi_receipts.sql 0032_povi_stakes.sql 0033_povi_challenges.sql 0034_partition_hot_tables.sql 0035_controlplane.sql 0036_check_constraints.sql 0037_drop_redundant_prompt_hash_idx.sql 0038_db_correctness.sql 0039_workspace_distill_policy.sql 0040_distill_method.sql 0041_workspace_cache_poolable.sql 0042_prompt_embeddings_pooling.sql 0043_pool_royalty_mints.sql 0044_pool_royalty_margin_view.sql 0045_pool_royalty_hashes.sql 0046_pool_royalty_holdback.sql 0047_pool_royalty_entry_index.sql 0048_pool_royalty_adjudications.sql 0049_pattern_mine_credits.sql"

# Same DDL as internal/dbmigrate/migrate.go — safe if it already exists.
q "CREATE TABLE IF NOT EXISTS schema_migrations (
     version    TEXT PRIMARY KEY,
     name       TEXT NOT NULL,
     applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
   )" >/dev/null

to_stamp=()
prefix_broken=""   # set to the first unverified version
echo "version  recorded  verified  action"
for name in $NAMES; do
  v="${name%%_*}"
  recorded=$(q "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version='$v')::text")
  if [ "$recorded" = "true" ]; then
    echo "$v     yes       -         skip (already recorded)"
    if [ -n "$prefix_broken" ]; then
      echo "ABORT: version $v is recorded but earlier version $prefix_broken did not verify — inconsistent database; reconcile by hand." >&2
      exit 1
    fi
    continue
  fi
  ok=$(probe "$v")
  if [ "$ok" = "true" ]; then
    if [ -n "$prefix_broken" ]; then
      echo "ABORT: version $v verifies but earlier version $prefix_broken does not — the applied set is not a prefix of the chain (hand-modified schema?); reconcile by hand." >&2
      exit 1
    fi
    echo "$v     no        yes       stamp"
    to_stamp+=("('$v','$name')")
  else
    if [ -z "$prefix_broken" ]; then prefix_broken="$v"; fi
    echo "$v     no        NO        leave for lens migrate"
  fi
done

if [ ${#to_stamp[@]} -eq 0 ]; then
  if [ -n "$prefix_broken" ]; then
    echo "Nothing verified to stamp; chain appears to stop before $prefix_broken. Run lens migrate to apply from there."
  else
    echo "All versions already recorded — nothing to stamp (this database is lens-migrate-tracked)."
  fi
  exit 0
fi

echo
echo "Stamp plan: ${#to_stamp[@]} version(s)${prefix_broken:+; chain verified through the version before $prefix_broken — lens migrate will apply the rest}."
if [ "$DRY" = "--dry-run" ]; then
  echo "--dry-run: not writing."
  exit 0
fi

vals=$(IFS=,; echo "${to_stamp[*]}")  # nosemgrep: bash.lang.security.ifs-tampering.ifs-tampering -- IFS is set inside the command substitution subshell only
q "BEGIN; INSERT INTO schema_migrations (version, name) VALUES $vals; COMMIT;" >/dev/null
echo "Stamped. lens migrate will now skip the recorded versions and apply anything newer."
