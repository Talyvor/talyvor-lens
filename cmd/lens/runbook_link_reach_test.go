package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// runbook_link_reach_test.go — every `runbook_url` in deploy/observability/prometheus/alerts.yaml
// pointed at `https://github.com/talyvor/lens/...`, and all six 404. That link is the one thing an
// on-call engineer clicks at 3am with an alert firing, so a 404 there costs exactly the minutes it
// is most expensive to lose.
//
// ── WHAT WAS MEASURED, AND WHAT THAT MEASUREMENT FALSIFIED ────────────────────────────────────
// Census (not a sample) of all six, `curl -sIL -o /dev/null -w %{http_code}`, 2026-08-10:
//
//	https://github.com/talyvor/lens/blob/main/.../LensHighErrorRate.md              -> 404
//	https://github.com/gaboracnicolai/lens/blob/main/.../LensHighErrorRate.md       -> 404
//	https://github.com/gaboracnicolai/talyvor-lens/blob/main/.../LensHighErrorRate.md -> 200
//	https://github.com/gaboracnicolai/talyvor-lens/blob/main/.../NoSuchRunbook.md   -> 404
//
// The second row is the point. It was written down that "only the ORG is wrong" — it is not. The
// REPOSITORY NAME is wrong too: the repo is `talyvor-lens`, not `lens`. Correcting only the org
// yields another 404, one path segment later. That is the same progress-shaped no-op the chart's
// image reference had (see chart_image_reach_test.go), and assertion R1a below is what stops a
// half-correction from reading as a fix. The fourth row is the control in the other direction: the
// corrected prefix does NOT return 200 for an arbitrary filename, so the 200s above are the files
// and not a catch-all.
//
// ── WHY THE REAL REPO IS A HARDCODED LITERAL ──────────────────────────────────────────────────
// Nothing in this repo names it. `go.mod` says `module github.com/talyvor/lens` — the Go import
// path, which need not resolve and which agrees with the very value that was broken. Deriving the
// expectation from any in-repo file would compare the URL to something equally wrong, or to
// itself. The literal was measured against the live site above, and again on 12 Oct 2026, the day
// the repository moved from gaboracnicolai to the Talyvor organisation:
//
//	https://github.com/Talyvor/talyvor-lens/blob/main/.../LensHighErrorRate.md        -> 200
//	https://github.com/Talyvor/talyvor-lens/blob/main/.../NoSuchRunbook.md            -> 404
//	https://github.com/talyvor/lens/blob/main/.../LensHighErrorRate.md                -> 404 (no `lens` repo)
//	https://github.com/gaboracnicolai/talyvor-lens/blob/main/.../LensHighErrorRate.md -> 301 to the first row
const runbookURLPrefix = "https://github.com/Talyvor/talyvor-lens/blob/main/deploy/observability/runbooks/"

// The address this repository had until 12 Oct 2026. GitHub redirects it today, but a redirect is
// not a promise: it lasts only until someone creates a repository of the same name under
// gaboracnicolai again, and then every such link 404s with nothing in this repo having changed.
// R2 below turns that into a repo-wide rule.
const staleOwnerURLPrefix = "https://github.com/gaboracnicolai/"

// ── THE PINNED SET ────────────────────────────────────────────────────────────────────────────
// A guard that only walks alerts.yaml cannot see a deletion: delete an alert and every remaining
// runbook_url still checks out. So the six are pinned here and assertion R1c requires the file's
// set to equal this list EXACTLY — an alert added without a row fails, and a row whose alert
// disappears fails too.
var alertsWithRunbooks = []string{
	"LensHighErrorRate",
	"LensHighLatencyP99",
	"LensInstanceDown",
	"LensProviderDown",
	"LensRateLimitSpike",
	"LensTokenLedgerSlow",
}

const alertsFile = "../../deploy/observability/prometheus/alerts.yaml"

