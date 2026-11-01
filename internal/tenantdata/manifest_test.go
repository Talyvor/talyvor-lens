package tenantdata

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// manifest_test.go — THE GUARD. The manifest is a list; this is what keeps it true.
//
// It reads the LIVE SCHEMA rather than a hardcoded table list, deliberately: every hardcoded count
// in this codebase has gone stale, usually within a day, and a stale deletion manifest is worse
// than none — it produces a confident claim that someone's data is gone when a table was missed.
// So the source of truth is information_schema, and this test fails when the schema grows a
// tenant-scoped table nobody classified.
//
// COST TO RUN: it needs a database with the migrations applied, so it is an INTEGRATION test, not
// a unit test. CI already provides one (LENS_TEST_DATABASE_URL, the pgvector service in ci.yaml),
// and it applies the real embedded migration chain into a private schema — the same harness the
// register-workspace and provision tests use. Without the variable it SKIPS, which means a
// developer running `go test ./...` on a laptop with no database does not get this coverage. That
// is a real limitation and the reason it must stay in CI's required set rather than being treated
// as an optional extra.

const manifestSchema = "lens_it_tenantdata"
const manifestSetupLock = 727274 // shared with the peer harnesses: serialises extension/catalog DDL

var manifestMigrateOnce sync.Once

// partitionSuffix matches the _p0.._p7 children of the partitioned audit tables. Partitions are
// collapsed to their parent: a DELETE against the parent reaches every partition, and listing the
// children would rot the moment the partition count changes.
var partitionSuffix = regexp.MustCompile(`_p\d+$`)

func schemaConn(t *testing.T) *pgx.Conn {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — the manifest guard needs a migrated database")
	}
	ctx := context.Background()

	manifestMigrateOnce.Do(func() {
		admin, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatalf("connect for setup: %v", err)
		}
		tx, err := admin.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, manifestSetupLock); err != nil {
			t.Fatalf("advisory lock: %v", err)
		}
		for _, stmt := range []string{
			`CREATE EXTENSION IF NOT EXISTS vector`,
			`DROP SCHEMA IF EXISTS ` + manifestSchema + ` CASCADE`,
			`CREATE SCHEMA ` + manifestSchema,
		} {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		_ = admin.Close(ctx)

		cfg, err := pgx.ParseConfig(url)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		cfg.RuntimeParams["search_path"] = manifestSchema + ",public"
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("connect for migrate: %v", err)
		}
		defer conn.Close(ctx)
		if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
			t.Fatalf("apply migrations: %v", err)
		}
	})

	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.RuntimeParams["search_path"] = manifestSchema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// tenantKeyedTablesSQL is the POPULATION, and every clause in it is load-bearing.
//
//	relkind IN ('r','p')  ordinary and partitioned tables only. ⚠ THE WIDENED SCAN NEEDS THIS AND
//	                      THE NARROW ONE DID NOT: information_schema.columns describes views too,
//	                      and distill_royalty_margin / pool_royalty_margin both expose
//	                      *_workspace_id. Without the filter the guard reports two extra
//	                      "unclassified tenant tables" — a false finding of the flattering kind,
//	                      and one a DELETE would then fail against. TestPopulationExcludesViews.
//	NOT relispartition    partitions collapse to their parent; a DELETE on the parent reaches them.
//	ILIKE '%workspace%'     every spelling of the tenant key, measured rather than listed: see the
//	                      table in manifest.go's package doc. A hand-written list of six column
//	                      names would go stale the first time somebody invents a seventh.
const tenantKeyedTablesSQL = `
	SELECT DISTINCT c.relname
	FROM pg_class c
	JOIN pg_namespace n ON n.oid = c.relnamespace
	JOIN information_schema.columns col
	  ON col.table_name = c.relname AND col.table_schema = n.nspname
	WHERE n.nspname = $1
	  AND c.relkind IN ('r','p')
	  AND NOT c.relispartition
	  AND col.column_name ILIKE '%workspace%'`

