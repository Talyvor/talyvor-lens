package economy

import "testing"

// B17.564 — an approval is signed over the fees a payout batch states, the batch's and each row's, so a signature
// over the batch at one fee approves it at no other.
func TestPayoutBatchPurpose_BindsTheFees(t *testing.T) {
	batch := func(fee int64) PayoutBatch {
		return PayoutBatch{ID: "mpob_1", Funding: FundingTest, RowsTotal: 1, RowsValid: 1, TotalsMinor: map[string]int64{"GBP": 1000},
			FeesMinor: map[string]int64{"GBP": fee}, Rows: []PayoutRow{{Line: 1, PayeeID: "mpayee_1", AmountMinor: 1000, Currency: "GBP",
				Status: PayoutRowValid, FeeMinor: fee}}}
	}
	free, charged := batch(0), batch(20)
	if payoutBatchPurpose(free) == payoutBatchPurpose(charged) {
		t.Error("a batch at no fee and at 0.20 are signed over the same purpose")
	}
	rowOnly := batch(0)
	rowOnly.Rows[0].FeeMinor = 20
	if payoutBatchPurpose(rowOnly) == payoutBatchPurpose(free) {
		t.Error("a row's fee changed and the purpose did not")
	}
}
