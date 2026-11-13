package poolsafety

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Gate holds the live cross-tenant pooling decision: whether the embedding configuration this
// process is running IS the one that last passed `lens poolcheck`.
//
// ⚠ WHY THIS IS RE-READ RATHER THAN DECIDED ONCE AT BOOT.
//
// The attestation lives in the database; the configuration it attests to lives in the process
// environment. Those have different lifetimes, and reading both once at boot creates a
// sequencing trap that silently disables pooling forever:
//
//	deploy:  docker compose up -d     → gateway boots, finds NO attestation → pooling off
//	deploy:  lens poolcheck           → passes, records the attestation
//	         ...nothing restarts the gateway, so it is still holding "off"
//
// That is not hypothetical — it is the order the full-stack runbook prescribes, because
// poolcheck needs a running container to exec into. A boot-only read means the very first
// deployment can never turn pooling on, and the failure is silent and looks like the feature
// simply not working.
//
// The alternative fix is a runbook step that restarts the gateway after poolcheck. That works
// on the scripted path and only there: it does nothing for an operator who runs poolcheck out
// of band later, and it puts a subtlety they cannot see into a document they may not follow.
// Re-reading makes the system self-correcting instead — run poolcheck whenever, and pooling
// follows within one refresh interval.
//
// What re-reading does NOT do, deliberately: the model and threshold are read from the process
// environment at boot and cannot change without a restart, so this only ever re-evaluates the
// DATABASE side of the comparison. That asymmetry is the correct one — config is boot-scoped,
// a shared mutable row is not.
type Gate struct {
	mu       sync.RWMutex
	attested bool
	reason   string
	// decided records whether any read has ever completed. Before the first successful read
	// the gate fails CLOSED; after one, a transient database error holds the last decision
	// rather than flapping pooling off and on with connection blips.
	decided bool
}

// ReasonUnreadable opens the reason when the attestation could not be read. The driver's error
// follows it in Reason(), which can name a host, so a reader outside the operator's sees this alone.
const ReasonUnreadable = "the pool-safety attestation could not be read"

// NewGate returns a gate that is closed and has never been evaluated. The zero value is
// deliberately the safe one: a Gate nobody refreshed reports not-attested.
func NewGate() *Gate {
	return &Gate{reason: "pool-safety attestation has not been read yet"}
}

// Attested reports whether cross-tenant pooling is currently justified. Safe for concurrent
// use; called on the request path via the pool gate.
func (g *Gate) Attested() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.attested
}

// Reason explains the current decision. Empty when pooling IS attested. This is what makes a
// forced-off state observable rather than merely safe — an operator seeing "pooling off" with
// no reason cannot tell it apart from pooling not being switched on.
func (g *Gate) Reason() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.attested {
		return ""
	}
	return g.reason
}

// Refresh re-evaluates the decision against the database. It returns true when the decision
// CHANGED, so the caller can log transitions rather than every tick.
//
// Fail-closed in every branch that has information; hold-last in the one that does not.
func (g *Gate) Refresh(ctx context.Context, db Reader, model string, threshold float64) (changed bool) {
	attested, reason, conclusive := g.evaluate(ctx, db, model, threshold)

	g.mu.Lock()
	defer g.mu.Unlock()
	if !conclusive && g.decided {
		// A transient read failure after a successful one. Holding the last decision is the
		// right call: flipping pooling off on a connection blip and back on a tick later is
		// churn that teaches operators the signal is noise, and the previous decision was
		// made from real data. At boot there is no previous decision, so this branch does
		// not apply and the gate stays closed.
		return false
	}
	changed = g.attested != attested || !g.decided
	g.attested, g.reason, g.decided = attested, reason, true
	return changed
}

func (g *Gate) evaluate(ctx context.Context, db Reader, model string, threshold float64) (attested bool, reason string, conclusive bool) {
	att, err := Load(ctx, db)
	switch {
	case errors.Is(err, ErrNoAttestation):
		return false, "no pool-safety attestation has ever been recorded for this database: " +
			"cross-tenant pooling is enabled in config but has never been measured safe here. " +
			"Run `lens poolcheck` with this deployment's env; it records the attestation on success, " +
			"and pooling turns on within one refresh interval without a restart.", true
	case err != nil:
		return false, fmt.Sprintf("%s: %v", ReasonUnreadable, err), false
	}
	if ok, why := att.MatchesLive(model, threshold); !ok {
		return false, "the live embedding configuration is NOT the one that passed poolcheck, so " +
			"the measurement that justified cross-tenant pooling no longer applies: " + why +
			". Re-run `lens poolcheck`; if it passes, pooling resumes without a restart.", true
	}
	return true, "", true
}
