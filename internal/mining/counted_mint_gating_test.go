package mining

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// counted_mint_gating_test.go — EVERY LENS THAT ENTERS SUPPLY MUST HAVE BEEN
// GATED SOMEWHERE.
//
// mint_gate.go's header claims the verified-to-earn gate "covers all mint paths
// at once and a new mint track CANNOT skip it". That claim is about a set
// (mintTypeList) and it is checkable against a DIFFERENT set that answers the
// other half of the question: countedSupplyTypeList, the types GetTotalSupply
// counts as minted LENS. Between them the invariant is:
//
//	If supply counts a type as newly minted LENS, then the moment that LENS
//	first accrued to the earner ran through the U6 floor and the U6 rate cap.
//
// The two lists are DELIBERATELY unequal (see both their docs) — supply counts
// at SETTLEMENT, the gate fires at the MINT MOMENT, which for most tracks is an
// earlier `_held` row. So the invariant cannot be "the lists match"; it needs a
// declaration of WHICH gated moment each counted type settles, and then an
// OBSERVATION that the declared moment is really gated.
//
// ⚠ WHY THIS IS OBSERVED AND NOT READ. Asking `IsMintType(x)` re-reads the very
// set whose completeness is in question, and a name rule ("x settles x_held")
// is the weak-instrument shape this repo has been bitten by before. So each
// declaration is checked by DRIVING THE REAL KERNELS with a recording verifier
// and a 1-µLENS rate cap, and reading back whether the controls actually fired.

// recordingVerifier ALLOWS every mint and records whether it was consulted.
// Allowing (rather than refusing) is deliberate: the observation is "did the
// kernel ask?", and refusing would abort the credit before the rate cap could
// be observed on the same call.
type recordingVerifier struct{ called bool }

func (r *recordingVerifier) MayEarn(ctx context.Context, tx pgx.Tx, workspaceID string) (bool, error) {
	r.called = true
	return true, nil
}

// controlObservation is what the two U6 money controls did for one txType at
// one kernel — measured, never inferred from the type's name or the set.
type controlObservation struct {
	verifierCalled bool // the U6 Sybil floor consulted the verifier
	capEnforced    bool // the U6 PR2 rolling-window rate cap refused the credit
}

func (o controlObservation) gated() bool { return o.verifierCalled && o.capEnforced }

var observeWSSeq int

// observeControls credits `txType` through one of the two mint kernels into a
// FRESH workspace, with the verifier wired and the rate cap set to 1 µLENS, and
// reports which controls fired. A mint-moment type must trip BOTH (the verifier
// is consulted, then 5 µLENS is refused against a 1 µLENS ceiling); a
// conservation/settlement type trips NEITHER and the credit lands.
func observeControls(t *testing.T, pool *pgxpool.Pool, txType string, heldKernel bool) controlObservation {
	t.Helper()
	ctx := context.Background()
	rec := &recordingVerifier{}
	store := NewLedgerStoreForTesting(pool)
	store.SetMintVerifier(rec)
	store.SetMintRateCap(1, 24*time.Hour)

	observeWSSeq++
	ws := fmt.Sprintf("ws_observe_%d", observeWSSeq)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var cerr error
	if heldKernel {
		cerr = store.CreditHeldTx(ctx, tx, ws, 5, txType, "gating observation", nil)
	} else {
		cerr = store.CreditTx(ctx, tx, ws, 5, txType, "gating observation", nil)
	}
	if cerr != nil {
		_ = tx.Rollback(ctx)
	} else if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if cerr != nil && !errors.Is(cerr, ErrMintRateCapExceeded) {
		t.Fatalf("observing %q (held=%v): unexpected error %v", txType, heldKernel, cerr)
	}
	return controlObservation{verifierCalled: rec.called, capEnforced: errors.Is(cerr, ErrMintRateCapExceeded)}
}

// ungatedMintMoment declares a counted-supply type whose LENS passes through NO
// gated mint moment at all. It is not a category — it is a KNOWN HOLE, and the
// test below proves it is still open, so it cannot quietly become a habit.
const ungatedMintMoment = ""

