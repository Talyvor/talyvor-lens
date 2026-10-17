package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// background_job_classification_test.go — every background goroutine main.go starts, and whether
// running it on more than one replica is safe.
//
// main.go starts 45 goroutines. 35 are wrapped in `haComps.leader.Run(...)` — exactly one instance
// across the fleet — and the file already documents the other category in as many words ("NOT
// leader-gated: each replica must refresh its OWN cache"). ⚠ SO THE CODEBASE ALREADY KNOWS THERE
// ARE TWO BUCKETS. Nothing checked which bucket a given job was in, and a job added to the wrong
// one is invisible: it works perfectly on one replica and misbehaves only after a scale-out.
//
// ⚠ THE COUNT IS NOT THE FINDING AND WAS NEVER GOING TO BE. Nine of the ten ungated jobs are
// ungated CORRECTLY, and leader-gating any of them would be a bug — a per-replica cache that only
// the leader refreshed would serve every follower stale. Each is classified individually below with
// the property that makes it safe, because a census that reports "10 ungated jobs" as a defect is
// manufacturing one.
//
// THE PROPERTY THAT DECIDES IT, measured per job rather than argued from the schedule:
//
//	IN-PROCESS STATE     the work touches this replica's own memory. MUST be per-replica.
//	IDEMPOTENT           an age predicate or a row lock makes the Nth run affect nothing.
//	PER-REQUEST          started inside an HTTP handler; it belongs to no fleet-wide bucket.
//	SINGLETON            shared state, non-idempotent, or it costs money. Must be leader-gated.

// perReplica is every goroutine deliberately NOT leader-gated, with the measured property that
// makes running it on every replica correct. ⚠ A REASON HERE IS A CLAIM ABOUT CODE, not a label:
// each was read before being written down.
var perReplica = map[string]string{
	"watcher.ApplyLoop": "IN-PROCESS STATE (B10.5). Apply reads the discovery record and writes only this " +
		"replica's in-memory catalog (catalog.Override); it never writes a row. Leader-gating it would leave " +
		"every follower offering retired models and missing confirmed prices.",
	"batchRouter.StartPoller": "IN-PROCESS STATE. pollAll iterates r.pending, a map held in this " +
		"replica's memory, so a replica polls only the jobs submitted to it. Nothing to duplicate.",
	"sessionTracker.StartCleanup": "IN-PROCESS STATE. evictStale walks t.sessions under t.mu — an " +
		"in-memory map. Leader-gating it would leak session state on every follower.",
	"statusPage.StartCacher": "IN-PROCESS STATE. Refreshes this replica's own status snapshot; a " +
		"follower that never refreshed would serve a stale status page forever.",
	"l.StartBackground": "READ-ONLY. The learner's loop calls Analyse and logs the top patterns. " +
		"Measured: no INSERT, no UPDATE, no ON CONFLICT anywhere in internal/learner. Duplicate log " +
		"lines across replicas are the intended per-replica observability.",
	"semanticCache.StartSweeper": "IDEMPOTENT. DeleteStale is a single DELETE bounded by " +
		"`created_at < cutoff`. The second replica's sweep on the same tick affects zero rows — an " +
		"age predicate, not a top-N eviction, which is the distinction that matters here.",
	"cpSyncer.Run": "IN-PROCESS STATE. Rebuilds this replica's compression-policy cache; main.go " +
		"says so directly at the reload-interval comment — each replica must refresh its OWN cache.",
	"detector staleness": "PER-REPLICA METRIC. Publishes a gauge so a stalled detector is visible " +
		"between runs. Every replica should report its own liveness.",
	"stranded reservation sweep": "IDEMPOTENT BY ROW LOCK, and this one was checked hardest because " +
		"it MOVES MONEY. ReleaseStrandedReservations reads held ids and then refunds each — a " +
		"read-then-write two concurrent replicas both enter. ReleaseLXCReservation takes the row " +
		"`FOR UPDATE` inside a transaction and returns early when status != 'held', so the second " +
		"refund is a no-op. Its own comment says \"a double-sweep ... is a safe no-op\". Double " +
		"refund is impossible.",
	"localRouterMulti.CheckHealth": "PER-REQUEST. Started inside an HTTP handler.",
	"audit export POST":            "PER-REQUEST. Started inside an HTTP handler.",
	"agent schedule run": "IDEMPOTENT BY ROW LOCK, and it MOVES MONEY (B19.8). Each schedule tick is paid " +
		"in one transaction that holds the schedule's row FOR UPDATE SKIP LOCKED, writes the run row " +
		"(PRIMARY KEY schedule_id, tick_at) and advances next_run_at, so a second replica skips the locked " +
		"row or finds the tick already advanced; a duplicate run row would abort the payment with it. A " +
		"top-up locks the agent's row and re-reads its balance, so the second replica finds it above the " +
		"threshold. Measured by TestAgentRoutes_AWeeklyPaymentRunsOnceAWeekAndATopUpFiresOnce, which runs " +
		"two stores at once on one tick and pays once.",
	"scheduled prompt run": "CLAIMED BY ROW LOCK, and it MOVES MONEY (B28.125). Each due prompt schedule is claimed in " +
		"one transaction that holds its row FOR UPDATE SKIP LOCKED and advances next_run_at (or ends it) before the model " +
		"is asked, so a second replica skips the locked row or finds the time already moved on; the question is then " +
		"charged as the agent's own call is. Measured by TestB28125_AScheduledPromptIsAskedAtItsTimeOnTheAgentsWallet.",
	"ECB rate refresh": "IDEMPOTENT (B19.12). ecbRates.Refresh fetches the ECB's public file and inserts each " +
		"published rate ON CONFLICT (rate_date, currency) DO NOTHING — a second replica's refresh inserts zero " +
		"rows, and a published rate never changes. It moves no money: one GET every three hours per replica.",
	"room prize close": "IDEMPOTENT BY ROW LOCK (B32.35). ClosePrizes closes each prize past its deadline in a " +
		"transaction that holds its room's row FOR UPDATE and updates it only WHERE status = 'open', posting the " +
		"close message in the same transaction; a second replica's update matches no row and posts nothing. It " +
		"moves no money: a prize that closes charges nothing.",
	"rail probes": "READ-ONLY AND IN-PROCESS STATE (B37.2). WatchRails asks each partner one read-only question " +
		"(an account's details, a quote, a status, screening a fixed clean name): none sends or commits money or " +
		"opens anything, and the Test partners keep one entry for the fixed probe id however often it is asked. It " +
		"then refreshes this replica's own rails from partner_rails, whose upsert keeps the later of each time, so a " +
		"second replica's write changes nothing. Leader-gating it would leave every follower showing only its own calls.",
}

