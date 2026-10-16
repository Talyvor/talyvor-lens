package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/povi"
)

// ─── mocks for the povi Challenger (the slash decision) ───────────────

type countingSlasher struct {
	mu sync.Mutex
	n  int
}

func (s *countingSlasher) Slash(_ context.Context, _ string, fraction float64, _ string) (int64, error) {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return int64(fraction * 100), nil
}
func (s *countingSlasher) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.n }

// noopChallengeStore satisfies the (unexported) challengeStore interface structurally.
type noopChallengeStore struct{}

func (noopChallengeStore) Record(context.Context, povi.Challenge) error { return nil }
func (noopChallengeStore) UpdateResult(context.Context, string, string, povi.ChallengeResult, int64, string) error {
	return nil
}
func (noopChallengeStore) AlreadyChallenged(context.Context, string) (bool, error) {
	return false, nil
}
func (noopChallengeStore) Get(context.Context, string) (*povi.Challenge, error) { return nil, nil }
func (noopChallengeStore) List(context.Context, string, string) ([]povi.Challenge, error) {
	return nil, nil
}

// ─── fixtures ────────────────────────────────────────────────────────

// staleNode builds a node whose signer has a retained trace for reqID (so it CAN
// answer), pinned with a STALE Lens challenge key. If refetchTo is non-nil, it wires a
// reactive refetcher that serves refetchTo (simulating Lens's current key). Returns the
// node URL + the StoredReceipt Lens would challenge.
func staleNode(t *testing.T, reqID, output string, stalePinned, refetchTo ed25519.PublicKey) (string, povi.StoredReceipt) {
	t.Helper()
	srv := NewInferenceServer(fakeProvider{resp: InferResponse{Text: output}}, "", NodeConfig{})
	rs, _ := testSigner(t)
	rec := rs.sign(reqID, "m", 1, len([]rune(output)), output)
	srv.SetReceiptSigner(rs, nil)
	srv.SetChallengePubKey(stalePinned)
	if refetchTo != nil {
		srv.challengeRefetch = func(context.Context) (ed25519.PublicKey, error) { return refetchTo, nil }
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	sr := povi.StoredReceipt{
		RequestID: reqID, NodeID: "n1", WorkspaceID: "ws1",
		MerkleRootHex: hex.EncodeToString(rec.MerkleRoot[:]),
		LeafCount:     rec.LeafCount,
	}
	return ts.URL, sr
}

// challenge runs the REAL povi Challenger (signing with lensPriv) against the node and
// returns (slash count, result).
func challenge(t *testing.T, nodeURL string, rec povi.StoredReceipt, lensPriv ed25519.PrivateKey) (int, povi.ChallengeResult) {
	t.Helper()
	slasher := &countingSlasher{}
	client := povi.NewChallengeClient(lensPriv, 5*time.Second)
	lookup := func(context.Context, string) (string, error) { return nodeURL, nil }
	ch := povi.NewChallenger(lookup, client, slasher, noopChallengeStore{}, 3, 0.5)
	res, err := ch.Challenge(context.Background(), rec)
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	return slasher.count(), res.Result
}

// ─── 1. single-node rotation: RED slashes honest node, GREEN does not ─

func TestRotationRace_SingleNode(t *testing.T) {
	stalePub, _, _ := povi.GenerateNodeKey()         // the node's stale pinned Lens key
	lensPub, lensPriv, _ := ed25519.GenerateKey(nil) // Lens's CURRENT (rotated) key
	_ = lensPub

	// RED — no reactive refetch (today): stale node 401s → Lens reads timeout → SLASH.
	urlRED, recRED := staleNode(t, "r1", "héllo wörld", stalePub, nil)
	if n, result := challenge(t, urlRED, recRED, lensPriv); n != 1 || result != povi.ChallengeTimeout {
		t.Fatalf("RED: slashes=%d result=%q, want 1 / timeout (honest node wrongly slashed)", n, result)
	}

	// GREEN — reactive refetch serves Lens's current key: node re-pins → answers → pass.
	urlGREEN, recGREEN := staleNode(t, "r1", "héllo wörld", stalePub, lensPub)
	if n, result := challenge(t, urlGREEN, recGREEN, lensPriv); n != 0 || result != povi.ChallengePass {
		t.Fatalf("GREEN: slashes=%d result=%q, want 0 / pass (re-fetch saves the honest node)", n, result)
	}
}

// ─── 2. restart blast-radius: many stale nodes, none slashed with refetch ─

func TestRotationRace_RestartBlastRadius(t *testing.T) {
	stalePub, _, _ := povi.GenerateNodeKey()
	lensPub, lensPriv, _ := ed25519.GenerateKey(nil) // Lens's NEW ephemeral key after restart

	const nodes = 8

	// RED: every stale node is slashed by the post-restart challenge burst.
	redSlashed := 0
	for i := 0; i < nodes; i++ {
		url, rec := staleNode(t, "r1", "héllo wörld", stalePub, nil)
		if n, _ := challenge(t, url, rec, lensPriv); n > 0 {
			redSlashed++
		}
	}
	if redSlashed != nodes {
		t.Fatalf("RED: %d/%d stale nodes slashed, want all %d (mass wrongful slash)", redSlashed, nodes, nodes)
	}

	// GREEN: with reactive refetch, none are slashed.
	greenSlashed := 0
	for i := 0; i < nodes; i++ {
		url, rec := staleNode(t, "r1", "héllo wörld", stalePub, lensPub)
		if n, _ := challenge(t, url, rec, lensPriv); n > 0 {
			greenSlashed++
		}
	}
	if greenSlashed != 0 {
		t.Fatalf("GREEN: %d/%d nodes slashed, want 0 (no node wrongly slashed after restart)", greenSlashed, nodes)
	}
}

// ─── 3. deterrent intact: attacker not rescued + both provable slashes ─

// 3a — a forged challenge (signed by a non-Lens key) is still refused 401 even after
// the node re-fetches Lens's REAL key.
func TestRotationRace_ForgedChallengeStill401(t *testing.T) {
	stalePub, _, _ := povi.GenerateNodeKey()
	lensRealPub, _, _ := ed25519.GenerateKey(nil) // Lens's real key (served by refetch)
	_, forgerPriv, _ := ed25519.GenerateKey(nil)  // attacker, NOT Lens

	srv := NewInferenceServer(fakeProvider{resp: InferResponse{Text: "x"}}, "", NodeConfig{})
	rs, _ := testSigner(t)
	_ = rs.sign("r1", "m", 1, 5, "héllo")
	srv.SetReceiptSigner(rs, nil)
	srv.SetChallengePubKey(stalePub)
	srv.challengeRefetch = func(context.Context) (ed25519.PublicKey, error) { return lensRealPub, nil }
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	forged := povi.SignChallenge(forgerPriv, "r1", []int{0, 1}, 42)
	body, _ := json.Marshal(forged)
	resp, err := http.Post(ts.URL+"/challenge", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged challenge = %d, want 401 (re-fetch must NOT rescue a forged challenge)", resp.StatusCode)
	}
}

// 3b — a node that answers (verifies the challenge) but returns paths that DON'T match
// the recorded root is still slashed (provable misbehavior, untouched by the re-fetch).
func TestRotationRace_BadPathsStillSlashes(t *testing.T) {
	lensPub, lensPriv, _ := ed25519.GenerateKey(nil)

	// Node trace commits "héllo"; the recorded receipt claims a DIFFERENT root ("wörld",
	// same 5-leaf count) — the node's answers will not verify against it.
	url, rec := staleNode(t, "r1", "héllo", lensPub, nil) // pinned = the live Lens key → verifies fine
	wrong := povi.MerkleRoot(povi.StepsFromRunes("wörld"))
	rec.MerkleRootHex = hex.EncodeToString(wrong[:])

	if n, result := challenge(t, url, rec, lensPriv); n != 1 || result != povi.ChallengeFail {
		t.Fatalf("bad paths: slashes=%d result=%q, want 1 / fail (provable misbehavior must still slash)", n, result)
	}
}

// 3c — a node that can't answer (no retained trace for the challenged request) is still
// slashed on timeout.
func TestRotationRace_TimeoutStillSlashes(t *testing.T) {
	lensPub, lensPriv, _ := ed25519.GenerateKey(nil)
	url, rec := staleNode(t, "r1", "héllo", lensPub, nil) // pinned = live key → challenge verifies
	rec.RequestID = "r2"                                  // but the node has NO trace for r2
	rec.LeafCount = 5

	if n, _ := challenge(t, url, rec, lensPriv); n != 1 {
		t.Fatalf("no-answer: slashes=%d, want 1 (a node that can't answer must still slash)", n)
	}
}

// ─── 4. rate-limit: a flood of unverifiable challenges → ONE re-fetch ──

func TestReactiveRefetch_RateLimited(t *testing.T) {
	stalePub, _, _ := povi.GenerateNodeKey()
	lensRealPub, _, _ := ed25519.GenerateKey(nil)
	_, forgerPriv, _ := ed25519.GenerateKey(nil)

	srv := NewInferenceServer(fakeProvider{resp: InferResponse{Text: "x"}}, "", NodeConfig{})
	rs, _ := testSigner(t)
	_ = rs.sign("r1", "m", 1, 5, "héllo")
	srv.SetReceiptSigner(rs, nil)
	srv.SetChallengePubKey(stalePub)

	var refetches int64
	var mu sync.Mutex
	srv.challengeRefetch = func(context.Context) (ed25519.PublicKey, error) {
		mu.Lock()
		refetches++
		mu.Unlock()
		return lensRealPub, nil // never matches the forged sigs → stays unverifiable
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 20 forged challenges in a tight burst; all within reactiveRefetchMinInterval.
	for i := 0; i < 20; i++ {
		forged := povi.SignChallenge(forgerPriv, "r1", []int{0, 1}, int64(i))
		body, _ := json.Marshal(forged)
		resp, err := http.Post(ts.URL+"/challenge", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("forged challenge #%d = %d, want 401", i, resp.StatusCode)
		}
	}
	mu.Lock()
	got := refetches
	mu.Unlock()
	if got != 1 {
		t.Fatalf("re-fetches under a 20-challenge flood = %d, want 1 (rate-limited — no Lens hammering)", got)
	}
}