// mintMomentFor maps each type GetTotalSupply counts as minted LENS to the
// txType whose credit is that LENS's GATED mint moment. The map is a human
// declaration (like the counted list itself); every entry is then VERIFIED by
// observation, so a wrong declaration reds rather than documenting a fiction.
//
// Most tracks mint HELD and settle later, so the declared moment is the `_held`
// row. annotation_mine has no held stage — it is its own mint moment.
var mintMomentFor = map[string]string{
	TypeCacheMine:           TypeCacheMineHeld,
	TypeComputeMine:         TypeComputeMineHeld,
	TypeComputeFee:          TypeComputeMineHeld, // B56.20: the fee is part of the node's gated mint, booked in its tx
	TypeEmbeddingMine:       TypeEmbeddingMineHeld,
	TypeAnnotationMine:      TypeAnnotationMine, // no held stage: mint moment == settlement
	TypePatternMine:         TypePatternMineHeld,
	TypePoolRoyalty:         TypePoolRoyaltyHeld,
	TypeEvalContribution:    TypeEvalContributionHeld,
	TypeRoutingPrediction:   TypeRoutingPredictionHeld,
	TypeLatencyLocality:     TypeLatencyLocalityHeld,
	TypeConfidentialCompute: TypeConfidentialComputeHeld,

	// ⚠ THE ONE HOLE, MEASURED AND OPEN. stake_yield is the LENS computeYield()
	// creates at unstake — its own constant says "IT IS A MINT" — and supply
	// counts it. It has NO held stage and it is NOT in mintTypeList, so
	// MarketplaceStore.Unstake's CreditTx runs through applyTx with both U6
	// controls no-oping: measured, an unverified workspace mints yield through a
	// REFUSING verifier, and 999999 µLENS lands against a 5000 µLENS cap. Its
	// µLENS are also absent from the cap's own SUM (type = ANY(mintTypeList)), so
	// yield does not consume the headroom the next real mint is measured against.
	//
	// ⚠ IT IS NOT FIXED HERE BECAUSE THE ONE-LINE FIX STRANDS CUSTOMER PRINCIPAL.
	// Adding it to mintTypeList closes all three holes — and measured on real PG,
	// an unverified workspace's Unstake then fails ENTIRELY: wallet delta 0, the
	// stake position stays locked, because the yield credit's error rolls back the
	// same tx that returns the principal (deliberately — see Unstake's
	// loss-of-funds comment). "You cannot withdraw your own stake because you are
	// over your minting cap" is a product decision, not a session's repair.
	// Recorded in QUEUE.md W6.1 for that decision.
	TypeStakeYield: ungatedMintMoment,
}

// TestEveryCountedMintHasAGatedMintMoment is the completeness half of the U6
// floor: mintTypeList says what IS gated, this says what MUST BE.
func TestEveryCountedMintHasAGatedMintMoment(t *testing.T) {
	pool := u6TestPool(t)

	// (1) Every counted type must be DECLARED. An undeclared one is a new mint
	// track whose gating nobody was forced to think about — the exact way
	// stake_yield arrived.
	for _, counted := range countedSupplyTypeList {
		if _, ok := mintMomentFor[counted]; !ok {
			t.Errorf("counted-supply type %q has no entry in mintMomentFor.\n"+
				"Supply counts it as newly minted LENS, so say WHERE it was gated: name the txType "+
				"whose credit is its mint moment, or ungatedMintMoment if none exists (and then say why).", counted)
		}
	}

	// (2) No stale declarations — an entry for a type supply no longer counts is
	// a claim about money that nothing checks.
	counted := map[string]bool{}
	for _, c := range countedSupplyTypeList {
		counted[c] = true
	}
	for declared := range mintMomentFor {
		if !counted[declared] {
			t.Errorf("mintMomentFor declares %q, which is NOT in countedSupplyTypeList — stale entry", declared)
		}
	}

	// (3) THE OBSERVATION. Each declared mint moment must really be gated, at
	// BOTH kernels — that is precisely mint_gate.go's "placed in BOTH" claim.
	for _, c := range countedSupplyTypeList {
		moment, ok := mintMomentFor[c]
		if !ok {
			continue // already reported by (1)
		}
		if moment == ungatedMintMoment {
			// A declared hole. Prove it is STILL a hole: if either control has
			// since been armed, this entry is out of date and the exception must
			// be deleted rather than left standing as a false excuse.
			for _, held := range []bool{false, true} {
				got := observeControls(t, pool, c, held)
				if got.verifierCalled || got.capEnforced {
					t.Errorf("%q is declared ungatedMintMoment, but the %s kernel now gates it "+
						"(verifier=%v cap=%v). The hole is closed — delete the exception and declare the real mint moment.",
						c, kernelName(held), got.verifierCalled, got.capEnforced)
				}
			}
			continue
		}
		for _, held := range []bool{false, true} {
			got := observeControls(t, pool, moment, held)
			if !got.gated() {
				t.Errorf("counted mint %q declares its mint moment as %q, but the %s kernel does NOT gate %q "+
					"(verifier consulted=%v, rate cap enforced=%v).\n"+
					"Supply counts this LENS as minted while nothing checked the earner was verified or under their cap.",
					c, moment, kernelName(held), moment, got.verifierCalled, got.capEnforced)
			}
		}
	}
}

func kernelName(held bool) string {
	if held {
		return "held (CreditHeldTx/heldInner)"
	}
	return "spendable (CreditTx/applyTx)"
}
