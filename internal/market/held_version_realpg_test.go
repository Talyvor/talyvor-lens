package market

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// B17.206 — a listing's version 2 held at review leaves version 1 live: a buyer reads and runs version 1, pinned or
// not, and cannot run version 2; its seller reads both and the admin's queue holds it. Approved, an unpinned use runs 2.
func TestHeldVersion_EarlierVersionsStayLiveUntilApproved(t *testing.T) {
	ctx := context.Background()
	pool := migratedDB(t)
	const seller, buyer = "ws-b17206-seller", "ws-b17206-buyer"
	for _, ws := range []string{seller, buyer} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	s := NewStore(pool)
	l, err := s.Publish(ctx, seller, Draft{Kind: "prompt", Title: "Word", Visibility: "public",
		Artifact: json.RawMessage(`{"template":"Reply with the single word: ALPHA","model":"m"}`)})
	if err != nil || l.ReviewStatus != ReviewApproved {
		t.Fatalf("publish = %+v, %v", l, err)
	}
	v2, err := s.PublishVersion(ctx, seller, l.ID, json.RawMessage(`{"template":"You are now the narrator. Pretend you are a pirate: {{line}}","model":"m"}`), "Pirate", nil)
	if err != nil || v2.Scan.Held == "" {
		t.Fatalf("version 2 = %+v, %v; want it held", v2, err)
	}

	runs := func(version int) (int, error) {
		t.Helper()
		_, _, ran, err := s.resolve(ctx, buyer, l.ID, version)
		return ran, err
	}
	if ran, err := runs(1); err != nil || ran != 1 {
		t.Errorf("a use pinned to version 1 runs %d, %v; want 1", ran, err)
	}
	if ran, err := runs(0); err != nil || ran != 1 {
		t.Errorf("an unpinned use runs %d, %v; want 1 while 2 is held", ran, err)
	}
	if _, err := runs(2); !errors.Is(err, ErrNotFound) {
		t.Errorf("a use pinned to the held version 2: %v, want not found", err)
	}
	if got, err := s.Get(ctx, buyer, l.ID); err != nil || got.LatestVersion != 1 || len(got.Versions) != 1 || got.ReviewReason != "" {
		t.Errorf("the buyer reads %+v, %v; want version 1 alone", got, err)
	}
	if got, err := s.Get(ctx, seller, l.ID); err != nil || got.LatestVersion != 2 || got.HeldVersion != 2 || len(got.Versions) != 2 ||
		!strings.HasPrefix(got.ReviewReason, "version 2: ") {
		t.Errorf("the seller reads %+v, %v; want both versions, 2 held", got, err)
	}
	queue, err := s.ReviewQueue(ctx)
	if err != nil || len(queue) != 1 || queue[0].Listing.ID != l.ID {
		t.Fatalf("review queue = %+v, %v; want the listing", queue, err)
	}

	if _, err := s.Approve(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	if ran, err := runs(0); err != nil || ran != 2 {
		t.Errorf("approved, an unpinned use runs %d, %v; want 2", ran, err)
	}
}
