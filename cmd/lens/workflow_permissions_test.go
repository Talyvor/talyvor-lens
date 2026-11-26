package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// B28.262: every workflow sets a top-level permissions block that grants no write, so a job that names
// no permissions of its own gets a read-only GITHUB_TOKEN rather than the repository default. A job that
// needs a write (images.yaml's packages: write) names it in its own block.
func TestEveryWorkflowDefaultsToReadOnlyToken(t *testing.T) {
	dir := filepath.Join(repoRootT(t), ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	topLevel := regexp.MustCompile(`^permissions:\s*(.*?)\s*(#.*)?$`)
	scope := regexp.MustCompile(`^\s+([a-z-]+):\s*([a-z-]+)\s*(#.*)?$`)
	seen := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		seen++
		lines := strings.Split(readRepoFile(t, filepath.Join(".github", "workflows", name)), "\n")
		found := false
		for i, line := range lines {
			m := topLevel.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			found = true
			switch inline := m[1]; inline {
			case "{}", "read-all":
				continue
			case "":
			default:
				t.Errorf("%s: top-level `permissions: %s` — want a block of read/none scopes, {} or read-all", name, inline)
				continue
			}
			for _, sub := range lines[i+1:] {
				if strings.TrimSpace(sub) == "" || strings.HasPrefix(strings.TrimSpace(sub), "#") {
					continue
				}
				s := scope.FindStringSubmatch(sub)
				if s == nil {
					break
				}
				if s[2] != "read" && s[2] != "none" {
					t.Errorf("%s: top-level permissions grant `%s: %s`; a write belongs in the job that needs it", name, s[1], s[2])
				}
			}
		}
		if !found {
			t.Errorf("%s has no top-level `permissions:` block, so its jobs get the repository's default token", name)
		}
	}
	if seen == 0 {
		t.Fatalf("no workflow files found in %s", dir)
	}
}