// liveTenantTables asks the SCHEMA which tables carry a tenant key under ANY of its spellings,
// plus the declared MappingTables, whose key is `id` and which therefore cannot be discovered by
// a column scan at all.
func liveTenantTables(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, m := range MappingTables {
		seen[m] = true
	}
	rows, err := conn.Query(context.Background(), tenantKeyedTablesSQL, manifestSchema)
	if err != nil {
		t.Fatalf("query schema: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen[partitionSuffix.ReplaceAllString(name, "")] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// cascadeParents maps child table → the tables it references with ON DELETE CASCADE.
func cascadeParents(t *testing.T, conn *pgx.Conn) map[string][]string {
	t.Helper()
	rows, err := conn.Query(context.Background(), `
		SELECT cl.relname, pr.relname
		FROM pg_constraint con
		JOIN pg_class cl ON cl.oid = con.conrelid
		JOIN pg_class pr ON pr.oid = con.confrelid
		JOIN pg_namespace n ON n.oid = cl.relnamespace
		WHERE con.contype = 'f' AND con.confdeltype = 'c' AND n.nspname = $1`, manifestSchema)
	if err != nil {
		t.Fatalf("query fks: %v", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			t.Fatalf("scan fk: %v", err)
		}
		child = partitionSuffix.ReplaceAllString(child, "")
		parent = partitionSuffix.ReplaceAllString(parent, "")
		if child != parent {
			out[child] = append(out[child], parent)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate fks: %v", err)
	}
	return out
}

// reachedByCascade reports whether deleting the manifest's Delete tables also removes `table`,
// by following ON DELETE CASCADE edges upward to a Delete-classified root.
//
// ⚠ THIS IS THE CLAUSE THAT KEEPS THE FINDING HONEST, and it earned its place: the first pass at
// W6.26 had session_turns, experiment_results, cache_node_metrics and node_metrics down as
// unreachable tenant data. All four cascade off a parent the manifest already deletes, so all four
// were FALSE. Calling a covered table a gap is the flattering error here, and four of the first
// seventeen candidates were exactly that.
//
// ⚠ AND THE ROOT MUST BE *Delete*, NOT MERELY PRESENT. A cascade into a Retain table reaches
// nothing, because the Retain tables are the ones migration 0055 makes undeletable.
func reachedByCascade(table string, parents map[string][]string, depth int) bool {
	if depth > 16 {
		return false // cycle guard; the schema has one self-referencing FK
	}
	for _, p := range parents[table] {
		if Manifest[p].Disposition == Delete {
			return true
		}
		if reachedByCascade(p, parents, depth+1) {
			return true
		}
	}
	return false
}

// ⚠ THE GUARD. A tenant-scoped table nobody classified is a table a by-hand deletion will miss.
func TestManifestCoversEveryTenantScopedTable(t *testing.T) {
	conn := schemaConn(t)
	parents := cascadeParents(t, conn)
	var missing []string
	for _, table := range liveTenantTables(t, conn) {
		if _, ok := Manifest[table]; ok {
			continue
		}
		// Not classified — but a child that ON DELETE CASCADEs to a Delete-classified root is
		// genuinely reached by the procedure, and naming it here would be a false gap.
		//
		// ⚠ THIS BRANCH DOES NOT CURRENTLY FIRE, AND SAYING SO IS THE POINT. Every table in the
		// population carries a tenant key of its own, so every one is classified directly;
		// measured, not assumed — the only population member with a cascade parent is
		// prompt_embeddings, via its SELF-reference. A clause that cannot fire is exactly what
		// this queue keeps finding, so: it is kept because the population and the FK graph both
		// move (a table that gains a `%workspace%` column while already cascading off a deleted
		// parent would need it), and the work the cascade analysis actually does today is in
		// TestCascadeOnlyChildrenStillReachADeleteParent, which is not inert.
		if reachedByCascade(table, parents, 0) {
			continue
		}
		missing = append(missing, table)
	}
	if len(missing) > 0 {
		t.Errorf(`%d tenant-scoped table(s) are NOT in the manifest: %s

A table with a workspace_id column holds a customer's data. Unclassified, it is a table the
by-hand deletion procedure will silently miss — which is worse than having no procedure, because
it produces a confident claim that someone's data is gone.

Add each to Manifest in internal/tenantdata/manifest.go with one of:
    Delete          — customer data, remove it
    Retain          — with the legal reason; only for audit-guarded rows that cannot be deleted
    NotTenantScoped — carries workspace_id but describes nobody; say why`,
			len(missing), strings.Join(missing, ", "))
	}
}

// The other direction: a manifest entry for a table that no longer exists is stale, and a stale
// entry in a deletion procedure is a DELETE against a missing table — which aborts the run at 2am.
func TestManifestHasNoEntriesForTablesThatNoLongerExist(t *testing.T) {
	conn := schemaConn(t)
	live := map[string]bool{}
	for _, n := range liveTenantTables(t, conn) {
		live[n] = true
	}
	for table := range Manifest {
		// `workspaces` is the MAPPING table: its key is `id`, not `workspace_id`, so the column
		// query above cannot discover it. It still belongs in the manifest and in DeleteOrder —
		// deleting it is what makes the retained ledger rows unlinkable — so it is exempt from
		// the staleness check rather than removed. (The guard found this itself: the first
		// version of the manifest failed here, which is the check working.)
		if table == "workspaces" {
			continue
		}
		if !live[table] {
			t.Errorf("manifest lists %q but no such tenant-scoped table exists — a DELETE against "+
				"it would abort the by-hand run partway through", table)
		}
	}
}

// A classification without a reason is an assertion. Retain and NotTenantScoped must justify
// themselves, so the next person can tell a decision from an oversight.
func TestRetainAndNotScopedEntriesCarryAReason(t *testing.T) {
	for table, e := range Manifest {
		if (e.Disposition == Retain || e.Disposition == NotTenantScoped) && strings.TrimSpace(e.Why) == "" {
			t.Errorf("%q is classified %v with no reason. Retaining or excluding a customer's data "+
				"needs a stated justification, not a bare classification.", table, e.Disposition)
		}
	}
}

// The delete order must end with the mapping. An interrupted run that removed `workspaces` first
// would leave orphaned rows and no way to find them again — the procedure could not be restarted.
func TestDeleteOrderEndsWithTheMapping(t *testing.T) {
	order := DeleteOrder()
	if len(order) == 0 {
		t.Fatal("DeleteOrder is empty")
	}
	if last := order[len(order)-1]; last != "workspaces" {
		t.Errorf("DeleteOrder ends with %q, want \"workspaces\" — the mapping must go last so an "+
			"interrupted run can be restarted", last)
	}
	for _, tbl := range order {
		if Manifest[tbl].Disposition != Delete {
			t.Errorf("DeleteOrder includes %q, which is not classified Delete", tbl)
		}
	}
}

// ─── W6.26: the controls. Each clause of the population, proved load-bearing. ────────────────

// w626PopulationDelta is everything the widened population sees that a bare `workspace_id` scan
// does not. Recorded by name because a count alone cannot tell a table that got classified from
// one that was dropped from the schema.
//
// ⚠ `workspaces` IS IN THIS LIST AND WAS NEVER A GAP, and the difference is the finding in
// miniature. The old scan could not see it either — its key is `id` — but the staleness check
// exempted it BY NAME, so it stayed classified. The category was known and only one member of it
// was written down. workspace_configs is the member nobody wrote down.
var w626PopulationDelta = []string{
	"agent_escrows",                           // B22.6: keyed payer_/payee_workspace_id, classified in the manifest
	"agent_loans",                             // B22.5: keyed lender_/borrower_workspace_id, classified in the manifest
	"agent_money_requests", "agent_transfers", // B22.3: keyed from_/to_workspace_id, classified in the manifest
	"annotation_tasks", "benchmark_eval_items", "confidential_compute_mints",
	"distill_royalty_basis", "distill_royalty_mints", "distill_serve_attribution",
	"eval_contribution_mints", "eval_correctness_attestations",
	"market_earnings",
	"market_invoices",            // B28.140: keyed buyer_workspace_id, classified in the manifest
	"market_ip_claims",           // B32.47: keyed claimant_workspace_id, classified in the manifest
	"market_licence_idle_alerts", // B41.176: keyed buyer_workspace_id, classified in the manifest
	"market_licences",            // B32.19: keyed buyer_workspace_id, classified in the manifest
	"market_listing_reports",     // B20.2: keyed reporter_workspace_id, classified in the manifest
	"market_receipts",            // B32.40: keyed buyer_workspace_id, classified in the manifest
	"market_refunds",             // B20.4: keyed seller_/buyer_workspace_id, classified in the manifest
	"market_reviews",             // B32.49: keyed buyer_workspace_id, classified in the manifest
	"market_uses",                // B20.2: keyed seller_/buyer_workspace_id, classified in the manifest
	"node_latency_mints",
	"pool_royalty_mints",
	"room_contributions", // B32.31: keyed author_workspace_id, classified in the manifest
	"room_messages",      // B32.30: keyed author_workspace_id, classified in the manifest
	"room_prizes",        // B32.35: keyed poster_workspace_id, classified in the manifest
	"room_reports",       // B32.52: keyed reporter_workspace_id, classified in the manifest
	"rooms",              // B32.28: keyed owner_workspace_id, classified in the manifest
	"routing_prediction_mints", "royalty_detector_findings",
	"test_crossing_reversals", // B26.15: keyed from_/to_workspace_id, classified in the manifest
	"workspace_configs", "workspaces",
}

// w626NewlyClassified are the thirteen that were genuinely unclassified: tenant data the by-hand
// erasure procedure could not reach, because the guard that keeps the procedure honest could not
// see them. This is the finding's actual population.
var w626NewlyClassified = []string{
	"annotation_tasks", "benchmark_eval_items", "confidential_compute_mints",
	"distill_royalty_basis", "distill_royalty_mints", "distill_serve_attribution",
	"eval_contribution_mints", "eval_correctness_attestations", "node_latency_mints",
	"pool_royalty_mints", "routing_prediction_mints", "royalty_detector_findings",
	"workspace_configs",
}

// narrowTenantTables reproduces the population this guard used BEFORE W6.26 — a column named
// exactly `workspace_id` — so the widening can be measured rather than asserted.
func narrowTenantTables(t *testing.T, conn *pgx.Conn) map[string]bool {
	t.Helper()
	rows, err := conn.Query(context.Background(), `
		SELECT DISTINCT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN information_schema.columns col
		  ON col.table_name = c.relname AND col.table_schema = n.nspname
		WHERE n.nspname = $1 AND c.relkind IN ('r','p') AND NOT c.relispartition
		  AND col.column_name = 'workspace_id'`, manifestSchema)
	if err != nil {
		t.Fatalf("narrow query: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[partitionSuffix.ReplaceAllString(n, "")] = true
	}
	return out
}

// ⚠ CONTROL 1 — THE WIDENING IS REAL, AND THIS IS THE FINDING AS A NUMBER. If the two populations
// ever coincide, either somebody renamed every tenant key to workspace_id (good, and this test
// should be deleted saying so) or the ILIKE stopped matching (bad, and the guard is back to
// missing thirteen tables silently).
func TestPopulationIsStrictlyWiderThanABareWorkspaceIDScan(t *testing.T) {
	conn := schemaConn(t)
	narrow := narrowTenantTables(t, conn)
	if len(narrow) == 0 {
		t.Fatal("the narrow population is empty — the comparison below would report the whole " +
			"schema as newly visible and prove nothing")
	}
	wide := map[string]bool{}
	for _, n := range liveTenantTables(t, conn) {
		wide[n] = true
	}
	for n := range narrow {
		if !wide[n] {
			t.Errorf("%q is in the OLD population and not the new one — widening must never drop a "+
				"table that was already classified", n)
		}
	}
	var extra []string
	for n := range wide {
		if !narrow[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	if len(extra) == 0 {
		t.Fatal("the widened population found NOTHING the bare workspace_id scan did not. W6.26's " +
			"entire finding is that it finds thirteen tables; zero means the ILIKE is not matching")
	}
	if got, want := strings.Join(extra, ","), strings.Join(w626PopulationDelta, ","); got != want {
		t.Errorf("the set of newly-visible tenant tables changed.\n  now:  %s\n  W6.26: %s\n"+
			"A table ADDED here is one the old guard would have missed — classify it. A table "+
			"REMOVED means it was renamed or dropped; say which, because a shrinking gap list and a "+
			"broken scan look identical from here.", got, want)
	}
	t.Logf("MEASURED: %d tables carry a tenant key; %d of them answer to a bare `workspace_id`. "+
		"The other %d were invisible to the deletion manifest's guard.", len(wide), len(narrow), len(extra))
}

// ⚠ CONTROL 2 — VIEWS ARE EXCLUDED, AND THE EXCLUSION IS NOT AN ACCIDENT. The widened scan reads
// information_schema.columns, which describes views as well as tables. Two views in this schema
// expose *_workspace_id. Reporting them as unclassified tenant tables would be a false finding,
// and putting them in DeleteOrder would abort the by-hand run on a DELETE against a view.
func TestPopulationExcludesViews(t *testing.T) {
	conn := schemaConn(t)
	rows, err := conn.Query(context.Background(), `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN information_schema.columns col
		  ON col.table_name = c.relname AND col.table_schema = n.nspname
		WHERE n.nspname = $1 AND c.relkind = 'v' AND col.column_name ILIKE '%workspace%'
		GROUP BY c.relname`, manifestSchema)
	if err != nil {
		t.Fatalf("query views: %v", err)
	}
	defer rows.Close()
	var views []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		views = append(views, n)
	}
	// ARMED: if no such view exists, this test cannot fail and the relkind filter is untested.
	if len(views) == 0 {
		t.Skip("no view exposes a %workspace% column any more — the relkind filter is currently " +
			"unexercised, so this control proves nothing until one exists again")
	}
	live := map[string]bool{}
	for _, n := range liveTenantTables(t, conn) {
		live[n] = true
	}
	for _, v := range views {
		if live[v] {
			t.Errorf("the population includes %q, which is a VIEW. It holds no rows of its own, a "+
				"DELETE against it fails, and classifying it would put a false entry in DeleteOrder", v)
		}
		if _, ok := Manifest[v]; ok {
			t.Errorf("the manifest classifies %q, which is a VIEW — DeleteOrder would emit a DELETE "+
				"that aborts the by-hand run", v)
		}
	}
	t.Logf("MEASURED: %d view(s) expose a tenant key and are correctly outside the population: %s",
		len(views), strings.Join(views, ", "))
}

// ⚠ CONTROL 3 — CASCADE REACHABILITY DISCRIMINATES. A reachability helper that returned true for
// everything would hide every real gap; one that returned false for everything would manufacture
// four false ones. Both directions are asserted on named tables.
func TestCascadeReachabilityDiscriminates(t *testing.T) {
	conn := schemaConn(t)
	parents := cascadeParents(t, conn)
	if len(parents) == 0 {
		t.Fatal("no ON DELETE CASCADE edges found — reachedByCascade would be constantly false and " +
			"the guard would report covered tables as gaps")
	}
	// Reached: session_turns holds a workspace's conversation turns and has no tenant key of its
	// own; it is erased through sessions, which the manifest deletes.
	if Manifest["sessions"].Disposition != Delete {
		t.Fatal("`sessions` is no longer classified Delete — the positive case below is not one")
	}
	if !reachedByCascade("session_turns", parents, 0) {
		t.Error("session_turns is not reported as cascade-reached. It ON DELETE CASCADEs off " +
			"`sessions`; if that edge is gone, a workspace's turns now SURVIVE the procedure and " +
			"session_turns needs its own manifest entry")
	}
	// NOT reached: a table with no FK at all. eval_results names a run_id and a test_case_id and
	// enforces NEITHER, so deleting eval_runs leaves its rows behind.
	if reachedByCascade("eval_results", parents, 0) {
		t.Error("eval_results is reported as cascade-reached — it has no foreign key at all, so " +
			"reachedByCascade is returning true too easily and every real gap would be hidden")
	}
	// And the root must be Delete, not merely present: a cascade into a Retain table reaches
	// nothing, because migration 0055 makes those rows undeletable.
	if reachedByCascade("lens_token_ledger", parents, 0) {
		t.Error("an audit-guarded Retain table is reported as cascade-reached")
	}
}

// ⚠ CONTROL 4 — THE MAPPING TABLES ARE DECLARED, NOT ASSUMED. MappingTables is the one part of the
// population a schema scan cannot derive, so it is the one part that can rot silently.
func TestMappingTablesExistAndAreClassified(t *testing.T) {
	conn := schemaConn(t)
	if len(MappingTables) < 2 {
		t.Fatalf("MappingTables has %d entries; `workspaces` and `workspace_configs` are both "+
			"keyed by `id` and both hold the customer's name", len(MappingTables))
	}
	for _, m := range MappingTables {
		var kind string
		err := conn.QueryRow(context.Background(), `
			SELECT c.relkind::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = $2`, manifestSchema, m).Scan(&kind)
		if err != nil {
			t.Errorf("MappingTables names %q and no such relation exists: %v", m, err)
			continue
		}
		if kind != "r" && kind != "p" {
			t.Errorf("MappingTables names %q, whose relkind is %q — not a table", m, kind)
		}
		if _, ok := Manifest[m]; !ok {
			t.Errorf("MappingTables names %q and the manifest does not classify it. This is the "+
				"exact hole W6.26 found: `workspaces` was exempted by name while `workspace_configs`, "+
				"its sibling in tenant.Store, was neither exempted nor classified", m)
		}
	}
}

// ⚠ CONTROL 5 — THE THIRTEEN CARRY THEIR REASONING. The pre-W6.26 entries are bare `{Delete, ""}`,
// which was tolerable when the population was one column name. These were classified by applying
// the manifest's own rule (Retain is for audit-guarded rows only), and an applied rule that does
// not say which precedent it applied is indistinguishable from a guess.
func TestW626EntriesCarryTheirReason(t *testing.T) {
	for _, tbl := range w626NewlyClassified {
		e, ok := Manifest[tbl]
		if !ok {
			t.Errorf("%q is missing from the manifest", tbl)
			continue
		}
		if strings.TrimSpace(e.Why) == "" {
			t.Errorf("%q carries no reason. It was invisible to the old guard, so nobody has ever "+
				"reviewed its disposition — a bare classification here is a guess wearing a decision's "+
				"clothes", tbl)
		}
		if e.Disposition == Delete && !strings.Contains(e.Why, "W6.26") {
			t.Errorf("%q's reason does not say where the classification came from", tbl)
		}
	}
}

// ⚠ CONTROL 6 — AND THEY REACH THE PROCEDURE. Classifying a table Delete and leaving it out of
// DeleteOrder would satisfy every test above while changing nothing about what gets erased.
func TestTheNewlyVisibleTablesAreInTheDeleteOrder(t *testing.T) {
	order := map[string]bool{}
	for _, tbl := range DeleteOrder() {
		order[tbl] = true
	}
	for _, tbl := range w626NewlyClassified {
		if Manifest[tbl].Disposition != Delete {
			continue // a later decision may reclassify one; that is not this test's business
		}
		if !order[tbl] {
			t.Errorf("%q is classified Delete and DeleteOrder does not emit it — the by-hand "+
				"procedure still misses it, which is the whole defect W6.26 found", tbl)
		}
	}
	if idx, last := indexOf(DeleteOrder(), "workspace_configs"), len(DeleteOrder())-1; idx == last {
		t.Error("workspace_configs is last in DeleteOrder; `workspaces` must be, so an interrupted " +
			"run can still find the workspace and be restarted")
	}
}

func indexOf(s []string, want string) int {
	for i, v := range s {
		if v == want {
			return i
		}
	}
	return -1
}

// ⚠ CONTROL 7 — THE CASCADE ANALYSIS, WHERE IT ACTUALLY BITES. These five tables hold a customer's
// data and carry NO tenant key of their own, so they are outside the manifest's population by
// design and always will be. They are erased only because an ON DELETE CASCADE carries the
// deletion down from a parent the manifest deletes. Drop one of those foreign keys — or reclassify
// one of those parents — and that customer's rows quietly start surviving erasure, with nothing
// else in the repository to notice.
//
// ⚠ `annotations` REACHES A DELETE PARENT ONLY BECAUSE THIS MERGE CLASSIFIED annotation_tasks.
// Before W6.26 its parent was unclassified, so the cascade led nowhere and BOTH tables survived
// the procedure. It is the fourteenth table this item made reachable and the only one that needed
// no entry of its own.
var cascadeOnlyChildren = map[string]string{
	"annotations":        "annotation_tasks",
	"cache_node_metrics": "cache_nodes",
	"experiment_results": "experiments",
	"node_metrics":       "inference_nodes",
	"session_turns":      "sessions",
}

func TestCascadeOnlyChildrenStillReachADeleteParent(t *testing.T) {
	conn := schemaConn(t)
	parents := cascadeParents(t, conn)

	// Non-vacuity: the census must describe the live schema, not a snapshot of it. A NEW
	// cascade-only child is a table nobody has considered.
	live := map[string]bool{}
	for child := range parents {
		live[child] = true
	}
	for child, wantParent := range cascadeOnlyChildren {
		if !live[child] {
			t.Errorf("%q no longer has any ON DELETE CASCADE parent. It carries no tenant key, so "+
				"nothing else reaches it — that customer's rows now survive erasure and %q needs its "+
				"own manifest entry", child, child)
			continue
		}
		var found bool
		for _, p := range parents[child] {
			if p == wantParent {
				found = true
			}
		}
		if !found {
			t.Errorf("%q no longer cascades off %q (parents now: %s) — say which table erases it now",
				child, wantParent, strings.Join(parents[child], ", "))
		}
		if !reachedByCascade(child, parents, 0) {
			t.Errorf("%q is not reached by any Delete-classified parent. Its parent %q is classified "+
				"%v; a cascade into a Retain table erases nothing, because migration 0055 makes those "+
				"rows undeletable", child, wantParent, Manifest[wantParent].Disposition)
		}
	}
	t.Logf("MEASURED: %d tables hold tenant rows with no tenant key of their own and are erased "+
		"only by cascade.", len(cascadeOnlyChildren))
}
