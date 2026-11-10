package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/rooms"
)

// B32.28 — a created room has its creator as owner; another workspace joins under the current terms version and that
// version is recorded; a new terms version asks it again; a viewer cannot be given may_spend; an agent joins only as
// its owner's member; GET /v1/rooms lists an open public room and not a closed one.
func TestRooms_CreateJoinUnderTermsRolesAndTheOpenList(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const owner, joiner, outsider = "ws-room-owner", "ws-room-joiner", "ws-room-outsider"
	r := chi.NewRouter()
	mountRoomRoutes(r, rooms.NewStore(pool, 3000))
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	create := func(title string) rooms.Detail {
		t.Helper()
		code, body := call(owner, http.MethodPost, "/v1/workspaces/"+owner+"/rooms",
			`{"title":"`+title+`","topic":"Translation","terms":{"split_rule":"equal","remix_share_bps":1000,"default_price_usd_micros":20000,"spend_policy":"members_with_spend"}}`)
		var d rooms.Detail
		if err := json.Unmarshal([]byte(body), &d); code != http.StatusCreated || err != nil {
			t.Fatalf("create %q = %d %s", title, code, body)
		}
		return d
	}
	row := func(roomID, ws string) (role string, maySpend bool, terms int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT role, may_spend, terms_version FROM room_members WHERE room_id = $1 AND workspace_id = $2`,
			roomID, ws).Scan(&role, &maySpend, &terms); err != nil {
			t.Fatalf("member row %s/%s: %v", roomID, ws, err)
		}
		return role, maySpend, terms
	}

	// The creator is the owner, with the room's budget, at terms version 1.
	room := create("French product copy")
	if room.OwnerWorkspaceID != owner || room.Me == nil || room.Me.Role != rooms.RoleOwner || room.Terms.Version != 1 ||
		room.Terms.SplitRule != rooms.SplitEqual || room.Terms.DefaultPriceUSDMicros != 20000 {
		t.Fatalf("created room = %+v, want its creator as owner at terms version 1", room)
	}
	if role, maySpend, terms := row(room.ID, owner); role != "owner" || !maySpend || terms != 1 {
		t.Fatalf("owner row = %s %v v%d, want owner, may_spend, v1", role, maySpend, terms)
	}

	// Joining must accept the current terms: a wrong version is 409 and writes no row; the right one records it.
	if code, body := call(joiner, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":2}`); code != http.StatusConflict {
		t.Fatalf("join with terms v2 = %d %s, want 409", code, body)
	}
	if code, body := call(joiner, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":1}`); code != http.StatusCreated ||
		!strings.Contains(body, `"terms_version":1`) || !strings.Contains(body, `"role":"member"`) {
		t.Fatalf("join = %d %s, want 201 as a member at terms v1", code, body)
	}
	if role, maySpend, terms := row(room.ID, joiner); role != "member" || maySpend || terms != 1 {
		t.Fatalf("joiner row = %s %v v%d, want member without budget at v1", role, maySpend, terms)
	}

	// New terms ask the member again: its row keeps v1 and reads not current until it joins under v2.
	if code, body := call(owner, http.MethodPut, "/v1/rooms/"+room.ID+"/terms",
		`{"split_rule":"by_votes","remix_share_bps":1000,"default_price_usd_micros":20000,"spend_policy":"members_with_spend"}`); code != http.StatusOK ||
		!strings.Contains(body, `"version":2`) {
		t.Fatalf("new terms = %d %s, want version 2", code, body)
	}
	if _, body := call(joiner, http.MethodGet, "/v1/rooms/"+room.ID, ""); !strings.Contains(body, `"terms_version":1,"terms_current":false`) {
		t.Fatalf("the joiner after new terms = %s, want its v1 acceptance marked not current", body)
	}
	if code, body := call(joiner, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":2}`); code != http.StatusOK {
		t.Fatalf("accept v2 = %d %s, want 200", code, body)
	}
	// Another workspace's user ids are not shown to the room.
	if _, body := call(joiner, http.MethodGet, "/v1/rooms/"+room.ID, ""); strings.Contains(body, "user-"+owner) || !strings.Contains(body, "user-"+joiner) {
		t.Fatalf("the room read by the joiner = %s, want its own user id and not the owner's", body)
	}
	if _, _, terms := row(room.ID, joiner); terms != 2 {
		t.Fatalf("joiner terms after accepting = v%d, want v2", terms)
	}

	// A viewer cannot be given may_spend; a member can, and becoming a viewer takes it away.
	if code, body := call(owner, http.MethodPatch, "/v1/rooms/"+room.ID+"/members/"+joiner, `{"role":"viewer","may_spend":true}`); code != http.StatusBadRequest ||
		!strings.Contains(body, "A viewer cannot be given may_spend") {
		t.Fatalf("viewer with may_spend = %d %s, want 400", code, body)
	}
	if code, body := call(owner, http.MethodPatch, "/v1/rooms/"+room.ID+"/members/"+joiner, `{"may_spend":true}`); code != http.StatusOK {
		t.Fatalf("member may_spend = %d %s", code, body)
	}
	if code, body := call(owner, http.MethodPatch, "/v1/rooms/"+room.ID+"/members/"+joiner, `{"role":"viewer"}`); code != http.StatusOK {
		t.Fatalf("make viewer = %d %s", code, body)
	}
	if role, maySpend, _ := row(room.ID, joiner); role != "viewer" || maySpend {
		t.Fatalf("viewer row = %s may_spend %v, want a viewer without the budget", role, maySpend)
	}
	// Only the owner or an editor changes members.
	if code, _ := call(joiner, http.MethodPatch, "/v1/rooms/"+room.ID+"/members/"+owner, `{"role":"member"}`); code != http.StatusBadRequest {
		t.Fatalf("a viewer changing the owner = %d, want 400", code)
	}

	// An agent joins only as its owner's member.
	if _, err := pool.Exec(ctx, `INSERT INTO agent_accounts (id, workspace_id, name) VALUES ('agt_room_out', $1, 'Outsider bot')`, outsider); err != nil {
		t.Fatal(err)
	}
	if code, body := call(outsider, http.MethodPost, "/v1/rooms/"+room.ID+"/agents", `{"agent_id":"agt_room_out"}`); code != http.StatusForbidden {
		t.Fatalf("a non-member's agent = %d %s, want 403", code, body)
	}
	if code, body := call(outsider, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":2}`); code != http.StatusCreated {
		t.Fatalf("outsider join = %d %s", code, body)
	}
	if code, body := call(owner, http.MethodPost, "/v1/rooms/"+room.ID+"/agents", `{"agent_id":"agt_room_out"}`); code != http.StatusNotFound {
		t.Fatalf("someone else's agent = %d %s, want 404", code, body)
	}
	if code, body := call(outsider, http.MethodPost, "/v1/rooms/"+room.ID+"/agents", `{"agent_id":"agt_room_out"}`); code != http.StatusCreated ||
		!strings.Contains(body, `"workspace_id":"`+outsider+`"`) {
		t.Fatalf("its owner's agent = %d %s, want 201", code, body)
	}

	// GET /v1/rooms lists the open public room and not a closed one; the rooms you are in come beside it.
	closed := create("Closed room")
	if _, err := pool.Exec(ctx, `UPDATE rooms SET status = 'closed' WHERE id = $1`, closed.ID); err != nil {
		t.Fatal(err)
	}
	code, body := call(outsider, http.MethodGet, "/v1/rooms?topic=translation", "")
	var list struct {
		Rooms  []rooms.Room `json:"rooms"`
		Joined []rooms.Room `json:"joined"`
	}
	if err := json.Unmarshal([]byte(body), &list); code != http.StatusOK || err != nil {
		t.Fatalf("list = %d %s", code, body)
	}
	if len(list.Rooms) != 1 || list.Rooms[0].ID != room.ID || list.Rooms[0].MemberCount != 3 {
		t.Fatalf("open rooms = %+v, want only %s with its 3 members", list.Rooms, room.ID)
	}
	if len(list.Joined) != 1 || list.Joined[0].ID != room.ID {
		t.Fatalf("joined = %+v, want %s", list.Joined, room.ID)
	}
	if code, _ := call(joiner, http.MethodPost, "/v1/rooms/"+closed.ID+"/join", `{"terms_version":1}`); code != http.StatusConflict {
		t.Fatalf("join a closed room = %d, want 409", code)
	}
}

// B32.29 — a Free workspace's fourth public room and its first private room are refused naming rooms_plan_limits, and
// with a team subscription both open; a private room is 404 to a non-member; an invite link admits a member until it
// is revoked and then answers 404; a workspace the owner names sees the room and joins it; the 51st member and the
// 11th agent of a Free owner's room are refused.
func TestRooms_PrivateRoomsInviteLinksAndPlanLimits(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const owner, guest, late, named, freeOwner = "ws-b3229-owner", "ws-b3229-guest", "ws-b3229-late", "ws-b3229-named", "ws-b3229-free"
	r := chi.NewRouter()
	mountRoomRoutes(r, rooms.NewStore(pool, 3000))
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	open := func(ws, title, visibility string) (int, string) {
		t.Helper()
		return call(ws, http.MethodPost, "/v1/workspaces/"+ws+"/rooms", `{"title":"`+title+`","visibility":"`+visibility+`"}`)
	}
	count := func(q string, args ...any) (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}

	// Free: three public rooms, then the fourth and the first private one are refused naming the setting and the plan.
	for _, title := range []string{"One", "Two", "Three"} {
		if code, body := open(owner, title, "public"); code != http.StatusCreated {
			t.Fatalf("free public room %s = %d %s", title, code, body)
		}
	}
	if code, body := open(owner, "Four", "public"); code != http.StatusPaymentRequired ||
		!strings.Contains(body, `"setting":"rooms_plan_limits"`) || !strings.Contains(body, `"error":"Free allows 3 public rooms, and you have 3 open. Team allows 25."`) {
		t.Fatalf("free fourth public room = %d %s, want 402 naming rooms_plan_limits and the free plan", code, body)
	}
	if code, body := open(owner, "Secret", "private"); code != http.StatusPaymentRequired ||
		!strings.Contains(body, `"setting":"rooms_plan_limits"`) || !strings.Contains(body, `"error":"Free allows no private rooms. Team allows 10."`) {
		t.Fatalf("free private room = %d %s, want 402 naming rooms_plan_limits and the free plan", code, body)
	}
	if n := count(`SELECT count(*) FROM rooms WHERE owner_workspace_id = $1`, owner); n != 3 {
		t.Fatalf("rooms after the refusals = %d, want 3", n)
	}

	// On a team subscription both open.
	if _, err := pool.Exec(ctx, `INSERT INTO subscriptions (workspace_id, stripe_subscription_id, stripe_customer_id, price_id,
		status, livemode, last_event_at, plan, byok) VALUES ($1, 'sub_b3229', 'cus_b3229', 'price_team', 'active', false, NOW(), 'team', false)`,
		owner); err != nil {
		t.Fatal(err)
	}
	if code, body := open(owner, "Four", "public"); code != http.StatusCreated {
		t.Fatalf("team fourth public room = %d %s", code, body)
	}
	code, body := open(owner, "Secret", "private")
	var priv rooms.Detail
	if err := json.Unmarshal([]byte(body), &priv); code != http.StatusCreated || err != nil || priv.Visibility != rooms.Private {
		t.Fatalf("team private room = %d %s", code, body)
	}

	// A private room is 404 to a non-member, absent from the open list, and cannot be joined without an invite.
	if code, _ := call(guest, http.MethodGet, "/v1/rooms/"+priv.ID, ""); code != http.StatusNotFound {
		t.Fatalf("a non-member reading the private room = %d, want 404", code)
	}
	if _, body := call(guest, http.MethodGet, "/v1/rooms", ""); strings.Contains(body, priv.ID) {
		t.Fatalf("the open list shows the private room to a non-member: %s", body)
	}
	if code, _ := call(guest, http.MethodPost, "/v1/rooms/"+priv.ID+"/join", `{"terms_version":1}`); code != http.StatusNotFound {
		t.Fatalf("joining the private room without an invite = %d, want 404", code)
	}

	// An invite link shows the room and its terms, admits a member and counts the use.
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	code, body = call(owner, http.MethodPost, "/v1/rooms/"+priv.ID+"/invites", `{"max_uses":5,"expires_at":"`+expires+`"}`)
	var link rooms.Invite
	if err := json.Unmarshal([]byte(body), &link); code != http.StatusCreated || err != nil || link.Token == "" {
		t.Fatalf("invite link = %d %s", code, body)
	}
	if code, body := call(guest, http.MethodGet, "/v1/room-invites/"+link.Token, ""); code != http.StatusOK ||
		!strings.Contains(body, priv.ID) || !strings.Contains(body, `"uses_left":5`) {
		t.Fatalf("the link's preview = %d %s, want the room and 5 uses left", code, body)
	}
	if code, body := call(guest, http.MethodPost, "/v1/room-invites/"+link.Token+"/join", `{"terms_version":1}`); code != http.StatusCreated {
		t.Fatalf("join by the link = %d %s, want 201", code, body)
	}
	if n := count(`SELECT count(*) FROM room_members WHERE room_id = $1 AND workspace_id = $2 AND removed_at IS NULL`, priv.ID, guest); n != 1 {
		t.Fatalf("the guest's membership rows = %d, want 1", n)
	}
	if n := count(`SELECT uses FROM room_invites WHERE id = $1`, link.ID); n != 1 {
		t.Fatalf("the link's uses = %d, want 1", n)
	}
	if code, _ := call(guest, http.MethodGet, "/v1/rooms/"+priv.ID, ""); code != http.StatusOK {
		t.Fatalf("the new member reading the private room = %d, want 200", code)
	}
	if _, body := call(owner, http.MethodGet, "/v1/rooms/"+priv.ID+"/invites", ""); strings.Contains(body, link.Token) {
		t.Fatalf("the invite list shows a link's token: %s", body)
	}

	// Revoked, the link answers 404 and admits nobody.
	if code, body := call(owner, http.MethodDelete, "/v1/rooms/"+priv.ID+"/invites/"+link.ID, ""); code != http.StatusOK ||
		!strings.Contains(body, `"live":false`) {
		t.Fatalf("revoke = %d %s", code, body)
	}
	if code, _ := call(late, http.MethodGet, "/v1/room-invites/"+link.Token, ""); code != http.StatusNotFound {
		t.Fatalf("a revoked link's preview = %d, want 404", code)
	}
	if code, _ := call(late, http.MethodPost, "/v1/room-invites/"+link.Token+"/join", `{"terms_version":1}`); code != http.StatusNotFound {
		t.Fatalf("joining by a revoked link = %d, want 404", code)
	}
	if n := count(`SELECT count(*) FROM room_members WHERE room_id = $1 AND workspace_id = $2`, priv.ID, late); n != 0 {
		t.Fatalf("a revoked link wrote %d membership rows, want 0", n)
	}

	// The owner names a workspace: it is invited, sees the room and joins it, which uses its invite.
	if code, body := call(owner, http.MethodPost, "/v1/rooms/"+priv.ID+"/invites", `{"workspace_id":"`+named+`"}`); code != http.StatusCreated {
		t.Fatalf("name a workspace = %d %s", code, body)
	}
	if _, body := call(named, http.MethodGet, "/v1/rooms", ""); !strings.Contains(body, `"invited":[{"id":"`+priv.ID+`"`) {
		t.Fatalf("the named workspace's list = %s, want the room among its invitations", body)
	}
	if code, body := call(named, http.MethodPost, "/v1/rooms/"+priv.ID+"/join", `{"terms_version":1}`); code != http.StatusCreated {
		t.Fatalf("the named workspace joining = %d %s, want 201", code, body)
	}
	if n := count(`SELECT uses FROM room_invites WHERE room_id = $1 AND workspace_id = $2`, priv.ID, named); n != 1 {
		t.Fatalf("the named invite's uses = %d, want 1", n)
	}
	// Removed, it cannot come back on the invite it already used.
	if code, body := call(owner, http.MethodPatch, "/v1/rooms/"+priv.ID+"/members/"+named, `{"remove":true}`); code != http.StatusOK {
		t.Fatalf("remove the named member = %d %s", code, body)
	}
	if code, _ := call(named, http.MethodPost, "/v1/rooms/"+priv.ID+"/join", `{"terms_version":1}`); code != http.StatusNotFound {
		t.Fatalf("a removed member rejoining the private room = %d, want 404", code)
	}

	// A Free owner's room holds 50 members, the owner included: the 51st is refused and writes no row.
	code, body = open(freeOwner, "Crowded", "public")
	var crowded rooms.Detail
	if err := json.Unmarshal([]byte(body), &crowded); code != http.StatusCreated || err != nil {
		t.Fatalf("free room = %d %s", code, body)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO room_members (room_id, workspace_id, role, terms_version)
		SELECT $1, 'ws-b3229-filler-' || g, 'member', 1 FROM generate_series(1, 49) g`, crowded.ID); err != nil {
		t.Fatal(err)
	}
	if code, body := call(late, http.MethodPost, "/v1/rooms/"+crowded.ID+"/join", `{"terms_version":1}`); code != http.StatusPaymentRequired ||
		!strings.Contains(body, `"setting":"rooms_plan_limits"`) || !strings.Contains(body, `"error":"The room owner’s plan, Free, allows 50 members in a room."`) {
		t.Fatalf("the 51st member = %d %s, want 402 naming rooms_plan_limits and the free plan", code, body)
	}
	if n := count(`SELECT count(*) FROM room_members WHERE room_id = $1 AND removed_at IS NULL`, crowded.ID); n != 50 {
		t.Fatalf("members after the refusal = %d, want 50", n)
	}

	// ... and 10 agents: the 11th is refused.
	if _, err := pool.Exec(ctx, `INSERT INTO agent_accounts (id, workspace_id, name)
		SELECT 'agt_b3229_' || g, $1, 'Bot ' || g FROM generate_series(1, 11) g`, freeOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO room_member_agents (room_id, agent_id, workspace_id)
		SELECT $1, 'agt_b3229_' || g, $2 FROM generate_series(1, 10) g`, crowded.ID, freeOwner); err != nil {
		t.Fatal(err)
	}
	if code, body := call(freeOwner, http.MethodPost, "/v1/rooms/"+crowded.ID+"/agents", `{"agent_id":"agt_b3229_11"}`); code != http.StatusPaymentRequired ||
		!strings.Contains(body, `"error":"The room owner’s plan, Free, allows 10 agents in a room."`) {
		t.Fatalf("the 11th agent = %d %s, want 402 naming the free plan", code, body)
	}
}

// B32.30 — in a public room a message carrying an API key is refused with the finding and writes no row; another
// member's event stream, through the router's gzip and rate-limit-header writers, receives a posted message within two
// seconds; its author edits it and the owner removes it, leaving a tombstone; a non-member reading a private room's
// messages gets 404; a member's 21st message in a minute is refused.
func TestRooms_MessagesScannedStreamedEditedDeletedAndLimited(t *testing.T) {
	pool := overPgBouncer(t, agentRoutesDB(t))
	ctx := context.Background()
	const owner, joiner, outsider = "ws-b3230-owner", "ws-b3230-joiner", "ws-b3230-outsider"
	r := chi.NewRouter()
	r.Use(api.GzipMiddleware, api.RateLimitHeadersMiddleware)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ws := req.Header.Get("X-Test-Workspace")
			next.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(),
				&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-" + ws, Scopes: []string{auth.ScopeKeys}})))
		})
	})
	mountRoomRoutes(r, rooms.NewStore(pool, 3000))
	srv := httptest.NewServer(r)
	defer srv.Close()
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("X-Test-Workspace", ws)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	count := func(q string, args ...any) (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	var room rooms.Detail
	if code, body := call(owner, http.MethodPost, "/v1/workspaces/"+owner+"/rooms", `{"title":"Launch copy"}`); code != http.StatusCreated ||
		json.Unmarshal([]byte(body), &room) != nil {
		t.Fatalf("create = %d %s", code, body)
	}
	if code, body := call(joiner, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":1}`); code != http.StatusCreated {
		t.Fatalf("join = %d %s", code, body)
	}

	// A public room's message carrying an API key is refused with what the scan found, and writes no row.
	if code, body := call(joiner, http.MethodPost, "/v1/rooms/"+room.ID+"/messages",
		`{"body":"try my key sk-proj-a1b2c3d4e5f6g7h8i9j0k1l2m3n4"}`); code != http.StatusUnprocessableEntity ||
		!strings.Contains(body, `"secrets":["openai_key"]`) || !strings.Contains(body, "contains a secret (openai_key)") {
		t.Fatalf("a key in a public room = %d %s, want 422 naming openai_key", code, body)
	}
	if n := count(`SELECT count(*) FROM room_messages WHERE room_id = $1`, room.ID); n != 0 {
		t.Fatalf("the refused message wrote %d rows, want 0", n)
	}

	// The joiner's stream, opened from now, receives the owner's message within two seconds.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/rooms/"+room.ID+"/events", nil)
	req.Header.Set("X-Test-Workspace", joiner)
	req.Header.Set("Accept-Encoding", "gzip")
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(streamCtx))
	if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("open the stream = %v %v", resp, err)
	}
	defer resp.Body.Close()
	lines := bufio.NewReader(resp.Body)
	if l, err := lines.ReadString('\n'); err != nil || !strings.HasPrefix(l, "retry: ") {
		t.Fatalf("the stream's first line = %q %v, want its retry", l, err)
	}
	sent := time.Now()
	var msg rooms.Message
	if code, body := call(owner, http.MethodPost, "/v1/rooms/"+room.ID+"/messages", `{"body":"Draft one is in the doc."}`); code != http.StatusCreated ||
		json.Unmarshal([]byte(body), &msg) != nil {
		t.Fatalf("post = %d %s", code, body)
	}
	got := make(chan string, 1)
	go func() {
		for {
			l, err := lines.ReadString('\n')
			if err != nil {
				got <- "error: " + err.Error()
				return
			}
			if strings.HasPrefix(l, "data: ") && strings.Contains(l, msg.ID) {
				got <- l
				return
			}
		}
	}()
	select {
	case l := <-got:
		if !strings.Contains(l, `"kind":"message.posted"`) || !strings.Contains(l, "Draft one is in the doc.") {
			t.Fatalf("the stream's event = %s, want the posted message", l)
		}
		t.Logf("MEASURED: the joiner's stream received the message %v after it was posted", time.Since(sent).Round(time.Millisecond))
	case <-time.After(2 * time.Second):
		t.Fatal("the joiner's stream did not receive the posted message within two seconds")
	}
	cancel()

	// Its author edits it; another member may not; the owner removes the joiner's message and a tombstone stays.
	if code, body := call(joiner, http.MethodPatch, "/v1/rooms/"+room.ID+"/messages/"+msg.ID, `{"body":"mine now"}`); code != http.StatusForbidden {
		t.Fatalf("another member editing = %d %s, want 403", code, body)
	}
	if code, body := call(owner, http.MethodPatch, "/v1/rooms/"+room.ID+"/messages/"+msg.ID, `{"body":"Draft two is in the doc."}`); code != http.StatusOK ||
		!strings.Contains(body, `"edited_at"`) {
		t.Fatalf("the author editing = %d %s", code, body)
	}
	var theirs rooms.Message
	if code, body := call(joiner, http.MethodPost, "/v1/rooms/"+room.ID+"/messages", `{"body":"Off topic."}`); code != http.StatusCreated ||
		json.Unmarshal([]byte(body), &theirs) != nil {
		t.Fatalf("the joiner's post = %d %s", code, body)
	}
	if code, body := call(owner, http.MethodDelete, "/v1/rooms/"+room.ID+"/messages/"+theirs.ID, ""); code != http.StatusOK {
		t.Fatalf("the owner removing = %d %s", code, body)
	}
	var body, by string
	var deleted bool
	if err := pool.QueryRow(ctx, `SELECT body, deleted_at IS NOT NULL, deleted_by_workspace_id FROM room_messages WHERE id = $1`, theirs.ID).
		Scan(&body, &deleted, &by); err != nil || body != "" || !deleted || by != owner {
		t.Fatalf("the removed message's row = %q deleted %v by %q (%v), want an empty tombstone removed by the owner", body, deleted, by, err)
	}
	var page rooms.Page
	if code, b := call(outsider, http.MethodGet, "/v1/rooms/"+room.ID+"/messages", ""); code != http.StatusOK || json.Unmarshal([]byte(b), &page) != nil ||
		len(page.Messages) != 2 || page.Messages[0].Body != "Draft two is in the doc." || page.Messages[1].DeletedAt == nil || page.EventsCursor != 4 {
		t.Fatalf("a public room read by anyone = %d %s, want the edited message, the tombstone and events_cursor 4", code, b)
	}

	// A private room's messages are 404 to a non-member.
	if _, err := pool.Exec(ctx, `INSERT INTO subscriptions (workspace_id, stripe_subscription_id, stripe_customer_id, price_id,
		status, livemode, last_event_at, plan, byok) VALUES ($1, 'sub_b3230', 'cus_b3230', 'price_team', 'active', false, NOW(), 'team', false)`,
		owner); err != nil {
		t.Fatal(err)
	}
	var priv rooms.Detail
	if code, b := call(owner, http.MethodPost, "/v1/workspaces/"+owner+"/rooms", `{"title":"Pricing","visibility":"private"}`); code != http.StatusCreated ||
		json.Unmarshal([]byte(b), &priv) != nil {
		t.Fatalf("private room = %d %s", code, b)
	}
	if code, b := call(owner, http.MethodPost, "/v1/rooms/"+priv.ID+"/messages", `{"body":"Our margin target is in the sheet."}`); code != http.StatusCreated {
		t.Fatalf("a private room's post = %d %s", code, b)
	}
	if code, _ := call(outsider, http.MethodGet, "/v1/rooms/"+priv.ID+"/messages", ""); code != http.StatusNotFound {
		t.Fatalf("a non-member reading the private room's messages = %d, want 404", code)
	}
	if code, _ := call(outsider, http.MethodGet, "/v1/rooms/"+priv.ID+"/events?after=0", ""); code != http.StatusNotFound {
		t.Fatalf("a non-member streaming the private room = %d, want 404", code)
	}

	// The joiner has posted one message this minute: nineteen more are taken, and the 21st is refused and writes no row.
	for i := 2; i <= 20; i++ {
		if code, b := call(joiner, http.MethodPost, "/v1/rooms/"+room.ID+"/messages", fmt.Sprintf(`{"body":"note %d"}`, i)); code != http.StatusCreated {
			t.Fatalf("message %d = %d %s", i, code, b)
		}
	}
	code, b := call(joiner, http.MethodPost, "/v1/rooms/"+room.ID+"/messages", `{"body":"note 21"}`)
	if code != http.StatusTooManyRequests || !strings.Contains(b, "LENS_ROOM_MESSAGES_PER_MINUTE") || !strings.Contains(b, `"per_minute":20`) {
		t.Fatalf("the 21st message = %d %s, want 429 naming LENS_ROOM_MESSAGES_PER_MINUTE", code, b)
	}
	if n := count(`SELECT count(*) FROM room_messages WHERE room_id = $1 AND author_workspace_id = $2`, room.ID, joiner); n != 20 {
		t.Fatalf("the joiner's rows = %d, want 20", n)
	}
}

