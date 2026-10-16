package povi

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"
)

// ── test doubles ──

// honestProvider answers challenges truthfully from a retained trace (the node
// side: TraceCache.SampledPaths + the leaf values). tamper/failErr simulate a
// cheating / unreachable node.
type honestProvider struct {
	tc      *TraceCache
	steps   map[string][][]byte
	tamper  bool
	failErr error
}

func newHonestProvider(requestID string, steps [][]byte) *honestProvider {
	tc := NewTraceCache(time.Hour)
	tc.Put(requestID, steps)
	return &honestProvider{tc: tc, steps: map[string][][]byte{requestID: steps}}
}

func (p *honestProvider) FetchPaths(_ context.Context, _, _, requestID string, positions []int) ([]LeafProof, error) {
	if p.failErr != nil {
		return nil, p.failErr
	}
	proofs, err := p.tc.SampledPaths(requestID, positions)
	if err != nil {
		return nil, err
	}
	steps := p.steps[requestID]
	out := make([]LeafProof, len(positions))
	for i, pos := range positions {
		out[i] = LeafProof{Position: pos, Leaf: steps[pos], Proof: proofs[i]}
	}
	if p.tamper && len(out) > 0 {
		out[0].Leaf = []byte("FABRICATED") // breaks VerifyPath against the committed root
	}
	return out, nil
}

type slashCall struct {
	nodeID   string
	fraction float64
}

type fakeSlasher struct {
	mu     sync.Mutex
	calls  []slashCall
	amount int64
}

func (f *fakeSlasher) Slash(_ context.Context, nodeID string, fraction float64, _ string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, slashCall{nodeID, fraction})
	return f.amount, nil
}

type memChallengeStore struct {
	mu         sync.Mutex
	byID       map[string]Challenge
	challenged map[string]bool
}

func newMemChallengeStore() *memChallengeStore {
	return &memChallengeStore{byID: map[string]Challenge{}, challenged: map[string]bool{}}
}
func (s *memChallengeStore) Record(_ context.Context, c Challenge) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.challenged[c.RequestID] {
		return ErrAlreadyChallenged
	}
	s.challenged[c.RequestID] = true
	s.byID[c.ID] = c
	return nil
}
func (s *memChallengeStore) UpdateResult(_ context.Context, workspaceID, id string, result ChallengeResult, slashedAmount int64, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byID[id]
	// Confined to the owning workspace — mirrors the real store's WHERE id=$1 AND workspace_id=$2,
	// so a cross-workspace (id, workspace) pair is a no-op here too.
	if !ok || c.WorkspaceID != workspaceID {
		return nil
	}
	c.Result = result
	c.SlashedAmount = slashedAmount
	c.Reason = reason
	s.byID[id] = c
	return nil
}
func (s *memChallengeStore) Get(_ context.Context, id string) (*Challenge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byID[id]
	if !ok {
		return nil, nil
	}
	return &c, nil
}
func (s *memChallengeStore) AlreadyChallenged(_ context.Context, requestID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.challenged[requestID], nil
}
func (s *memChallengeStore) List(_ context.Context, _, _ string) ([]Challenge, error) {
	return nil, nil
}

func fixtureReceipt(requestID string, steps [][]byte) StoredReceipt {
	root := MerkleRoot(steps)
	return StoredReceipt{
		RequestID: requestID, NodeID: "node-1", WorkspaceID: "ws-op",
		MerkleRootHex: hex.EncodeToString(root[:]), Verified: true, LeafCount: len(steps),
	}
}

func steps(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = []byte{byte('a' + i%26), byte(i)}
	}
	return out
}

func newTestChallenger(t *testing.T, provider PathProvider) (*Challenger, *fakeSlasher, *memChallengeStore) {
	t.Helper()
	slasher := &fakeSlasher{amount: 50}
	store := newMemChallengeStore()
	urls := func(_ context.Context, _ string) (string, error) { return "http://node:9090", nil }
	c := NewChallenger(urls, provider, slasher, store, 4, 0.5)
	return c, slasher, store
}

// ── tests ──

// Valid paths (honest node, retained trace) → PASS, no slash.
func TestChallenge_ValidPathsPass(t *testing.T) {
	st := steps(20)
	rec := fixtureReceipt("req-1", st)
	c, slasher, store := newTestChallenger(t, newHonestProvider("req-1", st))

	ch, err := c.Challenge(context.Background(), rec)
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	if ch.Result != ChallengePass {
		t.Fatalf("result = %q, want pass", ch.Result)
	}
	if len(slasher.calls) != 0 {
		t.Errorf("a passing challenge must not slash, got %+v", slasher.calls)
	}
	if got, _ := store.Get(context.Background(), ch.ID); got == nil {
		t.Error("challenge should be recorded")
	}
}

