package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/rooms"
)

// B41.141 (B41.41's DONE line, from Lens alone) — in a synthetic workspace, a room with a 25% pool shows the exact
// wording to its members and nothing to a signed-out visitor or a non-member; only the owner sets it, within 10–50%;
// and a workspace that is not synthetic sees no shared-ownership switch at all until room_equity is cleared.
func TestRooms_SharedOwnershipIsIntentOnlyAndGatedOnRoomEquity(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const owner, member, stranger, live = "ws-b41141-owner", "ws-b41141-member", "ws-b41141-stranger", "ws-b41141-real"
	for _, ws := range []string{owner, member, stranger, live} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, $2)`, ws, ws != live); err != nil {
			t.Fatal(err)
		}
	}
	r := chi.NewRouter()
	store := rooms.NewStore(pool, 3000)
	mountRoomRoutes(r, store)
	mountRoomOwnershipRoutes(r, store)
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if ws != "" {
			req = req.WithContext(auth.WithAuthContext(req.Context(),
				&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-" + ws, Scopes: []string{auth.ScopeKeys}}))
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	must := func(want int, ws, method, path, body string) string {
		t.Helper()
		code, out := call(ws, method, path, body)
		if code != want {
			t.Fatalf("%s %s as %q = %d %s, want %d", method, path, ws, code, out, want)
		}
		return out
	}
	var room rooms.Detail
	_ = json.Unmarshal([]byte(must(http.StatusCreated, owner, http.MethodPost, "/v1/workspaces/"+owner+"/rooms", `{"title":"Shared build"}`)), &room)
	must(http.StatusCreated, member, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":1}`)
	path := "/v1/rooms/" + room.ID + "/shared-ownership"

	// The owner switches it on with the default pool; the member reads 25% and the exact wording.
	must(http.StatusOK, owner, http.MethodPut, path, `{"enabled":true}`)
	var o rooms.SharedOwnership
	_ = json.Unmarshal([]byte(must(http.StatusOK, member, http.MethodGet, path, "")), &o)
	if !o.Enabled || o.PoolBPS != 2500 || o.Notice != "No shares, options or rights arise unless and until a company is formed, a regulated partner adopts a Contributor Plan, and the Gate is met" {
		t.Fatalf("member sees %+v, want enabled, 2500 bps and the exact notice", o)
	}
	var stored int
	if err := pool.QueryRow(ctx, `SELECT pool_bps FROM room_shared_ownership WHERE room_id = $1 AND enabled`, room.ID).Scan(&stored); err != nil || stored != 2500 {
		t.Fatalf("stored pool = %d, %v; want 2500", stored, err)
	}

	// Nothing for a signed-out visitor or a non-member; a member may not set it; the pool stays within 10–50%.
	for _, ws := range []string{"", stranger} {
		if code, body := call(ws, http.MethodGet, path, ""); code != http.StatusNotFound || strings.Contains(body, "Contributor Plan") {
			t.Errorf("GET as %q = %d %s, want 404 and no wording", ws, code, body)
		}
	}
	must(http.StatusForbidden, member, http.MethodPut, path, `{"enabled":false}`)
	must(http.StatusBadRequest, owner, http.MethodPut, path, `{"enabled":true,"pool_bps":5001}`)
	must(http.StatusBadRequest, owner, http.MethodPut, path, `{"enabled":true,"pool_bps":999}`)
	must(http.StatusOK, owner, http.MethodPut, path, `{"enabled":true,"pool_bps":5000}`)

	// A workspace that is not synthetic sees no switch in its own room until an operator clears room_equity.
	var liveRoom rooms.Detail
	_ = json.Unmarshal([]byte(must(http.StatusCreated, live, http.MethodPost, "/v1/workspaces/"+live+"/rooms", `{"title":"Real build"}`)), &liveRoom)
	livePath := "/v1/rooms/" + liveRoom.ID + "/shared-ownership"
	must(http.StatusNotFound, live, http.MethodGet, livePath, "")
	must(http.StatusNotFound, live, http.MethodPut, livePath, `{"enabled":true}`)
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM room_shared_ownership WHERE room_id = $1`, liveRoom.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("the uncleared live room has %d rows, %v; want none", rows, err)
	}
	bank := economy.NewDualTokenStore(nil, pool, nil)
	if _, err := bank.ClearCapability(ctx, economy.CapabilityRoomEquity, "nicolai", economy.ClearanceTerms{Reference: "lawyer's wording approval Q44/Q54",
		Licence: "opinion-test", Partner: "Test counsel", Countries: []string{"GB"}, ExpiresAt: time.Now().Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	must(http.StatusOK, live, http.MethodPut, livePath, `{"enabled":true,"pool_bps":1000}`)
	if err := bank.RevokeClearance(ctx, economy.CapabilityRoomEquity, "nicolai", "test over"); err != nil {
		t.Fatal(err)
	}
	must(http.StatusNotFound, live, http.MethodGet, livePath, "")
}
