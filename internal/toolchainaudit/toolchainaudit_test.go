package toolchainaudit

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ── the scan ─────────────────────────────────────────────────────────────────
//
// Every rule below reads THIS one scan. A rule that runs its own private walk
// can be satisfied by a walk that found nothing, so the population floors
// (TestToolchain_D_*) and the comparisons (A, B, C) must be talking about the
// same set of files or the floors guarantee nothing about the comparisons.

// pin is one `go-version:` input to one actions/setup-go step.
type pin struct {
	file    string // repo-relative workflow path
	line    int
	job     string // the job key it sits under, "" if it could not be attributed
	raw     string // the value as written, e.g. "1.26.6" or "1.25"
	major   int
	minor   int
	patch   int
	exact   bool // false when the pin names only major.minor, so setup-go may resolve it upward
	comment string
}

// repoRoot walks up from this package until it finds go.mod. Tests run in their
// own package directory, so the root is not the working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find go.mod above the test's working directory")
	return ""
}

var toolchainRE = regexp.MustCompile(`(?m)^toolchain\s+go(\d+)\.(\d+)\.(\d+)\s*(?://.*)?$`)

// modToolchain returns go.mod's `toolchain` directive — the version this
// repository SHIPS, because every setup-go pin below it is upgraded to it.
// It fails rather than returning a zero value: an empty version compared
// against an empty pin is equal, and rule A would pass over nothing.
func modToolchain(t *testing.T, root string) (string, [3]int) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	m := toolchainRE.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("FLOOR: go.mod declares no `toolchain goX.Y.Z` directive. " +
			"Every comparison in this file is against that version; without it they " +
			"would each compare a pin against the empty string and pass. If the " +
			"directive was deliberately removed, this guard has no subject and must " +
			"be deleted deliberately, not left to pass over nothing.")
	}
	v := [3]int{atoi(t, m[1]), atoi(t, m[2]), atoi(t, m[3])}
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]), v
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("parse version component %q: %v", s, err)
	}
	return n
}

var (
	jobRE        = regexp.MustCompile(`^  ([A-Za-z_][A-Za-z0-9_-]*):\s*$`)
	goVersionRE  = regexp.MustCompile(`^\s*go-version:\s*"?([0-9][0-9.]*)"?\s*(#.*)?$`)
	setupGoRE    = regexp.MustCompile(`uses:\s*actions/setup-go@`)
	goVersionFRE = regexp.MustCompile(`^\s*go-version-file:`)
)

type scan struct {
	pins          []pin
	setupGoSteps  int
	versionFiles  int
	workflowFiles []string
}

// scanWorkflows reads every workflow file once. It counts setup-go STEPS as
// well as go-version PINS: a setup-go step that takes its version some other
// way is a step this guard cannot speak for, and rule D refuses to let that
// population grow unnoticed rather than skipping it with a `continue`.
func scanWorkflows(t *testing.T, root string) scan {
	t.Helper()
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var s scan
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		rel := filepath.Join(".github", "workflows", name)
		s.workflowFiles = append(s.workflowFiles, rel)
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		job := ""
		for i, ln := range strings.Split(string(b), "\n") {
			if m := jobRE.FindStringSubmatch(ln); m != nil {
				job = m[1]
			}
			if setupGoRE.MatchString(ln) {
				s.setupGoSteps++
			}
			if goVersionFRE.MatchString(ln) {
				s.versionFiles++
			}
			m := goVersionRE.FindStringSubmatch(ln)
			if m == nil {
				continue
			}
			parts := strings.Split(m[1], ".")
			p := pin{file: rel, line: i + 1, job: job, raw: m[1], comment: strings.TrimSpace(m[2])}
			p.exact = len(parts) >= 3
			if len(parts) > 0 {
				p.major = atoi(t, parts[0])
			}
			if len(parts) > 1 {
				p.minor = atoi(t, parts[1])
			}
			if len(parts) > 2 {
				p.patch = atoi(t, parts[2])
			}
			s.pins = append(s.pins, p)
		}
	}
	sort.Slice(s.pins, func(i, j int) bool {
		if s.pins[i].file != s.pins[j].file {
			return s.pins[i].file < s.pins[j].file
		}
		return s.pins[i].line < s.pins[j].line
	})
	return s
}

