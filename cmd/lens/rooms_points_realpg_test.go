package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/rooms"
)

// B41.143 (B41.43's DONE line, from Lens alone) — an accepted M-size contribution with quality 1.2 earns 1.2 × M points
// in an append-only row, and a fork credits its original the room's remix share; royalties already received take
// points off a size, rejecting accepted work takes its points back, and an owner holds points once identity-checked.
func TestRooms_PointsLedgerEarnsSizeTimesQualityAndAForkCreditsItsOriginal(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const owner, author, forker = "ws-b41143-owner", "ws-b41143-author", "ws-b41143-forker"
	for _, ws := range []string{owner, author, forker} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	// The author's owner is identity-checked at L2 (by the Test provider, as a synthetic workspace's is); the forker's is not.
	for level, subject := range map[int]string{1: "contact", 2: "person"} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref)
			VALUES ($1, $2, $3, 'test', 'completed', 'ref-' || $3)`, author, level, subject); err != nil {
			t.Fatal(err)
		}
	}
	marketStore := market.NewStore(pool)
	store := rooms.NewStore(pool, 3000)
	store.SetMarket(marketStore)
	store.SetPointsPerUSD(10)
	r := chi.NewRouter()
	mountMarketRoutes(r, marketStore)
	mountRoomRoutes(r, store)
	mountRoomOwnershipRoutes(r, store)
	do := func(want int, v any, ws, method, path, body string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != want || (v != nil && json.Unmarshal(w.Body.Bytes(), v) != nil) {
			t.Fatalf("%s %s as %s = %d %s, want %d", method, path, ws, w.Code, w.Body.String(), want)
		}
	}
	var room rooms.Detail
	do(http.StatusCreated, &room, owner, http.MethodPost, "/v1/workspaces/"+owner+"/rooms", `{"title":"Shared build","terms":{"remix_share_bps":2500}}`)
	base := "/v1/rooms/" + room.ID
	for _, ws := range []string{author, forker} {
		do(http.StatusCreated, nil, ws, http.MethodPost, base+"/join", `{"terms_version":1}`)
		do(http.StatusCreated, nil, ws, http.MethodPost, base+"/agreement", `{"version":"`+rooms.AgreementVersion+`","signed_name":"Signer"}`) // B41.150: points count once signed
	}
	do(http.StatusOK, nil, owner, http.MethodPut, base+"/rate-card", `{"pool_points":10000,"sizes":[{"size":"M","hours":8,"points_per_hour":50}]}`)
	accept := func(ws string, c rooms.Contribution) {
		t.Helper()
		do(http.StatusOK, nil, ws, http.MethodPut, base+"/contributions/"+c.ID+"/size", `{"size":"M"}`)
		do(http.StatusOK, nil, owner, http.MethodPatch, base+"/contributions/"+c.ID, `{"status":"accepted"}`)
	}
	ledger := func() rooms.Points {
		t.Helper()
		var p rooms.Points
		do(http.StatusOK, &p, author, http.MethodGet, base+"/points", "")
		return p
	}

	// The author's M-size contribution (400 points), scored at quality 1.2 — B41.144's reviewers will set this — earns 480.
	var orig rooms.Contribution
	do(http.StatusCreated, &orig, author, http.MethodPost, base+"/contributions", `{"kind":"prompt","title":"Onboarding copy","artifact":{"template":"Welcome {{name}}."}}`)
	if _, err := pool.Exec(ctx, `UPDATE room_contributions SET quality_bps = 12000 WHERE id = $1`, orig.ID); err != nil {
		t.Fatal(err)
	}
	accept(author, orig)
	p := ledger()
	if len(p.Entries) != 1 || p.Entries[0].Reason != "accepted" || p.Entries[0].WorkspaceID != author || p.Entries[0].SizePoints != 400 ||
		p.Entries[0].QualityBPS != 12000 || p.Entries[0].SurvivalBPS != 10000 || p.Entries[0].Points != 480 {
		t.Fatalf("after the accept the ledger is %+v, want one accepted row of 400 × 1.2 × 100%% = 480 to the author", p.Entries)
	}
	if _, err := pool.Exec(ctx, `UPDATE room_points SET points = 9999 WHERE contribution_id = $1`, orig.ID); err == nil {
		t.Fatal("a points row was changed: the ledger must be append-only")
	}

	// The forker's fork, accepted at M and quality 1.0, earns 400 and passes the room's 25% remix share — 100 — to the original's author.
	var fork rooms.Contribution
	do(http.StatusCreated, &fork, forker, http.MethodPost, base+"/contributions/"+orig.ID+"/fork", `{"title":"Shorter onboarding"}`)
	accept(forker, fork)
	got := map[string]int64{}
	for _, e := range ledger().Entries {
		if e.ContributionID == fork.ID {
			got[e.WorkspaceID+"/"+e.Reason] = e.Points
		}
	}
	if len(got) != 2 || got[forker+"/accepted"] != 300 || got[author+"/remix_share"] != 100 {
		t.Fatalf("the fork's rows are %v, want 300 to the forker and the 25%% remix share, 100, to the original's author", got)
	}

	// A contribution that already received $5 in royalties loses 5 × 10 points of its size: 400 − 50 = 350.
	var paid rooms.Contribution
	do(http.StatusCreated, &paid, forker, http.MethodPost, base+"/contributions", `{"kind":"prompt","title":"Pricing copy","artifact":{"template":"Plans."}}`)
	if _, err := pool.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, price_ulxc, charge)
		VALUES ('use-b41143', $1, 1, $2, $3, 1, 'billed')`, paid.ListingID, forker, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO market_earnings (use_id, seller_workspace_id, gross_usd_micros, share_usd_micros, invoice_id, cleared_at, payable_at)
		VALUES ('use-b41143', $1, 6000000, 5000000, 'in-b41143', now(), now())`, forker); err != nil {
		t.Fatal(err)
	}
	accept(forker, paid)

	// Rejected after it was accepted, the original no longer survives: a withdrawn row takes the author's 480 back.
	do(http.StatusOK, nil, owner, http.MethodPatch, base+"/contributions/"+orig.ID, `{"status":"rejected"}`)
	p = ledger()
	held := map[string]rooms.PointsHolder{}
	for _, h := range p.Holders {
		held[h.WorkspaceID] = h
	}
	if p.Entries[0].Reason != "withdrawn" || p.Entries[0].Points != -480 || p.Entries[0].SurvivalBPS != 0 {
		t.Fatalf("the newest row is %+v, want the author's 480 withdrawn at survival 0", p.Entries[0])
	}
	if h := held[author]; h.Points != 100 || !h.IdentityChecked {
		t.Fatalf("the author holds %+v, want the remix share's 100, identity-checked", h)
	}
	if h := held[forker]; h.Points != 300+350 || h.IdentityChecked {
		t.Fatalf("the forker holds %+v, want 300 + 350 waiting for its owner's L2 check", h)
	}
	if p.TotalPoints != 100 {
		t.Fatalf("total_points = %d, want only the identity-checked author's 100", p.TotalPoints)
	}

	// Anyone who is not a member does not see the ledger.
	do(http.StatusNotFound, nil, "ws-b41143-stranger", http.MethodGet, base+"/points", "")
}
