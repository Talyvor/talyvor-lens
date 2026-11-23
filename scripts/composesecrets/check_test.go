// Package composesecrets runs scripts/check-compose-secrets.sh against the repo's own
// docker-compose.yaml with the testers' hostile change applied (B17.503).
package composesecrets

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// run copies compose into a fresh directory and runs the check on it.
func run(t *testing.T, compose string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", "../check-compose-secrets.sh", dir).CombinedOutput()
	return string(out), err
}

// B17.503: the nightly's hostile pull request `compose-secret-lens` turns the first
// ${POSTGRES_PASSWORD:?...} after LENS_DATABASE_URL= into ${POSTGRES_PASSWORD:-changeme}.
// That line carried a `# compose-secret-ok:` comment, which exempted the whole line, so the
// check stayed green. The same goes for DB_PASSWORD= on pgbouncer.
func TestHostileDefaultOnPostgresPasswordIsCaught(t *testing.T) {
	raw, err := os.ReadFile("../../docker-compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	compose := string(raw)
	if out, err := run(t, compose); err != nil {
		t.Fatalf("the check is red on main's docker-compose.yaml, so its red on the change proves nothing:\n%s", out)
	}
	for _, line := range []string{"LENS_DATABASE_URL", "DB_PASSWORD"} {
		re := regexp.MustCompile(`(` + line + `=[^\n]*?)\$\{POSTGRES_PASSWORD:\?[^}]*\}`)
		hostile := re.ReplaceAllString(compose, "${1}$${POSTGRES_PASSWORD:-changeme}")
		if hostile == compose {
			t.Fatalf("the hostile change no longer applies to the %s= line of docker-compose.yaml", line)
		}
		out, err := run(t, hostile)
		if err == nil {
			t.Errorf("%s=: the check passed ${POSTGRES_PASSWORD:-changeme}:\n%s", line, out)
			continue
		}
		if !strings.Contains(out, "secret-shaped variable 'POSTGRES_PASSWORD'") {
			t.Errorf("%s=: the check went red without naming POSTGRES_PASSWORD:\n%s", line, out)
		}
	}
}
