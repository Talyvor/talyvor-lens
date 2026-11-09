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
	"github.com/talyvor/lens/internal/rooms"
)

// B41.146 (B41.46's DONE line, from Lens alone) — a test room's tracker shows each test's progress from its usage
// records: Used counts the people not linked to the owner active in 6 of the last 8 weeks, linked people once; Shipped
// and Paid read "needs a published build" until a room can publish one. The owner may raise a threshold, and an
// attempt to lower one is refused.
func TestRooms_GateTrackerCountsUnlinkedActivePeopleAndThresholdsOnlyRise(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const owner, alice, bob, carol, erin, dan, frank = "ws-b41146-owner", "ws-b41146-alice", "ws-b41146-bob", "ws-b41146-carol",
		"ws-b41146-erin", "ws-b41146-dan", "ws-b41146-frank"
	members := []string{alice, bob, carol, erin, dan, frank}
	for _, ws := range append([]string{owner}, members...) {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	// Dan shares the owner's owner key; Erin shares Carol's card.
	for _, q := range []string{
		`INSERT INTO workspace_owner_links (workspace_id, owner_key) VALUES ('` + owner + `', 'ok-b41146'), ('` + dan + `', 'ok-b41146')`,
		`INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash) VALUES ('` + carol + `', 'fp-b41146'), ('` + erin + `', 'fp-b41146')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	store := rooms.NewStore(pool, 3000)
	r := chi.NewRouter()
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
	do(http.StatusCreated, &room, owner, http.MethodPost, "/v1/workspaces/"+owner+"/rooms", `{"title":"Gate build"}`)
	base := "/v1/rooms/" + room.ID
	for _, ws := range members {
		do(http.StatusCreated, nil, ws, http.MethodPost, base+"/join", `{"terms_version":1}`)
	}

	// Each writes one message in each of the weeks given, counted back from now; Frank's are all his agent's.
	active := func(ws, agent string, weeks ...int) {
		for _, k := range weeks {
			if _, err := pool.Exec(ctx, `INSERT INTO room_messages (id, room_id, author_workspace_id, author_agent_id, kind, body, created_at)
				VALUES ('rmsg_' || md5(random()::text), $1, $2, $3, 'text', 'hi', now() - interval '1 day' - make_interval(weeks => $4))`,
				room.ID, ws, agent, k); err != nil {
				t.Fatal(err)
			}
		}
	}
	active(owner, "", 0, 1, 2, 3, 4, 5, 6, 7)
	active(alice, "", 0, 1, 2, 3, 5, 7)     // 6 of 8: counts
	active(bob, "", 0, 1, 2, 3, 4)          // 5 of 8: does not
	active(carol, "", 0, 1, 2, 3, 4, 5, 6)  // 7 of 8: counts
	active(erin, "", 0, 1, 2, 3, 4, 5)      // 6 of 8, but linked to Carol: counted with her
	active(dan, "", 0, 1, 2, 3, 4, 5, 6, 7) // linked to the owner: does not
	active(frank, "agent-b41146", 0, 1, 2, 3, 4, 5, 6, 7)
	active(bob, "", 9, 10) // older than the last 8 weeks: still 5

	var g rooms.GateTracker
	do(http.StatusOK, &g, bob, http.MethodGet, base+"/gate", "")
	if g.Thresholds.ShippedWeeks != 8 || g.Thresholds.UsedPeople != 100 || g.Thresholds.UsedActiveWeeks != 6 ||
		g.Thresholds.UsedWindowWeeks != 8 || g.Thresholds.PaidUSDMicros != 1_000_000_000 || g.Thresholds.PaidPayers != 10 {
		t.Fatalf("the thresholds are %+v, want Talyvor's: 8 weeks, 100 people in 6 of 8 weeks, $1,000 from 10 payers", g.Thresholds)
	}
	if g.Used.People != 2 || g.Used.Status != rooms.GateInProgress {
		t.Fatalf("used is %+v, want 2 people (Alice, and Carol with Erin) in progress", g.Used)
	}
	if g.Shipped.Status != rooms.GateNeedsPublishedBuild || g.Shipped.Note != "needs a published build" ||
		g.Paid.Status != rooms.GateNeedsPublishedBuild || g.Paid.Note != "needs a published build" || g.Met {
		t.Fatalf("shipped is %+v and paid %+v (met %v), want both to need a published build", g.Shipped, g.Paid, g.Met)
	}

	// The owner sets stricter values: in 7 of 8 weeks only Carol counts.
	do(http.StatusOK, nil, owner, http.MethodPut, base+"/gate-thresholds", `{"used_people":150,"used_active_weeks":7}`)
	do(http.StatusOK, &g, alice, http.MethodGet, base+"/gate", "")
	if g.Thresholds.UsedPeople != 150 || g.Thresholds.UsedActiveWeeks != 7 || g.Used.People != 1 {
		t.Fatalf("after raising, the tracker is %+v, want 150 people in 7 of 8 weeks and 1 counted", g)
	}

	// Lowering one — below what the owner set, or below Talyvor's — is refused and changes nothing; only the owner sets them.
	for _, body := range []string{`{"used_people":120}`, `{"used_active_weeks":6}`, `{"paid_usd_micros":999999999}`, `{"shipped_weeks":4}`} {
		do(http.StatusConflict, nil, owner, http.MethodPut, base+"/gate-thresholds", body)
	}
	do(http.StatusForbidden, nil, alice, http.MethodPut, base+"/gate-thresholds", `{"used_people":500}`)
	do(http.StatusOK, &g, owner, http.MethodGet, base+"/gate", "")
	if g.Thresholds.UsedPeople != 150 || g.Thresholds.UsedActiveWeeks != 7 || g.Thresholds.PaidUSDMicros != 1_000_000_000 ||
		g.Thresholds.ShippedWeeks != 8 {
		t.Fatalf("after the refusals the thresholds are %+v, want them as the owner raised them", g.Thresholds)
	}

	// Anyone who is not a member does not see the tracker.
	do(http.StatusNotFound, nil, "ws-b41146-stranger", http.MethodGet, base+"/gate", "")
}
