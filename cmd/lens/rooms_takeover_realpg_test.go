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

// B41.148 (B41.48's DONE line, from Lens alone) — in a room whose owner has been inactive for 60 days, a member gives
// notice; the owner acting in the room answers it; once a later notice goes unanswered for 14 days, a vote by members
// holding most of the points makes another member the lead, a member may fork the room with lineage instead, and the
// new owner can hand the room on.
func TestRooms_InactiveOwnerIsReplacedByAPointsMajorityVote(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const owner, alice, bob, carol, outsider = "ws-b41148-owner", "ws-b41148-alice", "ws-b41148-bob", "ws-b41148-carol", "ws-b41148-out"
	marketStore := market.NewStore(pool)
	store := rooms.NewStore(pool, 3000)
	store.SetMarket(marketStore)
	r := chi.NewRouter()
	mountRoomRoutes(r, store)
	mountRoomTakeoverRoutes(r, store)
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	do := func(want int, v any, ws, method, path, body string) {
		t.Helper()
		code, out := call(ws, method, path, body)
		if code != want {
			t.Fatalf("%s %s as %s = %d %s, want %d", method, path, ws, code, out, want)
		}
		if v != nil {
			if err := json.Unmarshal([]byte(out), v); err != nil {
				t.Fatal(err)
			}
		}
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}

	var room rooms.Detail
	do(http.StatusCreated, &room, owner, http.MethodPost, "/v1/workspaces/"+owner+"/rooms", `{"title":"Shared build","terms":{"split_rule":"by_votes"}}`)
	base := "/v1/rooms/" + room.ID
	for _, ws := range []string{alice, bob, carol} {
		do(http.StatusCreated, nil, ws, http.MethodPost, base+"/join", `{"terms_version":1}`)
	}
	// Points: alice's accepted work has three votes from others, bob's one, carol has none — 4 in all.
	propose := func(ws string) string {
		var c rooms.Contribution
		do(http.StatusCreated, &c, ws, http.MethodPost, base+"/contributions", `{"kind":"prompt","title":"Work","artifact":{"template":"Say {{x}}."}}`)
		do(http.StatusOK, nil, owner, http.MethodPatch, base+"/contributions/"+c.ID, `{"status":"accepted"}`)
		return c.ID
	}
	aliceWork, bobWork := propose(alice), propose(bob)
	for _, ws := range []string{owner, bob, carol} {
		do(http.StatusOK, nil, ws, http.MethodPut, base+"/contributions/"+aliceWork+"/vote", `{"value":1}`)
	}
	do(http.StatusOK, nil, alice, http.MethodPut, base+"/contributions/"+bobWork+"/vote", `{"value":1}`)

	// The old owner lets carol spend its wallet; that is not the new owner's grant to give.
	do(http.StatusOK, nil, owner, http.MethodPatch, base+"/members/"+carol, `{"may_spend":true}`)
	do(http.StatusOK, nil, owner, http.MethodPatch, base+"/members/"+carol, `{"role":"editor"}`)

	// The owner was active today: no notice yet. A non-member sees nothing.
	do(http.StatusConflict, nil, bob, http.MethodPost, base+"/takeover", "")
	do(http.StatusNotFound, nil, outsider, http.MethodGet, base+"/takeover", "")

	// 61 days of silence from the owner; bob gives notice and the room is told.
	away := func() {
		exec(`UPDATE rooms SET created_at = now() - interval '61 days' WHERE id = $1`, room.ID)
		exec(`UPDATE room_contributions SET created_at = now() - interval '61 days', decided_at = now() - interval '61 days' WHERE room_id = $1`, room.ID)
		exec(`UPDATE room_messages SET created_at = now() - interval '61 days' WHERE room_id = $1`, room.ID)
		exec(`UPDATE room_members SET joined_at = now() - interval '62 days' WHERE room_id = $1`, room.ID)
		exec(`UPDATE room_takeovers SET noticed_at = now() - interval '62 days', closed_at = now() - interval '61 days' WHERE room_id = $1`, room.ID)
	}
	away()
	var tk rooms.Takeover
	do(http.StatusCreated, &tk, bob, http.MethodPost, base+"/takeover", "")
	if tk.Notice == nil || tk.Notice.Status != rooms.TakeoverOpen || tk.Voting || tk.TotalPoints != 4 {
		t.Fatalf("after notice = %+v, want an open notice, no voting yet, 4 points", tk)
	}
	do(http.StatusConflict, nil, alice, http.MethodPut, base+"/takeover/vote", `{"candidate_workspace_id":"`+bob+`"}`)

	// The owner posts in the room: that answers the notice, and the 60 days start again.
	do(http.StatusCreated, nil, owner, http.MethodPost, base+"/messages", `{"body":"Still here."}`)
	do(http.StatusOK, &tk, carol, http.MethodGet, base+"/takeover", "")
	if tk.Notice == nil || tk.Notice.Status != rooms.TakeoverAnswered {
		t.Fatalf("after the owner posted = %+v, want the notice answered", tk.Notice)
	}
	do(http.StatusConflict, nil, bob, http.MethodPost, base+"/takeover", "")

	// Silent again; a new notice goes unanswered for 15 days.
	away()
	// Workspaces that join while the owner is away and upvote bob's work add nothing: only members there while the owner
	// was last active give points...
	for _, ws := range []string{"ws-b41148-s1", "ws-b41148-s2", "ws-b41148-s3"} {
		do(http.StatusCreated, nil, ws, http.MethodPost, base+"/join", `{"terms_version":1}`)
		do(http.StatusOK, nil, ws, http.MethodPut, base+"/contributions/"+bobWork+"/vote", `{"value":1}`)
	}
	do(http.StatusCreated, nil, bob, http.MethodPost, base+"/takeover", "")
	exec(`UPDATE room_takeovers SET noticed_at = now() - interval '15 days' WHERE room_id = $1 AND status = 'open'`, room.ID)

	// ...and after it: they add nothing either, as the notice counted the points before they came.
	do(http.StatusOK, nil, "ws-b41148-s1", http.MethodPut, base+"/contributions/"+bobWork+"/vote", `{"value":1}`)
	do(http.StatusCreated, nil, "ws-b41148-s4", http.MethodPost, base+"/join", `{"terms_version":1}`)
	do(http.StatusOK, nil, "ws-b41148-s4", http.MethodPut, base+"/contributions/"+bobWork+"/vote", `{"value":1}`)

	// Nor does carol, an editor, removing alice: the points are the ones the notice found, alice's 3 among them.
	do(http.StatusOK, nil, carol, http.MethodPatch, base+"/members/"+alice, `{"remove":true}`)

	// A member may fork the room instead; the fork names the original.
	var fork rooms.Detail
	do(http.StatusCreated, &fork, carol, http.MethodPost, base+"/fork", "")
	if fork.OwnerWorkspaceID != carol || fork.ForkedFromRoomID == nil || *fork.ForkedFromRoomID != room.ID || fork.Terms.SplitRule != rooms.SplitByVotes {
		t.Fatalf("fork = %+v, want carol's room forked from %s under by_votes", fork, room.ID)
	}

	// bob (1 point) and carol (0) for bob is not most of 4; alice (3), back in the room, joining them is.
	do(http.StatusOK, nil, carol, http.MethodPut, base+"/takeover/vote", `{"candidate_workspace_id":"`+bob+`"}`)
	do(http.StatusOK, &tk, bob, http.MethodPut, base+"/takeover/vote", `{"candidate_workspace_id":"`+bob+`"}`)
	if tk.OwnerWorkspaceID != owner || tk.Tally[bob] != 1 {
		t.Fatalf("after bob and carol = owner %s, tally %v; want the owner unchanged and bob on 1", tk.OwnerWorkspaceID, tk.Tally)
	}
	do(http.StatusCreated, nil, alice, http.MethodPost, base+"/join", `{"terms_version":1}`)
	do(http.StatusOK, &tk, alice, http.MethodPut, base+"/takeover/vote", `{"candidate_workspace_id":"`+bob+`"}`)
	var dbOwner, oldRole, newRole, status, elected string
	var carolSpends bool
	if err := pool.QueryRow(ctx, `SELECT r.owner_workspace_id,
			(SELECT may_spend FROM room_members WHERE room_id = r.id AND workspace_id = $4),
			(SELECT role FROM room_members WHERE room_id = r.id AND workspace_id = $2),
			(SELECT role FROM room_members WHERE room_id = r.id AND workspace_id = $3),
			t.status, t.elected_workspace_id
		FROM rooms r JOIN room_takeovers t ON t.room_id = r.id WHERE r.id = $1 ORDER BY t.noticed_at DESC LIMIT 1`,
		room.ID, owner, bob, carol).Scan(&dbOwner, &carolSpends, &oldRole, &newRole, &status, &elected); err != nil {
		t.Fatal(err)
	}
	if carolSpends {
		t.Fatal("carol kept may_spend from the old owner: the new owner's wallet is the new owner's to share")
	}
	if dbOwner != bob || newRole != rooms.RoleOwner || oldRole != rooms.RoleEditor || status != rooms.TakeoverElected || elected != bob {
		t.Fatalf("after alice's vote: owner %s (bob %s, old owner %s), notice %s for %q; want bob the owner, the old owner an editor",
			dbOwner, newRole, oldRole, status, elected)
	}
	if tk.Voting || tk.OwnerWorkspaceID != bob {
		t.Fatalf("takeover after the election = %+v", tk)
	}

	// The new owner hands the room on, within the new owner's plan; nobody else can.
	do(http.StatusForbidden, nil, carol, http.MethodPost, base+"/owner", `{"workspace_id":"`+carol+`"}`)
	for i := 0; i < 3; i++ {
		do(http.StatusCreated, nil, alice, http.MethodPost, "/v1/workspaces/"+alice+"/rooms", `{"title":"Alice's own"}`)
	}
	if code, body := call(bob, http.MethodPost, base+"/owner", `{"workspace_id":"`+alice+`"}`); code != http.StatusPaymentRequired ||
		!strings.Contains(body, "rooms_plan_limits") {
		t.Fatalf("handing a fourth public room to a free workspace = %d %s, want 402 naming rooms_plan_limits", code, body)
	}
	var after rooms.Detail
	do(http.StatusOK, &after, bob, http.MethodPost, base+"/owner", `{"workspace_id":"`+carol+`"}`)
	if after.OwnerWorkspaceID != carol {
		t.Fatalf("after the hand-over the owner is %s, want carol", after.OwnerWorkspaceID)
	}
}
