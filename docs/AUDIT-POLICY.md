# Audit-Trail Integrity & Retention Policy (U14)

This document records Talyvor Lens's audit-trail integrity guarantees, the threat
model they hold under, retention defaults, and the off-box export mechanism. It is
intended to be defensible in an enterprise security review.

## 1. The trail inventory

### Append-only set (enforced immutable — migration 0055)
| Table | What it records | Partitioned |
|---|---|---|
| `token_events` | one row per AI call (spend, tokens, model, PII flag) | HASH(workspace_id), 8-way |
| `lens_token_ledger` | every LENS credit/debit (the money ledger) | HASH(workspace_id), 8-way |
| `lxc_ledger` | every LXC credit/debit (fiat usage credit ledger) | HASH(workspace_id), 8-way |
| `request_attribution` | per-request spend attribution (branch/PR/author) | no |
| `povi_receipts` | signed proof-of-inference receipts (Merkle root + verify) | no |

### Mutable-by-design set (deliberately NOT guarded — legitimate state transitions)
| Table | Why it must mutate |
|---|---|
| `lxc_purchases` | a refund marks `status='refunded', refunded_at` (`internal/billing`) |
| `pool_royalty_mints` | the holdback lifecycle CAS: `held → final / revoked` (`internal/poolroyalty`) |
| `audit_export_state` | the export watermark advances on every successful export (0056) |

## 2. Enforcement (migration 0055)

A single trigger function (`audit_block_mutation`) is attached to the five
append-only tables:

- **`BEFORE UPDATE OR DELETE … FOR EACH ROW`** on each parent. A row trigger on a
  partitioned parent is **automatically created on every partition** (current and
  future), so `UPDATE`/`DELETE` are rejected whether issued against the parent OR
  directly against a partition (empirically verified on pg16).
- **`BEFORE TRUNCATE … FOR EACH STATEMENT`** on each parent **and every partition**.
  TRUNCATE triggers do **not** cascade to partitions, so they are enumerated; the
  hash modulus is fixed at 8, so the partition set is closed.

### Scoped retention bypass
`token_events` is both append-only **and** the retention target, so a `DELETE` on
`token_events` is permitted **only** inside a transaction that runs
`SET LOCAL lens.audit_retention = 'on'` — which only the retention sweeper
(`internal/audit/retention.go`) does. `UPDATE` and `TRUNCATE` remain blocked even
under the flag; the **ledgers are never deletable** (see §4). A stray `DELETE`
without the flag (app bug / injection) is still rejected.

**The append-only guarantee has exactly one scoped exception:** the retention
sweeper may `DELETE` aged `token_events` rows under the flag; `UPDATE`/`TRUNCATE`
and all ledger tables (`lens_token_ledger`, `lxc_ledger`) have **no exception**.
This single-caller property is pinned by a static test
(`TestAuditIntegrity_RetentionBypassFlagSingleCaller`): the flag is referenced in
exactly one non-test file — the sweeper — and any second reference fails the build.

### ⚠ Repartitioning constraint
The `BEFORE TRUNCATE` triggers were enumerated over the partitions that existed
**at migration time**. If any future migration repartitions `token_events`,
`lens_token_ledger`, or `lxc_ledger` (new partitions, a different modulus, or
range instead of hash), the `audit_no_truncate` triggers **must be re-applied** to
the new partition set. The row-level `audit_no_mutation` trigger re-cascades
automatically; the TRUNCATE triggers do not.

## 3. Threat model — stated honestly

**Protects against:** application bugs, accidental mutation, and SQL-injection that
issues `UPDATE`/`DELETE`/`TRUNCATE` against the audit tables (via the parent or a
partition).

**Does NOT protect against, by default:** a **compromised database role**. Unless
you switch on role separation below, Lens logs in as the role that runs the
migrations and owns the tables, and that role can `ALTER TABLE … DISABLE TRIGGER`,
`DROP TRIGGER`, or `SET lens.audit_retention='on'` to bypass the guard.
Tamper-evidence (hash-chaining / signed checkpoints) is a separate, additive layer
not built here.

### Role separation (B39.6, #186) — opt-in

`lens migrate` creates two roles (NOLOGIN until given a password) and re-applies
their grants after every run, from the live schema, so a table a later migration
adds is covered at the next migrate:

| Role | Gets | Never gets |
|---|---|---|
| the migration login (`lens` on compose) | owns the schema and every `audit_block_mutation` trigger | — |
| `lens_app` — Lens itself | `SELECT, INSERT, UPDATE, DELETE` on every table, sequences | `UPDATE, DELETE, TRUNCATE` on any table an `audit_block_mutation` trigger guards; any write to `schema_migrations`; ownership, so it cannot `DISABLE` or `DROP` a trigger |
| `lens_retention` — the retention sweeper | `SELECT, DELETE` on `token_events`, `SELECT` on `audit_export_state` | anything else |

As `lens_app`, `UPDATE money_postings` fails with `permission denied for table
money_postings` and `ALTER TABLE money_postings DISABLE TRIGGER …` with `must be
owner of table money_postings` (both SQLSTATE 42501), and setting the retention
flag is no bypass: without `DELETE` it changes nothing.
`internal/dbmigrate/roles_integration_test.go` proves this on the full schema.