// B55.34 — a room's members, its messages' authors and its named invites carry their workspace's name, read from
// workspaces.name; a workspace whose name is still its id reads "A workspace not named yet". A named invite carries the
// name only once its workspace has been a member: naming an id must not tell the owner whose it is.
func TestRooms_MembersAuthorsAndInvitesCarryWorkspaceNames(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ana, ben, cy, dee = "ws-b5534-ana", "ws-b5534-ben", "ws-b5534-cy", "ws-b5534-dee"
	for id, name := range map[string]string{ana: "Ana's studio", ben: "Ben's lab", cy: cy, dee: "Dee's desk"} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $2, $1)`, id, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO subscriptions (workspace_id, stripe_subscription_id, stripe_customer_id, price_id,
		status, livemode, last_event_at, plan, byok) VALUES ($1, 'sub_b5534', 'cus_b5534', 'price_team', 'active', false, NOW(), 'team', false)`,
		ana); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountRoomRoutes(r, rooms.NewStore(pool, 3000))
	call := func(ws, method, path, body string, want int, into any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != want || (into != nil && json.Unmarshal(w.Body.Bytes(), into) != nil) {
			t.Fatalf("%s %s as %s = %d %s, want %d", method, path, ws, w.Code, w.Body.String(), want)
		}
	}
	var room rooms.Detail
	call(ana, http.MethodPost, "/v1/workspaces/"+ana+"/rooms", `{"title":"Names","visibility":"private"}`, http.StatusCreated, &room)
	for _, ws := range []string{ben, cy, dee} {
		call(ana, http.MethodPost, "/v1/rooms/"+room.ID+"/invites", `{"workspace_id":"`+ws+`"}`, http.StatusCreated, nil)
	}
	for _, ws := range []string{ben, cy} {
		call(ws, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":1}`, http.StatusCreated, nil)
	}

	// A member reads every member by its workspace's name.
	var seen rooms.Detail
	call(ben, http.MethodGet, "/v1/rooms/"+room.ID, "", http.StatusOK, &seen)
	names := map[string]string{}
	for _, m := range seen.Members {
		names[m.WorkspaceID] = m.WorkspaceName
	}
	if want := map[string]string{ana: "Ana's studio", ben: "Ben's lab", cy: "A workspace not named yet"}; fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("members by name = %v, want %v", names, want)
	}

	// Ana's message carries her workspace's name.
	call(ana, http.MethodPost, "/v1/rooms/"+room.ID+"/messages", `{"body":"Welcome in."}`, http.StatusCreated, nil)
	var page rooms.Page
	call(ben, http.MethodGet, "/v1/rooms/"+room.ID+"/messages", "", http.StatusOK, &page)
	if len(page.Messages) != 1 || page.Messages[0].AuthorWorkspaceName != "Ana's studio" {
		t.Fatalf("messages = %+v, want Ana's carrying \"Ana's studio\"", page.Messages)
	}

	// Ben's invite, used, names Ben's workspace; Dee's, still waiting, does not name Dee's.
	var invites struct{ Invites []rooms.Invite }
	call(ana, http.MethodGet, "/v1/rooms/"+room.ID+"/invites", "", http.StatusOK, &invites)
	named := map[string]string{}
	for _, i := range invites.Invites {
		named[i.WorkspaceID] = i.WorkspaceName
	}
	if want := map[string]string{ben: "Ben's lab", cy: "A workspace not named yet", dee: ""}; fmt.Sprint(named) != fmt.Sprint(want) {
		t.Fatalf("invites by name = %v, want %v", named, want)
	}
}
