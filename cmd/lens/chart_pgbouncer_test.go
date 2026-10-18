package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// B39.8: the chart's PgBouncer must heal like compose's — probes that run a real query through
// the pooler (a tcpSocket probe passes on a wedged pooler, #174) and a short server_login_retry.
// The live proof (probe exits 0 through a healthy pooler, non-zero with Postgres down) needs
// helm and docker, which CI lacks; this pins the rendered contract that proof ran against.
func TestChartPgBouncerHealsLikeCompose(t *testing.T) {
	read := func(rel string) string {
		raw, err := os.ReadFile(filepath.Join(repoRootT(t), rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	deploy := read("deploy/helm/lens/templates/pgbouncer-deployment.yaml")
	if strings.Contains(deploy, "tcpSocket:") {
		t.Error("pgbouncer probes must not be tcpSocket: a wedged pooler still accepts TCP")
	}
	if n := strings.Count(deploy, `-tAc 'SELECT 1'`); n != 2 {
		t.Errorf("want liveness AND readiness to run SELECT 1 through the pooler, found %d", n)
	}
	cm := read("deploy/helm/lens/templates/pgbouncer-configmap.yaml")
	for _, want := range []string{"SERVER_LOGIN_RETRY: {{ .Values.pgbouncer.serverLoginRetry", "DB_HOST:", "LISTEN_PORT:"} {
		if !strings.Contains(cm, want) {
			t.Errorf("pgbouncer configmap missing %q (edoburu/pgbouncer's variables)", want)
		}
	}
	values := read("deploy/helm/lens/values.yaml")
	compose := read("docker-compose.yaml")
	for _, line := range strings.Split(compose, "\n") {
		if ref, ok := strings.CutPrefix(strings.TrimSpace(line), "image: edoburu/pgbouncer:"); ok {
			if !strings.Contains(values, `image: "edoburu/pgbouncer:`+ref+`"`) {
				t.Errorf("chart pgbouncer.image must be pinned as compose pins it: edoburu/pgbouncer:%s", ref)
			}
			if !strings.Contains(values, "serverLoginRetry: 3") {
				t.Error("pgbouncer.serverLoginRetry must default to compose's 3")
			}
			return
		}
	}
	t.Fatal("docker-compose.yaml has no edoburu/pgbouncer image line")
}
