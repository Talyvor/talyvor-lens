package main

// provision_visibility_test.go — "fails closed" and "fails silently" are the same thing when
// nobody looks.
//
// Provisioning fails closed by design: no LENS_PROVISION_SECRET ⇒ POST /v1/provision is not
// registered ⇒ 404. That is the right posture — an unconfigured deployment must not expose an
// unauthenticated provisioning surface. But it is INVISIBLE. A Lens paired with a suite BFF that
// expects provisioning will boot clean, pass its healthcheck, serve every other route, and 404
// every login, with nothing anywhere saying why. That is a broken deploy wearing a green
// healthcheck.
//
// TWO GUARDS, one for each half of how that happens:
//
//  1. THE SECRET MUST REACH THE PROCESS. It was absent from docker-compose.yaml entirely, so
//     setting it in .env did nothing at all — the container never saw it. A config value the
//     compose file does not pass is indistinguishable, from inside, from one nobody set.
//  2. THE BOOT MUST SAY SO. When the secret is unset, run() logs a WARN naming the variable and
//     the consequence, so the one person who is looking — whoever is watching `docker compose
//     logs lens` during the deploy — sees it at the moment it matters.

import (
	"os"
	"strings"
	"testing"
)

// 1. The compose file must pass LENS_PROVISION_SECRET into the lens service.
//
// This is a call-site guard, not a behavioural one, and deliberately so: no test that runs inside
// the process can observe a variable the compose file failed to forward. The absence is only
// visible from outside, in the file that does the forwarding.
func TestComposePassesProvisionSecretToLens(t *testing.T) {
	raw, err := os.ReadFile("../../docker-compose.yaml")
	if err != nil {
		t.Fatalf("read docker-compose.yaml: %v", err)
	}
	compose := string(raw)

	if !strings.Contains(compose, "LENS_PROVISION_SECRET") {
		t.Fatal("docker-compose.yaml never mentions LENS_PROVISION_SECRET — setting it in .env " +
			"cannot reach the container, so Lens boots healthy, leaves POST /v1/provision " +
			"unregistered, and every login 404s with nothing in the logs. Add " +
			"`- LENS_PROVISION_SECRET=${LENS_PROVISION_SECRET:-}` to the lens service's environment.")
	}

	// It must be forwarded with the `:-` default, like its siblings: unset ⇒ empty ⇒ route
	// unregistered, which is the existing fail-closed behaviour. A `:?` would make an
	// unconfigured deployment refuse to start, which is a different (and wrong — see below)
	// decision, taken accidentally by punctuation.
	if !strings.Contains(compose, "LENS_PROVISION_SECRET=${LENS_PROVISION_SECRET:-}") {
		t.Errorf("LENS_PROVISION_SECRET is mentioned but not forwarded as " +
			"`${LENS_PROVISION_SECRET:-}`; it must reach the lens service with an empty default")
	}

	// And it must sit in the LENS SERVICE, not only in the one-shot migrate service (which has
	// its own tiny environment block and never serves traffic).
	lensStart := strings.Index(compose, "\n  lens:")
	if lensStart < 0 {
		t.Fatal("no `lens:` service found in docker-compose.yaml")
	}
	rest := compose[lensStart+1:]
	if next := strings.Index(rest, "\n  "); next > 0 {
		// bound the search to the lens service block: up to the next top-level service key
		for _, marker := range []string{"\n  pgbouncer:", "\n  postgres:", "\n  migrate:", "\n  redis:", "\n  caddy:", "\n  nats:"} {
			if i := strings.Index(rest, marker); i > 0 && i < next+len(rest) {
				if i < len(rest) {
					rest = rest[:i]
				}
			}
		}
	}
	if !strings.Contains(rest, "LENS_PROVISION_SECRET") {
		t.Errorf("LENS_PROVISION_SECRET appears in docker-compose.yaml but NOT in the lens " +
			"service's own environment — the serving process is the one that needs it")
	}
}

// 2. Booting without the secret must say so, at WARN, naming the variable and the consequence.
//
// Checked structurally over run(), in the style of the sibling wiring guards (audit_wiring_test.go,
// TestEconomyKillSwitch_WorkersGuarded): run() needs a live database, Redis and NATS, so it cannot
// be invoked here — but the presence of the warning beside the mount is exactly what would be
// deleted by a refactor, and that is what this pins.
func TestBootWarnsWhenProvisioningIsUnconfigured(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	s := string(src)

	mount := strings.Index(s, "mountProvisionRoute(limited, cfg.ProvisionSecret")
	if mount < 0 {
		t.Fatal("mountProvisionRoute call not found — this guard has drifted from the code it protects")
	}

	// The warning must be adjacent to the mount: near enough that someone changing one sees the
	// other. 1200 bytes is the size of the surrounding comment block plus the call.
	lo := mount - 1200
	if lo < 0 {
		lo = 0
	}
	hi := mount + 400

	// ⚠ THE WINDOW IS UNCHANGED AND DELIBERATE — 1200 bytes back, 400 forward, "near enough that
	// someone changing one sees the other". What changed in #525 is WHAT COUNTS INSIDE IT.
	// strings.Contains counted a COMMENT: deleting the warning and leaving its text in a comment
	// beside the mount left this green, and the guard's own message says the cost — provisioning
	// fails closed and silently, so a paired BFF 404s every login while Lens looks healthy. A real
	// CALL and a real STRING LITERAL are now required; the offsets come from the AST, so the byte
	// window keeps exactly the meaning it had.
	warnAt, err := callOffsets("main.go", src, "logger.Warn")
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	nameAt, err := stringLiteralOffsets("main.go", src, "LENS_PROVISION_SECRET")
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	if len(warnAt) == 0 {
		t.Fatal("main.go contains no logger.Warn call at all — the scan is blind, not the file silent")
	}

	if !anyWithin(warnAt, lo, hi) {
		t.Error("run() does not log a WARN beside mountProvisionRoute. Provisioning fails CLOSED " +
			"when LENS_PROVISION_SECRET is unset, which is correct — but it is then completely " +
			"silent: a paired BFF 404s every login while Lens looks healthy. Boot must say so.")
	}
	if !anyWithin(nameAt, lo, hi) {
		t.Error("the warning beside mountProvisionRoute must NAME LENS_PROVISION_SECRET — a " +
			"warning that does not name the variable to set is a warning nobody can act on")
	}
}
