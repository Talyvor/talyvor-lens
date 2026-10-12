package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// chart_image_reach_test.go — the FIRST thing a self-host evaluator runs is `helm install`, and
// until this file existed the chart's default render named an image that does not exist. Both
// halves were wrong at once and they fail DIFFERENTLY, which is why one boolean would have hidden
// the defect:
//
//	ghcr.io/talyvor/talyvor-lens:0.1.0   -> token endpoint 403  (the ORG is not anonymously
//	                                       pullable — `talyvor` is not a GitHub org at all)
//	ghcr.io/gaboracnicolai/talyvor-lens:0.1.0 -> manifest 404   (the org is right and PUBLIC, that
//	                                       TAG was simply never published)
//
// (Measured 2026-08-10. On 12 Oct 2026 the repositories moved to a new GitHub organisation,
// "Talyvor", and the image moved with them to ghcr.io/talyvor/talyvor-lens — see publishedImageRepo.)
//
// So "fix the organisation" alone yields ImagePullBackOff just the same, one HTTP status later.
// That is the specific progress-shaped no-op this file exists to prevent, and it is assertion
// (3) below.
//
// ── WHY AN OFFLINE TEST AND NOT A REGISTRY PROBE ──────────────────────────────────────────────
// The obvious guard is "render the chart, resolve every image". It cannot live here. `helm` is
// installed in NO talyvor-lens CI job (checked all five: go vet · migrations · go test ·
// golangci-lint · govulncheck · build), and a registry call would make `go test` depend on GHCR
// being reachable. So the split is:
//
//   - THIS FILE, offline: cross-check values.yaml / Chart.yaml / deploy/k8s/manifests.yaml against
//     .github/workflows/images.yaml, the in-repo source of truth for what is actually published.
//     Drift in EITHER direction goes red.
//   - `deploy/helm/lens/check-image-reach.py`, by hand: the real anonymous-pull probe, kept the way
//     the suite keeps its site-parity premise. It carries its own both-directions controls.
//
// ── WHY THE ORG IS A HARDCODED LITERAL ────────────────────────────────────────────────────────
// go.mod says `module github.com/talyvor/lens` — that is the Go import path, NOT the GHCR
// namespace, and it agrees with the very value that was once broken. Until 12 Oct 2026 images.yaml
// got the owner from `${{ github.repository_owner }}`, which only exists at Actions runtime, so
// nothing in this repo named the real owner and the literal below was measured against the live
// registry (see the probe script). That day the repository moved to the GitHub organisation whose
// login is "Talyvor"; Docker refuses capital letters in an image name, so images.yaml now writes
// the owner out in lowercase (`OWNER: talyvor`), and assertion (2) requires this literal and that
// one to agree.
const publishedImageRepo = "ghcr.io/talyvor/talyvor-lens"

// ── WHAT MAY APPEAR AS AN IMAGE IN values.yaml ────────────────────────────────────────────────
// Every image-bearing default is classified here, and assertion (5) requires the classification
// to match the file EXACTLY — a new image added without a row fails, and a row whose path
// disappears fails too. (A guard that only walks the file cannot see a deletion; a guard that only
// walks a pinned list cannot see an addition. This does both.)
//
// The inclusion rule, written down so the next person decides rather than guesses:
//
//	published        — this repo's CI builds and pushes it. MUST equal publishedImageRepo, and its
//	                   tag must be one images.yaml really publishes.
//	operatorSupplied — a PLACEHOLDER for a binary nothing in this repo publishes. MUST be
//	                   default-disabled, and MUST NOT sit under publishedImageRepo's org, because a
//	                   placeholder wearing the real org reads as something you can actually pull.
//	thirdParty       — someone else's image (postgres, pgbouncer). Not ours to publish or pin here.
type imageClass int

const (
	published imageClass = iota
	operatorSupplied
	thirdParty
)