// perReplicaMatch maps a classification key to the CALL that identifies its goroutine: either the
// callee itself, or — for a closure — a call inside its body.
//
// ⚠ IT MAPPED TO A SUBSTRING OF THE `go` LINE UNTIL #528, AND THREE OF THE TEN WERE CLOSURES
// IDENTIFIED BY THE TEXT AFTER `go func(`. One was a COMMENT. And the "stranded reservation
// sweep" needle reduced, after the matcher split it on "\n" and kept index 0, to `go func() {` —
// which every anonymous goroutine in this file begins with, so any new one silently inherited
// that entry's money-path reason. A call is not a spelling; these are calls.
var perReplicaMatch = map[string]string{
	"watcher.ApplyLoop":            "watcher.ApplyLoop",
	"batchRouter.StartPoller":      "batchRouter.StartPoller",
	"sessionTracker.StartCleanup":  "sessionTracker.StartCleanup",
	"statusPage.StartCacher":       "statusPage.StartCacher",
	"l.StartBackground":            "l.StartBackground",
	"semanticCache.StartSweeper":   "semanticCache.StartSweeper",
	"cpSyncer.Run":                 "cpSyncer.Run",
	"localRouterMulti.CheckHealth": "localRouterMulti.CheckHealth",
	// The three closures, each identified by the one call that is its whole point.
	"detector staleness":         "patternDetectorHealth.PublishAge",
	"stranded reservation sweep": "dualToken.ReleaseStrandedReservations",
	"audit export POST":          "auditExporter.ExportWebhook",
	"agent schedule run":         "dualToken.RunAgentSchedules",
	"scheduled prompt run":       "dualToken.RunPromptSchedules",
	"ECB rate refresh":           "ecbRates.Refresh",
	"room prize close":           "roomStore.ClosePrizes",
	"rail probes":                "partnerRegistry.WatchRails",
}

// ⚠ THE GUARD. A goroutine that is neither leader-gated nor classified is one nobody has decided
// about, and the wrong answer is invisible until the fleet grows past one replica.
func TestEveryBackgroundGoroutineIsClassified(t *testing.T) {
	sites, err := scanGoStatements("main.go", []byte(readMainGo(t)))
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var gated, unclassified []string
	for _, g := range sites {
		if g.jobName != "" {
			gated = append(gated, g.jobName)
			continue
		}
		var hits []string
		for key, needle := range perReplicaMatch {
			if g.matches(needle) {
				if perReplica[key] == "" {
					t.Errorf("%s is classified per-replica with no reason", key)
				}
				hits = append(hits, key)
			}
		}
		switch len(hits) {
		case 0:
			what := g.callee
			if what == "" {
				what = "func literal calling " + strings.Join(g.calls, ", ")
			}
			unclassified = append(unclassified, fmt.Sprintf("main.go:%d %s", g.line, what))
		case 1:
			// classified
		default:
			sort.Strings(hits)
			t.Errorf("the goroutine at main.go:%d matches %d classifications (%s) — a goroutine "+
				"that answers to two reasons has been decided about by neither",
				g.line, len(hits), strings.Join(hits, ", "))
		}
	}

	// Non-vacuity: a parse that finds nothing satisfies every check above.
	if len(gated) < 30 {
		t.Fatalf("found %d leader-gated jobs, expected 30+ — the scan is broken, and a broken scan "+
			"reports every job as classified", len(gated))
	}
	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		t.Errorf("%d background goroutine(s) are neither leader-gated nor classified:\n  %s\n\n"+
			"    Wrap it in haComps.leader.Run if it touches SHARED state, is not idempotent, or "+
			"COSTS MONEY — on N replicas it runs N times.\n"+
			"    Or add it to perReplica with the measured property that makes running it "+
			"everywhere correct: in-process state, an age predicate, or a row lock. Say which.",
			len(unclassified), strings.Join(unclassified, "\n  "))
	}
	t.Logf("MEASURED: %d leader-gated singletons, %d classified per-replica.", len(gated), len(perReplica))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