**Switching on, compose.** Put a URL-safe password (`openssl rand -hex 24`) in
`.env` as `LENS_APP_DB_PASSWORD` and run `docker compose up -d`. `migrate` gives
`lens_app` that password (sent as a SCRAM verifier, so no log holds it), and PgBouncer and Lens then log in as `lens_app`. The
migrate service keeps the owner login. With it unset nothing changes: Lens logs
in as `lens`, exactly as before. To switch back, remove the line and `up -d` again.

**Retention under its own role.** Under `lens_app` the sweeper cannot delete, so
with `LENS_AUDIT_RETENTION` set, also put `LENS_RETENTION_DB_PASSWORD` in `.env`
(migrate gives `lens_retention` that login) and in `lens.env`:
`LENS_AUDIT_RETENTION_DATABASE_URL=postgres://lens_retention:<password>@postgres:5432/talyvor_lens?sslmode=disable`.
Unset, the sweeper uses Lens's own login, as before. The sweeper runs inside
Lens, so Lens holds this login too: it can delete `token_events` rows, never a
ledger row.

**Switching on, Helm.** Point the Secret's `LENS_DATABASE_URL` at
`lens_app`, set `migrations.databaseURL` to the owner's direct URL (the Job then
uses it and the Deployment keeps the Secret's), and add `LENS_APP_DB_PASSWORD`
(and optionally `LENS_RETENTION_DB_PASSWORD`) to the Secret so the migration Job
sets the logins. With PgBouncer in front, its user must be `lens_app` too.

**Or by hand:** `ALTER ROLE lens_app LOGIN PASSWORD '…'` as the owner; migrate
leaves a login it was given no password for alone.

If the migration login may not create roles (some managed Postgres), migrate
logs a warning and carries on unless a password was asked for.

## 4. Retention

- **Config:** `LENS_AUDIT_RETENTION` (Go duration). **Default unset / `<= 0` =
  disabled** (rows kept indefinitely). The sweeper runs leader-only, hourly,
  deleting `token_events` older than the window in batches of **5000** to bound
  lock/WAL/vacuum cost on the highest-volume table.
- **`token_events` ONLY.** The sweeper is **hardcoded** to `token_events` — there
  is no table knob, so a config can never point it at a ledger.
- **The ledger constraint (why ledgers are never retention-eligible):**
  `mining.LedgerStore.GetTotalSupply` (`internal/mining/cache_mining.go`) computes
  total LENS supply as `SUM(amount)` over the **entire** `lens_token_ledger`
  history, feeding the peg and circulating supply. Deleting old ledger rows would
  silently corrupt reconciliation. A static test
  (`TestAuditIntegrity_NoLedgerMutationInProductionCode`) pins **zero**
  `UPDATE`/`DELETE` against `lens_token_ledger`/`lxc_ledger` in production code,
  forever.

## 5. Off-box export

- **Config:** `LENS_AUDIT_EXPORT_URL` (sink) + `LENS_AUDIT_EXPORT_INTERVAL`
  (Go duration, default 1h). **Default off** (empty URL ⇒ no loop).
- A leader-elected loop exports `token_events` in the window `(watermark, now]` as
  NDJSON to the sink (reusing the audit webhook exporter), advancing the persisted
  watermark (`audit_export_state`, 0056) to `now` **only on a successful POST**. On
  failure the watermark does not advance, so the next run re-covers the gap:
  **at-least-once** delivery (no audit row is lost; a boundary row may rarely
  duplicate — dedup on `request_id`).

### Retention ↔ export ordering
Retention and export are **independent** loops. By **default** the retention sweeper
deletes by age and does **not** consult the export watermark (today's behaviour,
unchanged), so if `LENS_AUDIT_RETENTION` is shorter than the export lag, retention
could delete `token_events` rows before they are exported. **Operational guidance for
the default:** set `LENS_AUDIT_RETENTION` far larger than the export interval
(months/years vs. minutes/hours).

**Proof-of-export-before-delete (U14 #187, opt-in).** Set
`LENS_AUDIT_REQUIRE_EXPORT_BEFORE_PRUNE=true` to gate the sweeper behind the export
watermark: a row is pruned only when `created_at <= audit_export_state.last_exported_at`
**and** `< (now − retention)`, so an aged-but-un-exported row is **kept** until it has
been exported off-box. The watermark is read once per sweep (it only advances, so a
stale read prunes less, never more). If export is **disabled** while this is on, the
sweep is **skipped with a warning** rather than pruning un-exportable rows. Default
`false` preserves the age-only behaviour exactly — flipping it on is the
customer-/compliance-driven decision, not a code default.

## Follow-ups
- **DB role separation** (migration role vs app role) — BUILT (#186, B39.6, opt-in
  via `LENS_APP_DB_PASSWORD`; §3).
- **Export-then-prune watermark** — proof-of-export-before-delete. BUILT (#187,
  opt-in via `LENS_AUDIT_REQUIRE_EXPORT_BEFORE_PRUNE`, default off).