func (p pin) String() string {
	return fmt.Sprintf("%s:%d (job %q) go-version %q", p.file, p.line, p.job, p.raw)
}

// ── D: the floors ────────────────────────────────────────────────────────────
//
// These run first in the file on purpose. Rules A, B and C are all comparisons
// over the scan, and every comparison over an empty scan passes.

// The counts are pinned, not asserted non-zero. "at least one pin exists" stays
// green while four of five disappear, and the four that vanished are exactly
// the jobs whose Go version stopped being checked.
const (
	wantSetupGoSteps = 8 // +1: ci.yaml's tare-model job (B27.35), +1 its mcp-sdk job (B40.1), Go 1.25 like the test job
	wantPins         = 8
	wantVersionFiles = 0
)

func TestToolchain_D1_EverySetupGoStepIsAccountedFor(t *testing.T) {
	root := repoRoot(t)
	s := scanWorkflows(t, root)

	if len(s.workflowFiles) == 0 {
		t.Fatal("FLOOR: found no workflow files at all — every rule below would pass over nothing")
	}
	if s.setupGoSteps != wantSetupGoSteps {
		t.Fatalf("FLOOR: found %d actions/setup-go steps across %v, want %d.\n"+
			"A step added or removed changes which jobs this guard speaks for. Re-count, "+
			"confirm the new step's Go version is intended, and move the pin.",
			s.setupGoSteps, s.workflowFiles, wantSetupGoSteps)
	}
	if len(s.pins) != wantPins {
		t.Fatalf("FLOOR: found %d go-version pins, want %d (across %v).\n"+
			"Rules A/B/C compare these pins; a shrinking population makes them pass for free.",
			len(s.pins), wantPins, s.workflowFiles)
	}
	// Every setup-go step must get its version from a pin this guard can read.
	// A step configured some other way is unverifiable BY CONSTRUCTION, which is
	// worse than a wrong pin because nothing here would mention it.
	if s.versionFiles != wantVersionFiles || s.setupGoSteps != len(s.pins) {
		t.Fatalf("FLOOR: %d setup-go steps but %d readable go-version pins (and %d go-version-file inputs, want %d).\n"+
			"A setup-go step whose version this guard cannot read is a step whose Go version "+
			"nothing in this repo checks against go.mod's toolchain. Decide what it should be "+
			"and make it readable, rather than letting it be skipped.",
			s.setupGoSteps, len(s.pins), s.versionFiles, wantVersionFiles)
	}
}

// ── A: the lockstep, which today is two comments ─────────────────────────────

// gradingJobs are the ci.yaml jobs whose whole purpose is to judge the shipped
// runtime. Their Go must BE the shipped runtime, not merely be near it.
var gradingJobs = []string{"lint", "vuln"}

func TestToolchain_A_TheGradingJobsPinExactlyGoModsToolchain(t *testing.T) {
	root := repoRoot(t)
	want, _ := modToolchain(t, root)
	s := scanWorkflows(t, root)

	byJob := map[string][]pin{}
	for _, p := range s.pins {
		byJob[p.job] = append(byJob[p.job], p)
	}
	for _, job := range gradingJobs {
		ps := byJob[job]
		if len(ps) == 0 {
			t.Fatalf("FLOOR: no go-version pin found under a job named %q.\n"+
				"This rule asserts that job's Go equals go.mod's toolchain; with the job "+
				"missing (or renamed) the assertion has no subject and would pass silently. "+
				"Found jobs with pins: %v", job, jobNames(byJob))
		}
		for _, p := range ps {
			if p.raw != want {
				t.Errorf("LOCKSTEP BROKEN: %s pins Go %q but go.mod's toolchain is go%s.\n"+
					"go.mod's toolchain is what SHIPS (the build job's setup-go pin is below it, so "+
					"the released binary is built with exactly that version). This job grades the "+
					"shipped runtime, so a different version here means it grades something nobody "+
					"builds with — and the dangerous direction is silent: pin this ABOVE go.mod's "+
					"toolchain and govulncheck goes GREEN over a binary still linked against the "+
					"vulnerable standard library.\nFix BOTH: go.mod's toolchain and %s.",
					p, p.raw, want, p.file)
			}
		}
	}
}

