package povi

import (
	"context"
	"errors"
	"log/slog"

	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/mining"
)

// Minter is the minimal ledger-credit surface MintFromReceipt needs. The real
// *mining.LedgerStore satisfies this signature exactly, so PoVI stays decoupled
// from the ledger package (and tests pass a fake minter). amount is µLENS (SEC-2).
type Minter interface {
	Credit(ctx context.Context, workspaceID string, amount int64, txType, description string, metadata map[string]interface{}) error
}

// TypeReceiptMineProvisional tags a provisional, receipt-based LENS mint in the
// ledger. The "_provisional" suffix is deliberate: this credit path is still
// deterred only by stake + challenge (Parts 2/3), and is gated off by default.
const TypeReceiptMineProvisional = "receipt_mine_provisional"

// ReceiptMineRate is the provisional per-1k-output-token rate in µLENS (SEC-2),
// mirroring the existing ComputeMineBaseRate. It only matters if the unsafe
// minting flag is enabled (default off), so it is academic in normal operation.
const ReceiptMineRate int64 = 50_000 // 0.050 LENS

// ServedMeasurement is Lens's OWN record of what a node served for one request:
// the node Lens dispatched the request to, and the gateway-measured output-token
// count. This — never the node's claimed OutputTokens — is the mint basis. See
// MeasurementLookup and MintFromReceipt.
type ServedMeasurement struct {
	NodeID       string
	OutputTokens int
}

// MeasurementLookup resolves a request_id to Lens's gateway measurement of the
// served request. A (nil, nil) return means Lens has NO record of that request
// — the mint then fails closed. Injected so povi stays decoupled from the proxy
// measurement store (the concrete MeasurementStore in measurement.go satisfies it).
type MeasurementLookup interface {
	ServedMeasurement(ctx context.Context, requestID string) (*ServedMeasurement, error)
}

var (
	// ErrNoServedMeasurement is returned when Lens has no gateway measurement for
	// the receipt's request_id — i.e. a receipt for work Lens never served (or a
	// node-asserted request_id Lens never dispatched). Fail closed: mint nothing.
	ErrNoServedMeasurement = errors.New("povi: no gateway measurement for request, so the receipt mints nothing (fail closed)")
	// ErrMeasurementNodeMismatch is returned when the measured request was served
	// by a DIFFERENT node than the receipt claims — a node naming a request it did
	// not serve. Fail closed: mint nothing.
	ErrMeasurementNodeMismatch = errors.New("povi: receipt node did not serve the measured request, so it mints nothing")
)

// ProvisionalMintAmountTokens is the µLENS a mint of `outputTokens` WOULD credit
// (rate × tokens / 1000, rounded DOWN — SEC-2). It is the ONLY amount helper:
// MintFromReceipt feeds it the gateway-MEASURED token count, never a node claim.
// There is deliberately NO Receipt-taking variant — a helper that computes an
// amount from a node's unverified r.OutputTokens is the exact shape of the bug
// this layer closes, and a "for display only" comment is not a real guard. Any
// "would mint X" readout must call this over a MEASURED count.
func ProvisionalMintAmountTokens(outputTokens int) int64 {
	return mining.MulFloor(ReceiptMineRate, float64(outputTokens)/1000.0)
}

// MintFromReceipt is the GATED, provisional receipt→LENS mint, priced on Lens's
// OWN gateway measurement of the request — never on the node's claimed
// OutputTokens (that field is signed + audited, but a node controls it, so it
// must not reach the amount).
//
//   - enabled == false (THE DEFAULT): mint NOTHING. No lookup, no ledger write,
//     no metric. Returns minted=false. Callers still verify + record the receipt
//     for audit; they just don't credit LENS.
//   - enabled == true (test-only / explicitly opted in via
//     LENS_POVI_MINTING_ENABLED): resolve r.RequestID to Lens's served
//     measurement and require that THIS node served THAT request:
//     · no measurement (or no lookup wired) ⇒ ErrNoServedMeasurement, mint nothing.
//     · measurement bound to another node   ⇒ ErrMeasurementNodeMismatch, mint nothing.
//     · otherwise price on the MEASURED output tokens, log a loud provisional
//     warning, credit LENS, and bump the provisional-mint metric.
//
// This function NEVER asserts the receipt proves honest computation — it is
// attestation + tamper-evidence only. What it now guarantees is that a mint is
// bounded by what Lens actually served, so a fabricated OutputTokens mints nothing extra.
func MintFromReceipt(ctx context.Context, minter Minter, measure MeasurementLookup, r Receipt, enabled bool) (minted bool, amount int64, err error) {
	if !enabled {
		// Default, safe behavior: do not mint on receipt-alone.
		return false, 0, nil
	}
	// Price on what LENS MEASURED. Resolve the receipt's request_id to Lens's own
	// served record; NO RECORD ⇒ NO MINT (fail closed).
	if measure == nil {
		return false, 0, ErrNoServedMeasurement
	}
	m, err := measure.ServedMeasurement(ctx, r.RequestID)
	if err != nil {
		return false, 0, err
	}
	if m == nil {
		return false, 0, ErrNoServedMeasurement
	}
	// Binding: the receipt's node must be the node Lens dispatched this request to.
	if m.NodeID != r.NodeID {
		return false, 0, ErrMeasurementNodeMismatch
	}
	amount = ProvisionalMintAmountTokens(m.OutputTokens) // MEASURED, never r.OutputTokens (the claim)
	slog.Warn("povi: PROVISIONAL minting is ENABLED — priced on the gateway MEASUREMENT (not the node's claim); "+
		"still deterred only by stake + challenge (Parts 2/3), and a receipt is attestation + tamper-evidence, "+
		"NOT proof of honest computation",
		slog.String("node_id", r.NodeID),
		slog.String("workspace_id", r.WorkspaceID),
		slog.Int64("amount_ulens", amount),
		slog.String("tx_type", TypeReceiptMineProvisional))
	metrics.POVIProvisionalMint()
	if err := minter.Credit(ctx, r.WorkspaceID, amount, TypeReceiptMineProvisional,
		"provisional receipt mint (priced on gateway measurement; still stake/challenge-deterred)",
		map[string]interface{}{
			"node_id":                r.NodeID,
			"request_id":             r.RequestID,
			"claimed_output_tokens":  r.OutputTokens,
			"measured_output_tokens": m.OutputTokens,
			"provisional":            true,
		}); err != nil {
		return false, amount, err
	}
	return true, amount, nil
}
