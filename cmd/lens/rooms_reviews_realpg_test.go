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

// B41.144 (B41.44's DONE line, from Lens alone) — a contribution's quality is the median of two scores from members not
// linked to its author, and its author cannot pick the reviewers; reviewers whose scores agreed with the decision earn
// points, and a review ring's score counts as 1.0 and earns nothing.
func TestRooms_TwoRandomUnlinkedReviewersScoreAContributionsQuality(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const author, sameKey, sameCard, sameUser = "ws-b41144-author", "ws-b41144-samekey", "ws-b41144-samecard", "ws-b41144-sameuser"
	const r1, r2 = "ws-b41144-r1", "ws-b41144-r2"
	for _, ws := range []string{author, sameKey, sameCard, sameUser, r1, r2} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	// Three members linked to the author: one shares its owner key, one its card, and one was joined by the same person.
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_owner_links (workspace_id, owner_key) VALUES ($1, 'human-b41144'), ($2, 'human-b41144')`,
		author, sameKey); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash) VALUES ($1, 'fp-b41144'), ($2, 'fp-b41144')`,
		author, sameCard); err != nil {
		t.Fatal(err)
	}
	marketStore := market.NewStore(pool)
	store := rooms.NewStore(pool, 3000)
	store.SetMarket(marketStore)
	store.SetReviewPoints(5)
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
	// The author owns the room, so every other member is a candidate unless it is linked to the author.
	var room rooms.Detail
	do(http.StatusCreated, &room, author, http.MethodPost, "/v1/workspaces/"+author+"/rooms", `{"title":"Reviewed build"}`)
	base := "/v1/rooms/" + room.ID
	for _, ws := range []string{sameKey, sameCard, sameUser, r1} {
		do(http.StatusCreated, nil, ws, http.MethodPost, base+"/join", `{"terms_version":1}`)
	}
	if _, err := pool.Exec(ctx, `UPDATE room_members SET user_id = 'user-'||$2 WHERE room_id = $1 AND workspace_id = $3`, room.ID, author, sameUser); err != nil {
		t.Fatal(err)
	}
	do(http.StatusOK, nil, author, http.MethodPut, base+"/rate-card", `{"pool_points":10000,"sizes":[{"size":"M","hours":8,"points_per_hour":50}]}`)
	var c rooms.Contribution
	do(http.StatusCreated, &c, author, http.MethodPost, base+"/contributions", `{"kind":"prompt","title":"Onboarding copy","artifact":{"template":"Welcome {{name}}."}}`)
	do(http.StatusOK, nil, author, http.MethodPut, base+"/contributions/"+c.ID+"/size", `{"size":"M"}`)
	cbase := base + "/contributions/" + c.ID

	// The author asks for the draw and Lens picks: only r1 is not linked to it, so one seat stays empty until r2 joins.
	var v rooms.Reviews
	do(http.StatusOK, &v, author, http.MethodPost, cbase+"/reviewers", "")
	if len(v.Reviews) != 1 || v.Reviews[0].ReviewerWorkspaceID != r1 {
		t.Fatalf("the draw seated %+v, want r1 alone: the author and members linked to it are never drawn", v.Reviews)
	}
	do(http.StatusCreated, nil, r2, http.MethodPost, base+"/join", `{"terms_version":1}`)
	do(http.StatusOK, &v, author, http.MethodPost, cbase+"/reviewers", "")
	if len(v.Reviews) != 2 || v.Reviews[0].ReviewerWorkspaceID != r1 || v.Reviews[1].ReviewerWorkspaceID != r2 {
		t.Fatalf("the second draw seated %+v, want r1 kept and r2 in the empty seat", v.Reviews)
	}

	// Nobody else scores it: not the author, not a linked member.
	for _, ws := range []string{author, sameKey} {
		do(http.StatusForbidden, nil, ws, http.MethodPut, cbase+"/review", `{"score_bps":15000}`)
	}
	// One score leaves quality at 1.0; the second sets it to the median of the two: (1.2 + 1.4) / 2 = 1.3.
	do(http.StatusOK, &v, r1, http.MethodPut, cbase+"/review", `{"score_bps":12000}`)
	if v.QualityBPS != 10000 {
		t.Fatalf("after one score quality_bps = %d, want 10000 until both reviewers score", v.QualityBPS)
	}
	do(http.StatusOK, &v, r2, http.MethodPut, cbase+"/review", `{"score_bps":14000}`)
	if v.QualityBPS != 13000 {
		t.Fatalf("after both scores quality_bps = %d, want the median 13000", v.QualityBPS)
	}

	// Accepted, it earns 400 × 1.3 = 520, and both reviewers, who scored it at least 1.0, earn the review points.
	do(http.StatusOK, nil, author, http.MethodPatch, cbase, `{"status":"accepted"}`)
	var p rooms.Points
	do(http.StatusOK, &p, author, http.MethodGet, base+"/points", "")
	got := map[string]int64{}
	for _, e := range p.Entries {
		got[e.WorkspaceID+"/"+e.Reason] = e.Points
	}
	if len(got) != 3 || got[author+"/accepted"] != 520 || got[r1+"/review"] != 5 || got[r2+"/review"] != 5 {
		t.Fatalf("the ledger is %v, want 520 to the author and 5 to each agreeing reviewer", got)
	}
	do(http.StatusConflict, nil, r1, http.MethodPut, cbase+"/review", `{"score_bps":5000}`)

	// A review ring: r1 scored the author's work, so on r1's own contribution the author's score counts as 1.0 and earns
	// nothing. With the linked members gone, the draw seats the author and r2.
	if _, err := pool.Exec(ctx, `UPDATE room_members SET removed_at = now() WHERE room_id = $1 AND workspace_id IN ($2, $3, $4)`,
		room.ID, sameKey, sameCard, sameUser); err != nil {
		t.Fatal(err)
	}
	var c2 rooms.Contribution
	do(http.StatusCreated, &c2, r1, http.MethodPost, base+"/contributions", `{"kind":"prompt","title":"Pricing copy","artifact":{"template":"Plans."}}`)
	c2base := base + "/contributions/" + c2.ID
	do(http.StatusOK, &v, r1, http.MethodPost, c2base+"/reviewers", "")
	if len(v.Reviews) != 2 {
		t.Fatalf("the draw seated %+v, want the author and r2", v.Reviews)
	}
	do(http.StatusOK, nil, author, http.MethodPut, c2base+"/review", `{"score_bps":15000}`)
	do(http.StatusOK, &v, r2, http.MethodPut, c2base+"/review", `{"score_bps":11000}`)
	if v.QualityBPS != 10500 {
		t.Fatalf("quality_bps = %d, want the median of the ring's 1.0 and r2's 1.1: 10500 (%+v)", v.QualityBPS, v.Reviews)
	}
	// Accepted: both scores agreed, but only r2 earns; the ring earns nothing.
	do(http.StatusOK, nil, author, http.MethodPatch, c2base, `{"status":"accepted"}`)
	do(http.StatusOK, &p, author, http.MethodGet, base+"/points", "")
	if e := p.Entries[0]; e.ContributionID != c2.ID || e.WorkspaceID != r2 || e.Reason != "review" || e.QualityBPS != 11000 || e.Points != 5 {
		t.Fatalf("the newest row is %+v, want r2's review row of 5 for agreeing with the acceptance", e)
	}
	if len(p.Entries) != 4 {
		t.Fatalf("the ledger has %d rows, want 4: the ring earned nothing", len(p.Entries))
	}
}
