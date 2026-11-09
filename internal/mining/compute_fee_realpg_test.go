package mining

import (
	"context"
	"testing"
	"time"
)

// B56.20 — the compute fee. When a registered node serves another workspace's request, Talyvor keeps
// compute_take_bps of what the node is paid, as its own ledger row: the node's held mint is the earning less
// the fee, and Talyvor's fee is held beside it in the same transaction and settles with it.

func TestComputeFee_NodeServedRequestBooksTalyvorsRow_Integration(t *testing.T) {
	pool := trafficHeldHarness(t, "node-owner")
	ctx := context.Background()
	ledger := NewLedgerStore(pool)
	ledger.SetMintVerifier(nodeMintVerifier{verified: map[string]bool{"node-owner": true}})
	miner := NewComputeMiner(ledger, pool)
	miner.SetHoldbackWindow(time.Millisecond)
	miner.SetComputeFee(500, "talyvor")
	if _, err := pool.Exec(ctx, `INSERT INTO inference_nodes (id, workspace_id, gpu_type, models) VALUES ('n1','node-owner','cpu','{}')`); err != nil {
		t.Fatal(err)
	}
	sum := func(ws, typ string) int64 {
		var n int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount),0) FROM lens_token_ledger WHERE workspace_id=$1 AND type=$2`, ws, typ).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	gross := EarningRate("cpu", 333) // 8325 µLENS: 5% is 416.25, so the fee rounds
	if gross != 8325 {
		t.Fatalf("gross=%d, want 8325", gross)
	}
	if err := miner.RecordServedRequest(ctx, "n1", "requester-ws", "creq-fee", 333, 5); err != nil {
		t.Fatalf("RecordServedRequest: %v", err)
	}
	// A replay of the same request books nothing more.
	if err := miner.RecordServedRequest(ctx, "n1", "requester-ws", "creq-fee", 333, 5); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := sum("node-owner", TypeComputeMineHeld); got != 7908 {
		t.Fatalf("node held mint=%d, want 7908 (8325 less the 417 fee)", got)
	}
	if got := sum("talyvor", TypeComputeFeeHeld); got != 417 {
		t.Fatalf("Talyvor's held fee=%d, want 417 (5%% of 8325, rounded up)", got)
	}

	time.Sleep(5 * time.Millisecond)
	if n, err := NewTrafficMintSweeper(pool, ledger).RunOnce(ctx); err != nil || n != 2 {
		t.Fatalf("sweep finalized n=%d err=%v, want 2 (the node's mint and Talyvor's fee)", n, err)
	}
	if got := sum("node-owner", TypeComputeMine); got != 7908 {
		t.Fatalf("node settled=%d, want 7908", got)
	}
	if got := sum("talyvor", TypeComputeFee); got != 417 {
		t.Fatalf("Talyvor settled fee=%d, want 417", got)
	}
	if s, err := ledger.GetTotalSupply(ctx); err != nil || s != gross {
		t.Fatalf("total supply=%d err=%v, want %d: the node's share and the fee together are the one earning", s, err, gross)
	}
}

func TestComputeFee_RoundingNeverOverpays(t *testing.T) {
	for _, bps := range []int64{0, 1, 333, 500, 9999, 10_000} {
		for _, gross := range []int64{1, 19, 20, 8325, 50_000, 1 << 40} {
			fee := ComputeFee(gross, bps)
			// Talyvor's fee is the exact share rounded up to the µLENS, so the node is paid gross − fee, never more.
			if fee*10_000 < gross*bps || (fee-1)*10_000 >= gross*bps || fee > gross {
				t.Fatalf("ComputeFee(%d, %d)=%d, want the share rounded up and no more than gross", gross, bps, fee)
			}
		}
	}
}

func TestComputeFee_NoFeeWhereNoNodeServed_Integration(t *testing.T) {
	pool := trafficHeldHarness(t, "node-owner")
	ctx := context.Background()
	ledger := NewLedgerStore(pool)
	ledger.SetMintVerifier(nodeMintVerifier{verified: map[string]bool{"node-owner": true}})
	miner := NewComputeMiner(ledger, pool)
	miner.SetComputeFee(500, "talyvor")
	if _, err := pool.Exec(ctx, `INSERT INTO inference_nodes (id, workspace_id, gpu_type, models) VALUES ('n1','node-owner','rtx4090','{}')`); err != nil {
		t.Fatal(err)
	}
	// The owner's own request on its own node, and a request that served no tokens, pay no node and no fee.
	if err := miner.RecordServedRequest(ctx, "n1", "node-owner", "creq-self", 1000, 5); err != nil {
		t.Fatal(err)
	}
	if err := miner.RecordServedRequest(ctx, "n1", "requester-ws", "creq-empty", 0, 5); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM lens_token_ledger`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("%d ledger rows, want 0: no node was paid, so nothing is owed to Talyvor", rows)
	}
}