// An invalid path (fabricated leaf) → FAIL → slash with the configured fraction.
func TestChallenge_InvalidPathFailsAndSlashes(t *testing.T) {
	st := steps(20)
	rec := fixtureReceipt("req-1", st)
	prov := newHonestProvider("req-1", st)
	prov.tamper = true
	c, slasher, _ := newTestChallenger(t, prov)

	ch, err := c.Challenge(context.Background(), rec)
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	if ch.Result != ChallengeFail {
		t.Fatalf("result = %q, want fail", ch.Result)
	}
	if len(slasher.calls) != 1 || slasher.calls[0].fraction != 0.5 || slasher.calls[0].nodeID != "node-1" {
		t.Fatalf("expected one slash of node-1 @ 0.5, got %+v", slasher.calls)
	}
	if ch.SlashedAmount != 50 {
		t.Errorf("slashed amount = %v, want 50", ch.SlashedAmount)
	}
}

// Node doesn't answer (timeout / unreachable) → treated as a failure → slash.
func TestChallenge_TimeoutFailsAndSlashes(t *testing.T) {
	st := steps(20)
	rec := fixtureReceipt("req-1", st)
	prov := newHonestProvider("req-1", st)
	prov.failErr = errors.New("connection refused")
	c, slasher, _ := newTestChallenger(t, prov)

	ch, err := c.Challenge(context.Background(), rec)
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	if ch.Result != ChallengeTimeout {
		t.Fatalf("result = %q, want timeout", ch.Result)
	}
	if len(slasher.calls) != 1 {
		t.Error("a non-answering node must be slashed (can't prove its work)")
	}
}

// Double-slash guard: the same receipt is challenged once; a re-challenge is
// refused (so one failure can't slash repeatedly).
func TestChallenge_DoubleSlashGuard(t *testing.T) {
	st := steps(20)
	rec := fixtureReceipt("req-1", st)
	prov := newHonestProvider("req-1", st)
	prov.tamper = true
	c, slasher, _ := newTestChallenger(t, prov)

	if _, err := c.Challenge(context.Background(), rec); err != nil {
		t.Fatalf("first challenge: %v", err)
	}
	_, err := c.Challenge(context.Background(), rec)
	if !errors.Is(err, ErrAlreadyChallenged) {
		t.Fatalf("second challenge should be refused, got %v", err)
	}
	if len(slasher.calls) != 1 {
		t.Errorf("the same failed receipt must slash exactly once, got %d", len(slasher.calls))
	}
}

// Position sampling: distinct, in range, and unpredictable (crypto/rand — not a
// deterministic function of public receipt fields).
func TestSamplePositions_DistinctInRangeUnpredictable(t *testing.T) {
	n, k := 100, 5
	a, err := samplePositions(n, k)
	if err != nil || len(a) != k {
		t.Fatalf("sample = %v, err %v", a, err)
	}
	seen := map[int]bool{}
	for _, p := range a {
		if p < 0 || p >= n {
			t.Errorf("position %d out of range", p)
		}
		if seen[p] {
			t.Errorf("duplicate position %d", p)
		}
		seen[p] = true
	}
	// Unpredictability: across several draws the sets should differ (crypto/rand).
	allSame := true
	for i := 0; i < 8; i++ {
		b, _ := samplePositions(n, k)
		if !sameSet(a, b) {
			allSame = false
			break
		}
	}
	if allSame {
		t.Error("samples are identical across draws — not using crypto/rand")
	}
	// k >= n returns all positions.
	all, _ := samplePositions(3, 5)
	if len(all) != 3 {
		t.Errorf("k>=n should return all %d positions, got %d", 3, len(all))
	}
}

// The trace TTL must comfortably exceed a challenge window so a retained trace
// can still answer a challenge issued shortly after the request.
func TestTraceTTL_CoversChallengeWindow(t *testing.T) {
	if DefaultTraceTTL < 5*time.Minute {
		t.Errorf("DefaultTraceTTL %v too short to cover a challenge window", DefaultTraceTTL)
	}
}

// Concurrent challenges over distinct receipts must be race-free (run -race).
func TestChallenger_Concurrent(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := "req-" + string(rune('a'+i))
			st := steps(15)
			rec := fixtureReceipt(id, st)
			c, _, _ := newTestChallenger(t, newHonestProvider(id, st))
			_, _ = c.Challenge(context.Background(), rec)
		}()
	}
	wg.Wait()
}

// TestChallenge_NoConcurrentDoubleSlash verifies that when many goroutines race
// to challenge the same receipt, exactly one slash fires. Without the
// atomic INSERT claim in Record, all goroutines that pass the AlreadyChallenged
// SELECT would each call Slash — the HA double-slash TOCTOU.
func TestChallenge_NoConcurrentDoubleSlash(t *testing.T) {
	st := steps(20)
	rec := fixtureReceipt("req-race", st)
	prov := newHonestProvider("req-race", st)
	prov.tamper = true // invalid paths → every challenger that gets through will slash

	slasher := &fakeSlasher{amount: 50}
	store := newMemChallengeStore()
	urls := func(_ context.Context, _ string) (string, error) { return "http://node:9090", nil }
	challenger := NewChallenger(urls, prov, slasher, store, 4, 0.5)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, _ = challenger.Challenge(context.Background(), rec)
		}()
	}
	wg.Wait()

	slasher.mu.Lock()
	slashCount := len(slasher.calls)
	slasher.mu.Unlock()

	if slashCount != 1 {
		t.Fatalf("expected exactly 1 slash, got %d — double-slash TOCTOU still present", slashCount)
	}
}

func sameSet(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[int]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if !m[x] {
			return false
		}
	}
	return true
}
