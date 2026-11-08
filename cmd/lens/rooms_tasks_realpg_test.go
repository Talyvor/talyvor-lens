package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/rooms"
)

// B41.152 (B41.52's DONE line, from Lens alone) — a member claims a task and others see the claim, and nothing in the
// task model can assign a task to a named person or set a due date.
func TestRooms_TasksBoardMemberClaimsAndOthersSeeItAndNothingAssignsOrSetsADeadline(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const owner, ann, bob = "ws-b41152-owner", "ws-b41152-ann", "ws-b41152-bob"
	for _, ws := range []string{owner, ann, bob} {
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
	for _, ws := range []string{ann, bob} {
		do(http.StatusCreated, nil, ws, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":1}`)
	}
	do(http.StatusOK, nil, owner, http.MethodPut, "/v1/rooms/"+room.ID+"/rate-card",
		`{"pool_points":10000,"sizes":[{"size":"S","hours":2,"points_per_hour":50},{"size":"M","hours":8,"points_per_hour":50}]}`)
	tasks := "/v1/rooms/" + room.ID + "/tasks"

	// The owner posts a task at a size on the rate card, and it carries that size's points. A member does not post one,
	// a size off the card is refused, and so is any attempt to name who does it or by when.
	post := `{"title":"Write the onboarding copy","description":"Three screens.","size":"M"}`
	do(http.StatusForbidden, nil, ann, http.MethodPost, tasks, post)
	do(http.StatusBadRequest, nil, owner, http.MethodPost, tasks, strings.Replace(post, `"M"`, `"XL"`, 1))
	do(http.StatusBadRequest, nil, owner, http.MethodPost, tasks, strings.Replace(post, `}`, `,"assignee_workspace_id":"`+ann+`"}`, 1))
	do(http.StatusBadRequest, nil, owner, http.MethodPost, tasks, strings.Replace(post, `}`, `,"due_at":"2026-12-01T00:00:00Z"}`, 1))
	var task rooms.Task
	do(http.StatusCreated, &task, owner, http.MethodPost, tasks, post)
	if task.Size != "M" || task.Points != 400 || task.ClaimantWorkspaceID != "" {
		t.Fatalf("posted = %+v, want open at M's 400 points", task)
	}

	// Ann claims it for herself; Bob sees her claim and cannot take it from her, nor let it go for her.
	claim := tasks + "/" + task.ID + "/claim"
	do(http.StatusOK, &task, ann, http.MethodPost, claim, "")
	var board []rooms.Task
	do(http.StatusOK, &board, bob, http.MethodGet, tasks, "")
	if len(board) != 1 || board[0].ClaimantWorkspaceID != ann || board[0].ClaimantWorkspaceName != ann || board[0].ClaimedAt == nil {
		t.Fatalf("Bob reads %+v, want the task claimed by %s", board, ann)
	}
	do(http.StatusConflict, nil, bob, http.MethodPost, claim, "")
	do(http.StatusForbidden, nil, bob, http.MethodDelete, claim, "")

	// Ann lets it go with nothing lost, and then Bob may claim it.
	var released rooms.Task
	do(http.StatusOK, &released, ann, http.MethodDelete, claim, "")
	if released.ClaimantWorkspaceID != "" || released.ClaimedAt != nil || released.Points != 400 {
		t.Fatalf("released = %+v, want open again at 400 points", released)
	}
	do(http.StatusOK, &task, bob, http.MethodPost, claim, "")

	// Bob leaves the room: his claim lapses with nothing lost, the board shows the task open, and Ann claims it again.
	do(http.StatusOK, nil, bob, http.MethodPatch, "/v1/rooms/"+room.ID+"/members/"+bob, `{"remove":true}`)
	board = nil
	do(http.StatusOK, &board, ann, http.MethodGet, tasks, "")
	if len(board) != 1 || board[0].ClaimantWorkspaceID != "" || board[0].ClaimedAt != nil {
		t.Fatalf("after Bob left Ann reads %+v, want the task open", board)
	}
	do(http.StatusOK, nil, ann, http.MethodPost, claim, "")
	var claimant string
	if err := pool.QueryRow(ctx, `SELECT claimant_workspace_id FROM room_tasks WHERE id = $1`, task.ID).Scan(&claimant); err != nil || claimant != ann {
		t.Fatalf("stored claimant = %q (%v), want %s", claimant, err, ann)
	}

	// The stored model has no column that could assign a task or give it a deadline or a penalty.
	rows, err := pool.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name = 'room_tasks'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n, forbidden := 0, regexp.MustCompile(`assign|due|deadline|penalt|expire`)
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatal(err)
		}
		if n++; forbidden.MatchString(col) {
			t.Fatalf("room_tasks has column %q: a task is never assigned and has no deadline or penalty", col)
		}
	}
	if n == 0 {
		t.Fatal("room_tasks has no columns: the table was not read")
	}
}