// TestAlertRunbookURLsResolve is R1: every runbook_url in alerts.yaml (a) carries the measured
// real-repo prefix, (b) names a runbook that exists in this repo, (c) the set of alerts is exactly
// the pinned six and each carries one link, and (d) each link names ITS OWN alert's runbook.
//
// The predicate for "this line is a link we ship" needs no exclusion list: a line whose first
// non-whitespace token is `runbook_url:` is a YAML mapping key. A YAML comment line begins with
// `#`, so it can never match. Prometheus renders values, never comments.
func TestAlertRunbookURLsResolve(t *testing.T) {
	raw, err := os.ReadFile(alertsFile)
	if err != nil {
		t.Fatalf("read %s: %v", alertsFile, err)
	}
	body := string(raw)

	// Premise, asserted rather than assumed: this file is the alert rules and it is not empty.
	// An empty or moved file would otherwise make every assertion below vacuously true.
	if !strings.Contains(body, "- alert: ") {
		t.Fatalf("%s contains no `- alert:` line — the file this guard reads is not the alert rules", alertsFile)
	}

	// Each link is attributed to the alert it sits under. The four assertions below are kept
	// SEPARABLE on purpose: "wrong prefix", "missing file", "alert set changed" and "link points at
	// somebody else's runbook" are four different mistakes, and a control that trips two of them at
	// once justifies neither.
	type link struct {
		line  int
		alert string
		url   string
	}
	var links []link
	var declaredAlerts []string
	current := ""
	for i, ln := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(ln)
		if name, ok := strings.CutPrefix(trimmed, "- alert: "); ok {
			current = strings.TrimSpace(name)
			declaredAlerts = append(declaredAlerts, current)
		}
		v, ok := strings.CutPrefix(trimmed, "runbook_url:")
		if !ok {
			continue
		}
		links = append(links, link{line: i + 1, alert: current, url: strings.Trim(strings.TrimSpace(v), `"'`)})
	}

	// R1c — the pinned set of ALERTS, both directions, read from the `- alert:` lines and not from
	// the URLs. Do this first: if an alert vanished, the per-link assertions below would be green
	// about the survivors and say nothing about the loss.
	have := append([]string(nil), declaredAlerts...)
	sort.Strings(have)
	want := append([]string(nil), alertsWithRunbooks...)
	sort.Strings(want)
	if strings.Join(have, ",") != strings.Join(want, ",") {
		t.Errorf("R1c: the set of alerts in %s changed\n  have: %v\n  want: %v\n"+
			"Update alertsWithRunbooks deliberately, and give any new alert a runbook in "+
			"deploy/observability/runbooks/.", alertsFile, have, want)
	}
	// Every declared alert must carry exactly one link. Stated separately so "six good links"
	// cannot cover for a seventh alert that ships with no runbook at all.
	if len(declaredAlerts) != len(links) {
		t.Errorf("R1c: %d alerts declared but %d runbook_url values — every alert must link a runbook\n"+
			"  alerts: %v", len(declaredAlerts), len(links), declaredAlerts)
	}

	for _, l := range links {
		// R1a — the full prefix, org AND repository name. `Talyvor/lens` and `gaboracnicolai/lens` are
		// measured 404, and the pre-move `gaboracnicolai/talyvor-lens` is only a redirect.
		// A wrong prefix makes the filename questions below unanswerable, hence the `continue`.
		if !strings.HasPrefix(l.url, runbookURLPrefix) {
			t.Errorf("R1a: %s:%d runbook_url does not resolve\n  have: %s\n  want prefix: %s\n"+
				"Correcting the ORG alone is not enough — the repository is `talyvor-lens`, not `lens`.",
				alertsFile, l.line, l.url, runbookURLPrefix)
			continue
		}
		name := strings.TrimPrefix(l.url, runbookURLPrefix)
		// R1b — the target exists in this repo. A correct prefix onto a missing file is still a 404.
		path := filepath.Join("../../deploy/observability/runbooks", name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("R1b: %s:%d links a runbook that does not exist in this repo\n  url:  %s\n  path: %s\n"+
				"The prefix is right, so this would 404 on the filename.", alertsFile, l.line, l.url, path)
		}
		// R1d — the link belongs to THIS alert. A URL that resolves to some other alert's runbook
		// is a 200 and reads as healthy; the on-call engineer opens the wrong document. Neither
		// R1a nor R1b can see it.
		if wantName := l.alert + ".md"; name != wantName {
			t.Errorf("R1d: %s:%d alert %q links %q — an existing runbook, but not its own\n"+
				"This resolves 200 and sends the on-call engineer to the wrong document.",
				alertsFile, l.line, l.alert, name)
		}
	}
}

// TestNoDeadOrgURLInShippedYAML is R2, the class rather than the instance.
//
// THE RULE, stated so it is a predicate and not an exclusion list: no YAML line that is not a
// whole-line comment may contain `https://github.com/gaboracnicolai/`, in any letter case — the
// address this repository had until it moved to the Talyvor organisation on 12 Oct 2026.
//
// (Until that day the rule pointed the other way: `https://github.com/talyvor/` was the dead
// prefix, because the `talyvor` organisation did not exist. It exists now and is where the
// repository lives. The original `talyvor/lens` links stay dead because the repository is
// `talyvor-lens`, and in alerts.yaml R1a is what sees that.)
//
// The rule needs no allowlist. Whole-line YAML comments fall outside it for a reason, not by name:
// Chart.yaml's comment quotes the old address *with* its scheme on purpose, to say where the links
// used to point, and comments are stripped before Helm or Prometheus ever sees the document, so
// they are not clickable in the product. THE COMMENT RULE IS LOAD-BEARING, NOT DECORATION: delete
// the `#` skip below and this test goes red on an unmodified tree (control C6).
func TestNoDeadOrgURLInShippedYAML(t *testing.T) {
	var scanned int
	var offenders []string

	err := filepath.WalkDir("../..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(d.Name()); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned++
		for i, ln := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(ln), "#") {
				continue // whole-line comment: never rendered, never clickable in the product
			}
			if strings.Contains(strings.ToLower(ln), staleOwnerURLPrefix) {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", path, i+1, strings.TrimSpace(ln)))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// The instrument must have read something. A walk that matched zero files would report a clean
	// repo forever — the failure mode this project keeps finding.
	if scanned < 10 {
		t.Fatalf("R2 read only %d YAML files — the scanner is not reading the repo", scanned)
	}
	// And it must be able to see the subject at all: alerts.yaml is in the walked set.
	if _, err := os.Stat(alertsFile); err != nil {
		t.Fatalf("R2's scan root does not contain %s: %v", alertsFile, err)
	}

	if len(offenders) > 0 {
		t.Errorf("R2: %d YAML value(s) point at github.com/gaboracnicolai, where this repository lived until it moved\n"+
			"to the Talyvor organisation on 12 Oct 2026 (those links only redirect, and only until the old name is reused):\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	t.Logf("R2 scanned %d YAML files", scanned)
}
