package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// B50.5: the Lens image is built from bases pinned by digest and runs as a non-root user.
func TestImagePinnedByDigestAndRunsAsNonRoot(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRootT(t), "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	pinned := regexp.MustCompile(`^FROM\s+\S+@sha256:[0-9a-f]{64}(\s|$)`)
	var froms int
	var user string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "FROM ") {
			froms++
			user = "" // USER does not carry across stages; only the last stage's counts
			if !pinned.MatchString(line) {
				t.Errorf("base image not pinned by digest: %s", line)
			}
		}
		if u, ok := strings.CutPrefix(line, "USER "); ok {
			user = strings.TrimSpace(u)
		}
	}
	if froms == 0 {
		t.Fatal("no FROM line found in Dockerfile")
	}
	if user == "" || user == "root" || user == "0" || strings.HasPrefix(user, "0:") || strings.HasPrefix(user, "root:") {
		t.Errorf("final stage must set USER to a non-root user, got %q", user)
	}
}
