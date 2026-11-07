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
// points, and a review ring's scores count as 1.0, both ways, and earn nothing.
func TestRooms_TwoRandomUnlinkedReviewersScoreAContributionsQuality(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const owner, author, sameKey, sameCard, sameUser = "ws-b41144-owner", "ws-b41144-author", "ws-b41144-samekey", "ws-b41144-samecard", "ws-b41144-sameuser"
	const r1, r2 = "ws-b41144-r1", "ws-b41144-r2"
	for _, ws := range []string{owner, author, sameKey, sameCard, sameUser, r1, r2} {
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
	var room rooms.Detail
	do(http.StatusCreated, &room, owner, http.MethodPost, "/v1/workspaces/"+owner+"/rooms", `{"title":"Reviewed build"}`)
	base := "/v1/rooms/" + room.ID
	for _, ws := range []string{author, sameKey, sameCard, sameUser, r1} {
		do(http.StatusCreated, nil, ws, http.MethodPost, base+"/join", `{"terms_version":1}`)
	}
	if _, err := pool.Exec(ctx, `UPDATE room_members SET user_id = 'user-'||$2 WHERE room_id = $1 AND workspace_id = $3`, room.ID, author, sameUser); err != nil {
		t.Fatal(err)
	}
	do(http.StatusOK, nil, owner, http.MethodPut, base+"/rate-card", `{"pool_points":10000,"sizes":[{"size":"M","hours":8,"points_per_hour":50}]}`)
	contribute := func(ws, title string) (rooms.Contribution, string) {
		t.Helper()
		var c rooms.Contribution
		do(http.StatusCreated, &c, ws, http.MethodPost, base+"/contributions", `{"kind":"prompt","title":"`+title+`","artifact":{"template":"Hi."}}`)
		do(http.StatusOK, nil, ws, http.MethodPut, base+"/contributions/"+c.ID+"/size", `{"size":"M"}`)
		return c, base + "/contributions/" + c.ID
	}
	c1, c1base := contribute(author, "Onboarding copy")

	// The author asks for the draw and Lens picks: only r1 is a member that does not decide and is not linked to the
	// author, so one seat stays empty until r2 joins.
	var v rooms.Reviews
	do(http.StatusOK, &v, author, http.MethodPost, c1base+"/reviewers", "")
	if len(v.Reviews) != 1 || v.Reviews[0].ReviewerWorkspaceID != r1 {
		t.Fatalf("the draw seated %+v, want r1 alone: the owner, the author and members linked to it are never drawn", v.Reviews)
	}
	do(http.StatusCreated, nil, r2, http.MethodPost, base+"/join", `{"terms_version":1}`)
	do(http.StatusOK, &v, author, http.MethodPost, c1base+"/reviewers", "")
	if len(v.Reviews) != 2 || v.Reviews[0].ReviewerWorkspaceID != r1 || v.Reviews[1].ReviewerWorkspaceID != r2 {
		t.Fatalf("the second draw seated %+v, want r1 kept and r2 in the empty seat", v.Reviews)
	}

	// Nobody else scores it: not the author, not a linked member.
	for _, ws := range []string{author, sameKey} {
		do(http.StatusForbidden, nil, ws, http.MethodPut, c1base+"/review", `{"score_bps":15000}`)
	}
	// One score leaves quality at 1.0; the second sets it to the median of the two: (1.2 + 1.4) / 2 = 1.3.
	do(http.StatusOK, &v, r1, http.MethodPut, c1base+"/review", `{"score_bps":12000}`)
	if v.QualityBPS != 10000 {
		t.Fatalf("after one score quality_bps = %d, want 10000 until both reviewers score", v.QualityBPS)
	}
	do(http.StatusOK, &v, r2, http.MethodPut, c1base+"/review", `{"score_bps":14000}`)
	if v.QualityBPS != 13000 {
		t.Fatalf("after both scores quality_bps = %d, want the median 13000", v.QualityBPS)
	}

	// A review ring: r1 contributes, and with the linked members gone the draw seats the author and r2. The author scores
	// r1's work after r1 scored its own: both scores now count as 1.0, so c1's quality falls to (1.0 + 1.4) / 2 = 1.2.
	if _, err := pool.Exec(ctx, `UPDATE room_members SET removed_at = now() WHERE room_id = $1 AND workspace_id IN ($2, $3, $4)`,
		room.ID, sameKey, sameCard, sameUser); err != nil {
		t.Fatal(err)
	}
	c2, c2base := contribute(r1, "Pricing copy")
	do(http.StatusOK, &v, r1, http.MethodPost, c2base+"/reviewers", "")
	if len(v.Reviews) != 2 || v.Reviews[0].ReviewerWorkspaceID != author || v.Reviews[1].ReviewerWorkspaceID != r2 {
		t.Fatalf("the draw seated %+v, want the author and r2", v.Reviews)
	}
	do(http.StatusOK, nil, author, http.MethodPut, c2base+"/review", `{"score_bps":15000}`)
	do(http.StatusOK, &v, author, http.MethodGet, c1base+"/reviews", "")
	if v.QualityBPS != 12000 || !v.Reviews[0].Ring || v.Reviews[1].Ring {
		t.Fatalf("c1's reviews are %+v (quality %d), want r1's score a ring and quality 12000", v.Reviews, v.QualityBPS)
	}
	do(http.StatusOK, &v, r2, http.MethodPut, c2base+"/review", `{"score_bps":11000}`)
	if v.QualityBPS != 10500 {
		t.Fatalf("c2's quality_bps = %d, want the median of the ring's 1.0 and r2's 1.1: 10500 (%+v)", v.QualityBPS, v.Reviews)
	}

	// Accepted, c1 earns 400 × 1.2 = 480 and c2 400 × 1.05 = 420; r2 agreed both times and earns 5 each time, and the
	// ring's two members, who agreed too, earn nothing for it.
	do(http.StatusOK, nil, owner, http.MethodPatch, c1base, `{"status":"accepted"}`)
	do(http.StatusOK, nil, owner, http.MethodPatch, c2base, `{"status":"accepted"}`)
	var p rooms.Points
	do(http.StatusOK, &p, author, http.MethodGet, base+"/points", "")
	got := map[string]int64{}
	for _, e := range p.Entries {
		got[e.ContributionID+"/"+e.WorkspaceID+"/"+e.Reason] = e.Points
	}
	want := map[string]int64{c1.ID + "/" + author + "/accepted": 480, c1.ID + "/" + r2 + "/review": 5,
		c2.ID + "/" + r1 + "/accepted": 420, c2.ID + "/" + r2 + "/review": 5}
	if len(p.Entries) != len(want) || len(got) != len(want) {
		t.Fatalf("the ledger is %v, want %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("the ledger is %v, want %v", got, want)
		}
	}
	// Decided, it is scored no more.
	do(http.StatusConflict, nil, r2, http.MethodPut, c1base+"/review", `{"score_bps":5000}`)
}
