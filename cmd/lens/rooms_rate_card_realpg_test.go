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

// B41.142 (B41.42's DONE line, from Lens alone) — the owner publishes points for each size of work (hours × the
// points per hour of its band), no size worth more than 5% of the pool; a contribution claimed at size M earns M's
// points when accepted, a later rate card does not change them, and an attempt to raise them afterwards is refused.
func TestRooms_RateCardFixesPointsBeforeTheWorkAndRefusesARaiseAfterDelivery(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const owner, member = "ws-b41142-owner", "ws-b41142-member"
	for _, ws := range []string{owner, member} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	marketStore := market.NewStore(pool)
	store := rooms.NewStore(pool, 3000)
	store.SetMarket(marketStore)
	r := chi.NewRouter()
	mountMarketRoutes(r, marketStore)
	mountRoomRoutes(r, store)
	mountRoomOwnershipRoutes(r, store)
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
		if code != want || (v != nil && json.Unmarshal([]byte(out), v) != nil) {
			t.Fatalf("%s %s as %s = %d %s, want %d", method, path, ws, code, out, want)
		}
	}
	var room rooms.Detail
	do(http.StatusCreated, &room, owner, http.MethodPost, "/v1/workspaces/"+owner+"/rooms", `{"title":"Shared build"}`)
	do(http.StatusCreated, nil, member, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":1}`)
	card := "/v1/rooms/" + room.ID + "/rate-card"

	// Only the owner publishes it, and no size may be worth more than 5% of the pool: 600 of 10000 is refused.
	sizes := `{"pool_points":10000,"sizes":[{"size":"S","hours":2,"points_per_hour":50},{"size":"M","hours":8,"points_per_hour":50},` +
		`{"size":"L","hours":10,"points_per_hour":50}]}`
	do(http.StatusForbidden, nil, member, http.MethodPut, card, sizes)
	do(http.StatusBadRequest, nil, owner, http.MethodPut, card, strings.Replace(sizes, `"hours":10`, `"hours":12`, 1))
	var rc rooms.RateCard
	do(http.StatusOK, &rc, owner, http.MethodPut, card, sizes)
	do(http.StatusOK, &rc, member, http.MethodGet, card, "")
	if rc.PoolPoints != 10000 || rc.ItemCapPoints != 500 || len(rc.Sizes) != 3 || rc.Sizes[1].Size != "M" || rc.Sizes[1].Points != 400 {
		t.Fatalf("the member reads %+v, want M at 8 h × 50 = 400 points, capped at 500", rc)
	}

	// The member claims size M for its contribution before the work is accepted.
	var c rooms.Contribution
	do(http.StatusCreated, &c, member, http.MethodPost, "/v1/rooms/"+room.ID+"/contributions",
		`{"kind":"prompt","title":"Onboarding copy","artifact":{"template":"Welcome {{name}}."}}`)
	size := "/v1/rooms/" + room.ID + "/contributions/" + c.ID + "/size"
	do(http.StatusBadRequest, nil, member, http.MethodPut, size, `{"size":"XL"}`)
	do(http.StatusOK, &c, member, http.MethodPut, size, `{"size":"M"}`)
	do(http.StatusConflict, nil, member, http.MethodPut, size, `{"size":"L"}`) // the author claims once

	// The owner then makes M worth more; the claim keeps the points fixed when it was made.
	do(http.StatusOK, nil, owner, http.MethodPut, card, strings.Replace(sizes, `"hours":8`, `"hours":9`, 1))
	do(http.StatusOK, &c, owner, http.MethodPatch, "/v1/rooms/"+room.ID+"/contributions/"+c.ID, `{"status":"accepted"}`)
	if c.Status != rooms.ContributionAccepted || c.Size != "M" || c.Points != 400 {
		t.Fatalf("accepted = %+v, want size M at its claimed 400 points", c)
	}

	// After delivery no one raises its points, not even the owner; the row keeps M's 400.
	do(http.StatusConflict, nil, member, http.MethodPut, size, `{"size":"L"}`)
	do(http.StatusConflict, nil, owner, http.MethodPut, size, `{"size":"L"}`)
	// Rejecting it does not open a raise before accepting it again.
	do(http.StatusOK, nil, owner, http.MethodPatch, "/v1/rooms/"+room.ID+"/contributions/"+c.ID, `{"status":"rejected"}`)
	do(http.StatusConflict, nil, owner, http.MethodPut, size, `{"size":"L"}`)
	var gotSize string
	var points int64
	if err := pool.QueryRow(ctx, `SELECT size, points FROM room_contributions WHERE id = $1`, c.ID).Scan(&gotSize, &points); err != nil ||
		gotSize != "M" || points != 400 {
		t.Fatalf("stored claim = %s at %d (%v), want M at 400", gotSize, points, err)
	}
}