// ── B: nothing anywhere may sit above the floor ──────────────────────────────

func TestToolchain_B_NoWorkflowPinsAGoAboveGoModsToolchain(t *testing.T) {
	root := repoRoot(t)
	want, wv := modToolchain(t, root)
	s := scanWorkflows(t, root)

	checked := 0
	for _, p := range s.pins {
		if !p.exact {
			continue // rule C owns these
		}
		checked++
		if cmp3([3]int{p.major, p.minor, p.patch}, wv) > 0 {
			t.Errorf("PIN ABOVE THE SHIPPED RUNTIME: %s pins Go %q, above go.mod's toolchain go%s.\n"+
				"go.mod's toolchain is a FLOOR, not a pin: a setup-go version above it is NOT "+
				"pulled back down, so this job runs a Go the released binary is never built with.",
				p, p.raw, want)
		}
	}
	if checked == 0 {
		t.Fatalf("FLOOR: this rule compared 0 pins — every pin was major.minor only, or the scan "+
			"found none. A comparison over an empty set passes for free. Pins seen: %v", s.pins)
	}
}

// ── C: count what cannot be checked, never skip it ───────────────────────────

// A `go-version: "1.25"` pin names a minor series, and setup-go resolves it to
// the newest patch in that series — a moving target. It is only SAFE because
// 1.25.x is strictly below go.mod's minor, so any resolution of it is below the
// floor and gets upgraded to the floor. Once such a pin shares go.mod's
// major.minor that reasoning collapses and the pin becomes unverifiable from
// here. Those are COUNTED rather than skipped: without this, each one would
// fall through rule B's `continue` and the hardest-to-check pins would be the
// ones nothing mentions.
const wantUnverifiable = 0

func TestToolchain_C_TheUnverifiablePinsAreCountedNotSkipped(t *testing.T) {
	root := repoRoot(t)
	want, wv := modToolchain(t, root)
	s := scanWorkflows(t, root)

	var unverifiable []string
	inexact := 0
	for _, p := range s.pins {
		if p.exact {
			continue
		}
		inexact++
		// Strictly below go.mod's major.minor => every patch in the series is
		// below the floor => setup-go cannot resolve it above the shipped Go.
		if cmp3([3]int{p.major, p.minor, 0}, [3]int{wv[0], wv[1], 0}) < 0 {
			continue
		}
		unverifiable = append(unverifiable, p.String())
	}
	if inexact == 0 {
		// B17.268: since go.mod's go line is 1.26 every pin is exact (a 1.25 job refuses the module under
		// setup-go's GOTOOLCHAIN=local), so rule B checks them all and there is nothing for this one to count.
		t.Logf("0 major.minor pins: rule B checked every one of %d", len(s.pins))
		return
	}
	if len(unverifiable) != wantUnverifiable {
		t.Fatalf("UNVERIFIABLE PINS: %d, want %d.\n%s\n"+
			"These name a Go minor series at or above go.mod's toolchain go%s, so setup-go may "+
			"resolve them to a patch ABOVE the shipped runtime and nothing here can tell. Pin the "+
			"full major.minor.patch so rule B can check it.",
			len(unverifiable), wantUnverifiable, strings.Join(unverifiable, "\n"), want)
	}
}

func cmp3(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func jobNames(m map[string][]pin) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
