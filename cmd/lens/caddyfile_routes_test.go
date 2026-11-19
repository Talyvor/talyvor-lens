package main

import (
	"os"
	"strings"
	"testing"
)

// B14.1 — deploy/caddy/Caddyfile is the ONLY routing for talyvor.com and app.talyvor.com. It lived as an
// uncommitted edit on the production server, which made git refuse every later Caddyfile pull. It is in
// git now; this guard stops a later change from dropping either site block.

// caddySite returns the body of the top-level site block whose address line is exactly `host {`.
func caddySite(caddyfile, host string) (string, bool) {
	lines := strings.Split(caddyfile, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != host+" {" || strings.HasPrefix(l, "\t") || strings.HasPrefix(l, " ") {
			continue
		}
		var body []string
		for _, b := range lines[i+1:] {
			if b == "}" {
				return strings.Join(body, "\n"), true
			}
			body = append(body, strings.TrimPrefix(b, "\t"))
		}
	}
	return "", false
}

func publicRoutingProblems(caddyfile string) []string {
	var problems []string
	if body, ok := caddySite(caddyfile, "app.talyvor.com"); !ok || !strings.Contains(body, "reverse_proxy host.docker.internal:8787") {
		problems = append(problems, "app.talyvor.com must reverse_proxy host.docker.internal:8787 (the web app)")
	}
	if body, ok := caddySite(caddyfile, "talyvor.com"); !ok || !strings.Contains(body, "redir https://app.talyvor.com/marketing") {
		problems = append(problems, "talyvor.com must redir https://app.talyvor.com/marketing")
	}
	return problems
}

func TestCaddyfile_RoutesTheAppAndTheApexDomain(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/caddy/Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	caddyfile := string(raw)
	for _, p := range publicRoutingProblems(caddyfile) {
		t.Error(p)
	}
	// Control: the same check on this file with either block removed must fail.
	for _, host := range []string{"app.talyvor.com", "talyvor.com"} {
		body, ok := caddySite(caddyfile, host)
		if !ok {
			continue
		}
		without := strings.Replace(caddyfile, host+" {\n"+indentBack(body)+"\n}", "", 1)
		if without == caddyfile || len(publicRoutingProblems(without)) == 0 {
			t.Errorf("control: removing the %s block was not detected", host)
		}
	}
}

// indentBack re-tabs a block body the way the Caddyfile writes it (one more tab per line).
func indentBack(body string) string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		lines[i] = "\t" + l
	}
	return strings.Join(lines, "\n")
}

// B17.27 — a screen opened while lens or the app restarts waits for it instead of showing a 502: both
// proxied sites hold a request that cannot reach their upstream (lb_try_duration) until it is back.
func TestCaddyfile_WaitsForARestartingUpstreamInsteadOf502(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/caddy/Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"{$LENS_DOMAIN}", "app.talyvor.com"} {
		body, ok := caddySite(string(raw), host)
		if !ok {
			t.Errorf("%s: no site block", host)
			continue
		}
		if !strings.Contains(body, "reverse_proxy ") || !strings.Contains(body, "\tlb_try_duration 30s") {
			t.Errorf("%s: its reverse_proxy does not wait for a restarting upstream (lb_try_duration 30s)", host)
		}
	}
}

// B64.10 — talyvor.com's redirect keeps the query, so an outreach link's utm_ tags reach the marketing
// page. Caddy's {?query} is "?" plus the query, or nothing when there is none.
func TestCaddyfile_ApexRedirectKeepsTheQuery(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/caddy/Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := caddySite(string(raw), "talyvor.com")
	if !ok {
		t.Fatal("no talyvor.com site block")
	}
	if !strings.Contains(body, "redir https://app.talyvor.com/marketing{?query} 301") {
		t.Errorf("talyvor.com must redir to https://app.talyvor.com/marketing{?query} 301 so utm_ tags survive; got:\n%s", body)
	}
}
