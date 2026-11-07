package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/rooms"
)

// B41.147 (B41.47's DONE line, from Lens alone) — an objection raised in the window after the leaderboard freezes goes
// to three members drawn at random from those linked neither to the objector nor to the author, and their majority
// decision changes the points in the ledger. Outside the window, or before the freeze, there is no objection.
func TestRooms_AnObjectionGoesToThreeUnlinkedMembersWhoseMajorityChangesThePoints(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const owner, alice, bob, linked = "ws-b41147-owner", "ws-b41147-alice", "ws-b41147-bob", "ws-b41147-linked"
	const p1, p2, p3, p4 = "ws-b41147-p1", "ws-b41147-p2", "ws-b41147-p3", "ws-b41147-p4"
	for _, ws := range []string{owner, alice, bob, linked, p1, p2, p3, p4} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	// linked shares an owner key with Alice, the author: it never sits on a panel about her points.
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_owner_links (workspace_id, owner_key) VALUES ($1, 'human-b41147'), ($2, 'human-b41147')`,
		alice, linked); err != nil {
		t.Fatal(err)
	}
	// p1 and p2 share a card: one person, so never two seats on one panel.
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash) VALUES ($1, 'fp-b41147'), ($2, 'fp-b41147')`,
		p1, p2); err != nil {
		t.Fatal(err)
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
	for _, ws := range []string{alice, bob, linked, p1, p2} {
		do(http.StatusCreated, nil, ws, http.MethodPost, base+"/join", `{"terms_version":1}`)
	}
	do(http.StatusOK, nil, owner, http.MethodPut, base+"/shared-ownership", `{"enabled":true,"pool_bps":2500}`)
	do(http.StatusOK, nil, owner, http.MethodPut, base+"/rate-card", `{"pool_points":10000,"sizes":[{"size":"M","hours":8,"points_per_hour":50}]}`)
	contribute := func(ws, title string) rooms.Contribution {
		var c rooms.Contribution
		do(http.StatusCreated, &c, ws, http.MethodPost, base+"/contributions", `{"kind":"prompt","title":"`+title+`","artifact":{"template":"Hello."}}`)
		do(http.StatusOK, nil, ws, http.MethodPut, base+"/contributions/"+c.ID+"/size", `{"size":"M"}`)
		return c
	}
	c := contribute(alice, "Copy A")
	do(http.StatusOK, nil, owner, http.MethodPatch, base+"/contributions/"+c.ID, `{"status":"accepted"}`) // 400 points
	raise := `{"contribution_id":"` + c.ID + `","points":-200,"reason":"Half of it was copied from the brief."}`

	// No objection before the freeze; only the owner freezes, once.
	do(http.StatusConflict, nil, bob, http.MethodPost, base+"/objections", raise)
	do(http.StatusForbidden, nil, bob, http.MethodPost, base+"/leaderboard/freeze", "")
	var ob rooms.Objections
	do(http.StatusOK, &ob, owner, http.MethodPost, base+"/leaderboard/freeze", "")
	if ob.FrozenAt == nil || ob.ObjectionsCloseAt == nil || ob.ObjectionsCloseAt.Sub(*ob.FrozenAt).Hours() != 14*24 || ob.Rules != rooms.ObjectionRules {
		t.Fatalf("the freeze answered %+v, want a 14-day window and the published rules", ob)
	}
	do(http.StatusConflict, nil, owner, http.MethodPost, base+"/leaderboard/freeze", "")
	var l rooms.Leaderboard
	do(http.StatusOK, &l, bob, http.MethodGet, base+"/leaderboard", "")
	if l.FrozenAt == nil || !l.ObjectionsCloseAt.Equal(*ob.ObjectionsCloseAt) {
		t.Fatalf("the leaderboard is %+v, want it frozen with the window's end", l)
	}
	// Frozen, the points move only by objection: a sized contribution is not accepted now.
	late := contribute(bob, "Copy B")
	do(http.StatusConflict, nil, owner, http.MethodPatch, base+"/contributions/"+late.ID, `{"status":"accepted"}`)

	// Only accepted contributions' points are objected to.
	do(http.StatusConflict, nil, bob, http.MethodPost, base+"/objections", `{"contribution_id":"`+late.ID+`","points":100,"reason":"It counts."}`)
	// With p1, p2 (one person) and p3, Lens cannot seat three independent members: the member linked to Alice never
	// counts, and p1 and p2 never sit together.
	do(http.StatusCreated, nil, p3, http.MethodPost, base+"/join", `{"terms_version":1}`)
	do(http.StatusConflict, nil, bob, http.MethodPost, base+"/objections", raise)
	do(http.StatusCreated, nil, p4, http.MethodPost, base+"/join", `{"terms_version":1}`)

	// Bob objects: Lens seats three members linked neither to Bob, nor to Alice, nor to each other — never the owner,
	// Alice, Bob or the member linked to Alice, and only one of p1 and p2.
	var o rooms.Objection
	do(http.StatusCreated, &o, bob, http.MethodPost, base+"/objections", raise)
	var panel []string
	for _, p := range o.Panel {
		panel = append(panel, p.WorkspaceID)
	}
	sort.Strings(panel)
	if got := strings.Join(panel, ","); o.Status != rooms.ObjectionOpen || o.HolderWorkspaceID != alice ||
		(got != p1+","+p3+","+p4 && got != p2+","+p3+","+p4) {
		t.Fatalf("the objection is %+v, want it open about Alice's points before p3, p4 and one of p1 and p2", o)
	}
	// Each person objects once, and never the author or a member linked to the author.
	for _, ws := range []string{bob, alice, linked} {
		do(http.StatusConflict, nil, ws, http.MethodPost, base+"/objections", raise)
	}
	do(http.StatusForbidden, nil, bob, http.MethodPut, base+"/objections/"+o.ID+"/vote", `{"uphold":true}`)

	// Two of three uphold it: the objection is decided, and Alice's 400 points become 200 by an objection row.
	vote := base + "/objections/" + o.ID + "/vote"
	do(http.StatusOK, &o, panel[0], http.MethodPut, vote, `{"uphold":true}`)
	do(http.StatusOK, &o, p3, http.MethodPut, vote, `{"uphold":false}`)
	if o.Status != rooms.ObjectionOpen {
		t.Fatalf("after one vote each way the objection is %s, want open", o.Status)
	}
	do(http.StatusOK, &o, p4, http.MethodPut, vote, `{"uphold":true}`)
	if o.Status != rooms.ObjectionUpheld || o.DecidedAt == nil {
		t.Fatalf("after two votes to uphold the objection is %+v, want upheld", o)
	}
	do(http.StatusConflict, nil, p3, http.MethodPut, vote, `{"uphold":true}`)
	var row int64
	if err := pool.QueryRow(ctx, `SELECT points FROM room_points WHERE room_id = $1 AND workspace_id = $2 AND reason = 'objection'`,
		room.ID, alice).Scan(&row); err != nil || row != -200 {
		t.Fatalf("Alice's objection row is %d (%v), want -200", row, err)
	}
	var pts rooms.Points
	do(http.StatusOK, &pts, p1, http.MethodGet, base+"/points", "")
	if len(pts.Holders) != 1 || pts.Holders[0].WorkspaceID != alice || pts.Holders[0].Points != 200 {
		t.Fatalf("the holders are %+v, want Alice with 200", pts.Holders)
	}

	// Once the window closes, no objection is raised; anyone who is not a member sees none.
	if _, err := pool.Exec(ctx, `UPDATE room_leaderboard_freezes SET objections_close_at = now() - interval '1 second' WHERE room_id = $1`,
		room.ID); err != nil {
		t.Fatal(err)
	}
	do(http.StatusConflict, nil, p1, http.MethodPost, base+"/objections", `{"contribution_id":"`+c.ID+`","points":100,"reason":"Too low now."}`)
	do(http.StatusOK, &ob, p1, http.MethodGet, base+"/objections", "")
	if len(ob.Objections) != 1 || ob.Objections[0].Status != rooms.ObjectionUpheld || len(ob.Objections[0].Panel) != 3 {
		t.Fatalf("the room's objections are %+v, want the one upheld with its panel", ob.Objections)
	}
	do(http.StatusNotFound, nil, "ws-b41147-stranger", http.MethodGet, base+"/objections", "")
}
