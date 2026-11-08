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

// B41.145 (B41.45's DONE line, from Lens alone) — after three accepted contributions by two people, the leaderboard
// shows both with shares that add up to 100% of the pool, with the note that nothing is final until the Gate, and
// anyone who is not a member does not see it.
func TestRooms_LeaderboardSharesTheWholePoolBetweenItsHolders(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const owner, alice, bob = "ws-b41145-owner", "ws-b41145-alice", "ws-b41145-bob"
	for _, ws := range []string{owner, alice, bob} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	for _, ws := range []string{alice, bob} {
		for level, subject := range map[int]string{1: "contact", 2: "person"} {
			if _, err := pool.Exec(ctx, `INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref)
				VALUES ($1, $2, $3, 'test', 'completed', 'ref-' || $1 || $3)`, ws, level, subject); err != nil {
				t.Fatal(err)
			}
		}
	}
	marketStore := market.NewStore(pool)
	store := rooms.NewStore(pool, 3000)
	store.SetMarket(marketStore)
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
	do(http.StatusCreated, &room, owner, http.MethodPost, "/v1/workspaces/"+owner+"/rooms", `{"title":"Shared build"}`)
	base := "/v1/rooms/" + room.ID
	for _, ws := range []string{alice, bob} {
		do(http.StatusCreated, nil, ws, http.MethodPost, base+"/join", `{"terms_version":1}`)
		do(http.StatusCreated, nil, ws, http.MethodPost, base+"/agreement", `{"version":"`+rooms.AgreementVersion+`","signed_name":"Signer"}`) // B41.150: points count once signed
	}
	do(http.StatusOK, nil, owner, http.MethodPut, base+"/shared-ownership", `{"enabled":true,"pool_bps":2500}`)
	do(http.StatusOK, nil, owner, http.MethodPut, base+"/rate-card", `{"pool_points":10000,"sizes":[{"size":"M","hours":8,"points_per_hour":50}]}`)

	// Alice's two contributions and Bob's one are accepted at M, 400 points each: 800 to 400.
	for i, ws := range []string{alice, alice, bob} {
		var c rooms.Contribution
		do(http.StatusCreated, &c, ws, http.MethodPost, base+"/contributions",
			`{"kind":"prompt","title":"Copy `+string(rune('A'+i))+`","artifact":{"template":"Hello."}}`)
		do(http.StatusOK, nil, ws, http.MethodPut, base+"/contributions/"+c.ID+"/size", `{"size":"M"}`)
		do(http.StatusOK, nil, owner, http.MethodPatch, base+"/contributions/"+c.ID, `{"status":"accepted"}`)
	}

	var l rooms.Leaderboard
	do(http.StatusOK, &l, bob, http.MethodGet, base+"/leaderboard", "")
	if !l.Enabled || l.PoolBPS != 2500 || l.TotalPoints != 1200 || l.Note != "Nothing is final until the Gate." {
		t.Fatalf("the leaderboard is %+v, want shared ownership on at a 25%% pool, 1200 points and the Gate note", l)
	}
	if len(l.Rows) != 2 || l.Rows[0].WorkspaceID != alice || l.Rows[0].Points != 800 || l.Rows[1].WorkspaceID != bob || l.Rows[1].Points != 400 {
		t.Fatalf("the rows are %+v, want Alice's 800 then Bob's 400", l.Rows)
	}
	// 800/1200 and 400/1200 of the pool are 6666.7 and 3333.3 basis points: rounded so they add up to 10000.
	if l.Rows[0].ShareBPS != 6667 || l.Rows[1].ShareBPS != 3333 {
		t.Fatalf("the shares are %d and %d, want 6667 and 3333: 100%% of the pool", l.Rows[0].ShareBPS, l.Rows[1].ShareBPS)
	}

	// Anyone who is not a member does not see the leaderboard.
	do(http.StatusNotFound, nil, "ws-b41145-stranger", http.MethodGet, base+"/leaderboard", "")
}