var imageClassification = map[string]imageClass{
	// The gateway. Two of the default render's two image references resolve to this
	// (Deployment + the pre-upgrade migrate Job, which inherits it via migrations.image="").
	"image.repository": published,

	// Operator-run mining binaries. MEASURED 2026-08-10, both orgs, all three names: every one
	// returns token endpoint 403 — they do not exist under `talyvor` OR under the real owner, and
	// images.yaml publishes exactly one image (talyvor-lens), so nothing is coming. They are
	// placeholders and this file requires them to stay behind `nodes.enabled=false`.
	"nodes.node.image.repository":      operatorSupplied,
	"nodes.cachenode.image.repository": operatorSupplied,
	"nodes.embednode.image.repository": operatorSupplied,

	// Not ours.
	"backup.image":    thirdParty,
	"pgbouncer.image": thirdParty,

	// Empty means "use the gateway image" — assertion (4) pins that it stays empty or matches.
	"migrations.image": published,
}

// yamlScalars walks a plain block-style YAML file and returns dotted path -> scalar value for
// every `key: value` line. values.yaml contains no block scalars and no flow mappings outside
// comments (checked), so an indentation stack is exact here rather than approximate. Assertion (5)
// compares the derived path set to the pinned one, so any parser drift is loud, not silent.
// It returns mapping paths separately: `image:` is a MAPPING (image.repository lives under it)
// while `migrations.image: ""` is an empty SCALAR that means "inherit the gateway image". Both
// read as an empty value on their own line, and conflating them made assertion (5) demand a
// classification for four parent keys that are not images at all.
func yamlScalars(t *testing.T, raw string) (map[string]string, map[string]bool) {
	t.Helper()
	out := map[string]string{}
	mappings := map[string]bool{}
	type frame struct {
		indent int
		key    string
	}
	var stack []frame
	kv := regexp.MustCompile(`^(\s*)([A-Za-z0-9_.-]+):\s*(.*?)\s*$`)
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
			continue
		}
		m := kv.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		indent, key, val := len(m[1]), m[2], m[3]
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		path := key
		if len(stack) > 0 {
			parts := make([]string, 0, len(stack)+1)
			for _, f := range stack {
				parts = append(parts, f.key)
			}
			path = strings.Join(append(parts, key), ".")
		}
		if i := strings.Index(val, " #"); i >= 0 { // trailing comment
			val = strings.TrimSpace(val[:i])
		}
		val = strings.Trim(val, `"'`)
		if val == "" {
			stack = append(stack, frame{indent: indent, key: key})
		}
		out[path] = val
	}
	// A path is a MAPPING iff some other path is nested under it. Decided after the walk rather
	// than at push time, because an empty scalar is pushed too (we cannot know until we see
	// whether anything indents beneath it).
	for path := range out {
		if i := strings.LastIndex(path, "."); i >= 0 {
			mappings[path[:i]] = true
		}
	}
	return out, mappings
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRootT(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

func TestChartImagesAreActuallyPublished(t *testing.T) {
	values, mappings := yamlScalars(t, readRepoFile(t, "deploy/helm/lens/values.yaml"))
	chart, _ := yamlScalars(t, readRepoFile(t, "deploy/helm/lens/Chart.yaml"))
	workflow := readRepoFile(t, ".github/workflows/images.yaml")

	// ── (1) the gateway repository is the one CI pushes to ────────────────────────────────────
	gotRepo := values["image.repository"]
	if gotRepo != publishedImageRepo {
		t.Errorf("(1) values.yaml image.repository = %q, want %q.\n"+
			"    `helm install` pulls anonymously. %q is not anonymously pullable and a self-host\n"+
			"    evaluator gets ImagePullBackOff on the first command in the README.",
			gotRepo, publishedImageRepo, gotRepo)
	}

	// ── (2) …and the workflow still works the way that literal assumes ────────────────────────
	// Without this, (1) is a literal compared to a literal: someone could repoint images.yaml at a
	// different owner or image name and (1) would stay green while the chart went stale again.
	publishedOwner := strings.TrimPrefix(publishedImageRepo[:strings.LastIndex(publishedImageRepo, "/")], "ghcr.io/")
	ownerLine := regexp.MustCompile(`(?m)^\s*OWNER:\s*` + regexp.QuoteMeta(publishedOwner) + `\s*$`)
	if !ownerLine.MatchString(workflow) {
		t.Errorf("(2) images.yaml does not set `OWNER: %s`, the owner publishedImageRepo (%s) assumes.\n"+
			"    It is written out in lowercase on purpose: github.repository_owner is \"Talyvor\", and Docker\n"+
			"    refuses an image name with a capital letter. Re-measure with\n"+
			"    deploy/helm/lens/check-image-reach.py and update both literals together.",
			publishedOwner, publishedImageRepo)
	}
	wantName := gotRepo[strings.LastIndex(gotRepo, "/")+1:]
	if pushLine := fmt.Sprintf("ghcr.io/${{ env.OWNER }}/%s:latest", wantName); !strings.Contains(workflow, pushLine) {
		t.Errorf("(2) images.yaml does not push %q.\n"+
			"    The chart's default names an image this repo's CI does not build.", pushLine)
	}

	// ── (3) the default TAG must be one images.yaml really publishes ──────────────────────────
	// images.yaml pushes exactly two tags: the literal `latest`, and ${{ github.sha }}. It fires on
	// push-to-main only and is not tag-driven; `git tag` in this repo returns ZERO. So there is no
	// process by which a semver tag could exist, and `tag: ""` — documented as "defaults to the
	// chart appVersion" — resolves to 0.1.0, a tag NOTHING PUBLISHES. This assertion is the one
	// that makes an org-only fix insufficient.
	tag := values["image.tag"]
	sha40 := regexp.MustCompile(`^[0-9a-f]{40}$`)
	switch {
	case tag == "":
		t.Errorf("(3) values.yaml image.tag is empty, which the file documents as \"defaults to the\n"+
			"    chart appVersion\" = %q. images.yaml publishes ONLY `latest` and the 40-char commit\n"+
			"    sha; it is not tag-driven and this repo has no git tags, so %[1]q is never pushed and\n"+
			"    the render 404s. Set `latest` (with the reproducibility caveat) or pin a sha.",
			chart["appVersion"])
	case tag != "latest" && !sha40.MatchString(tag):
		t.Errorf("(3) values.yaml image.tag = %q. images.yaml publishes only `latest` and a 40-char\n"+
			"    commit sha, so this tag is not published and the render will 404.", tag)
	}

	// ── (4) the migrate Job must not name a second, unpublished image ─────────────────────────
	if mi := values["migrations.image"]; mi != "" && !strings.HasPrefix(mi, publishedImageRepo+":") {
		t.Errorf("(4) values.yaml migrations.image = %q — neither empty (inherit the gateway image)\n"+
			"    nor a tag of %s. The migrate Job is a pre-upgrade hook: an unpullable image there\n"+
			"    fails the whole install, not just one pod.", mi, publishedImageRepo)
	}

	// ── (5) every image in values.yaml is classified, in both directions ──────────────────────
	found := map[string]bool{}
	for path := range values {
		if mappings[path] {
			continue // `image:` the mapping, not `migrations.image: ""` the scalar
		}
		if strings.HasSuffix(path, "image") || strings.HasSuffix(path, "image.repository") {
			found[path] = true
		}
	}
	for path := range found {
		if _, ok := imageClassification[path]; !ok {
			t.Errorf("(5) values.yaml has image-bearing key %q with no row in imageClassification.\n"+
				"    Classify it (published / operatorSupplied / thirdParty) — an unclassified image\n"+
				"    is exactly how the gateway default went unpublished and unnoticed.", path)
		}
	}
	for path := range imageClassification {
		if !found[path] {
			t.Errorf("(5) imageClassification pins %q but values.yaml no longer has it.\n"+
				"    Remove the row, or restore the value — a stale pin makes this guard smaller\n"+
				"    than it reads.", path)
		}
	}

	// ── (6) placeholders must stay placeholders ───────────────────────────────────────────────
	publishedOrg := publishedImageRepo[:strings.LastIndex(publishedImageRepo, "/")]
	for path, class := range imageClassification {
		if class != operatorSupplied {
			continue
		}
		if v := values[path]; strings.HasPrefix(v, publishedOrg+"/") {
			t.Errorf("(6) %s = %q sits under %q, the org whose images this repo really publishes —\n"+
				"    so it reads as pullable. images.yaml publishes ONE image (%s) and nothing\n"+
				"    builds this one. Keep placeholders out of the real namespace.",
				path, v, publishedOrg, publishedImageRepo)
		}
	}
	if values["nodes.enabled"] != "false" {
		t.Errorf("(7) values.yaml nodes.enabled = %q, want \"false\". The node images are placeholders\n"+
			"    for binaries nothing publishes (measured: token 403 under both orgs). Default-on\n"+
			"    would put an unpullable image in every default install.", values["nodes.enabled"])
	}
}

// TestGeneratedManifestsMatchTheChart guards SEAM #2. deploy/k8s/manifests.yaml is `helm template`
// output committed to the repo for people who apply raw manifests instead of installing the chart,
// and its own header says "Helm is the source of truth; do not edit by hand. Regenerate after any
// chart change." NOTHING ENFORCED THAT. It had not been regenerated across ELEVEN chart commits,
// so the broken image reference lived in two files and a fix to the chart alone would have left
// the second one broken and green.
//
// WHAT THIS DELIBERATELY DOES NOT CHECK: full regeneration equality. That needs `helm template`,
// and helm is not installed in CI (see the header above). This asserts the two properties that
// actually bit — the image references and the chart/app version stamped into the labels — and
// says so rather than implying a completeness it does not have.
func TestGeneratedManifestsMatchTheChart(t *testing.T) {
	values, _ := yamlScalars(t, readRepoFile(t, "deploy/helm/lens/values.yaml"))
	chart, _ := yamlScalars(t, readRepoFile(t, "deploy/helm/lens/Chart.yaml"))
	manifests := readRepoFile(t, "deploy/k8s/manifests.yaml")

	tag := values["image.tag"]
	if tag == "" {
		tag = chart["appVersion"]
	}
	want := fmt.Sprintf("%s:%s", values["image.repository"], tag)

	imageLine := regexp.MustCompile(`(?m)^\s*image:\s*"?([^"\s]+)"?\s*$`)
	var got []string
	for _, m := range imageLine.FindAllStringSubmatch(manifests, -1) {
		got = append(got, m[1])
	}
	if len(got) == 0 {
		t.Fatal("no `image:` line found in deploy/k8s/manifests.yaml — the regex reads nothing, so " +
			"every assertion below would pass vacuously")
	}
	sort.Strings(got)
	for _, g := range got {
		if g != want {
			t.Errorf("deploy/k8s/manifests.yaml renders image %q, the chart's default is %q.\n"+
				"    Run `make k8s-manifests` and commit the result — the file is generated and its\n"+
				"    header says so, but nothing checked it until now.", g, want)
		}
	}

	for _, want := range []string{
		fmt.Sprintf("helm.sh/chart: lens-%s", chart["version"]),
		fmt.Sprintf(`app.kubernetes.io/version: "%s"`, chart["appVersion"]),
	} {
		if !strings.Contains(manifests, want) {
			t.Errorf("deploy/k8s/manifests.yaml does not contain %q — it was generated from a\n"+
				"    different Chart.yaml. Run `make k8s-manifests`.", want)
		}
	}
}
