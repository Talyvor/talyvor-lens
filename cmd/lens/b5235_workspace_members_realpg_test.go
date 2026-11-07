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
	"github.com/talyvor/lens/internal/members"
)

// B52.35 — owner A invites B by link; B accepts once, lists A's workspace beside its own, and gets a token that reads A
// and writes nothing; B gets nothing for C, whose member B is not, nor for A once A removes B, whose token then ends.
func TestB5235_JoinByInviteListAndTokenForAWorkspace(t *testing.T) {
	mgr, pool := provisionManager(t)
	ctx := context.Background()
	keys := newKeyFixture(t)
	store := members.New(pool)
	keys.mgr.WithMemberCheck(store.LiveMember)
	const idA, idB, idC = "b5235|alice", "b5235|bob", "b5235|carol"
	wsA, wsB, wsC := deriveWorkspaceID(idA), deriveWorkspaceID(idB), deriveWorkspaceID(idC)
	for _, ws := range []string{wsA, wsB, wsC} {
		if _, err := pool.Exec(ctx, `DELETE FROM workspace_members WHERE workspace_id = $1`, ws); err != nil {
			t.Fatal(err)
		}
	}

	r := chi.NewRouter()
	mountProvisionRoute(r, provisionSecret, mgr, nil, keys.mint, store.MemberID)
	mountMemberProvisionRoutes(r, provisionSecret, store)
	r.Group(func(authed chi.Router) {
		authed.Use(auth.AuthMiddleware(auth.New(pool), keys.mgr))
		authed.Use(workspaceIsolationMiddleware)
		mountWorkspaceMemberRoutes(authed, store)
		// Any read and any write of the workspace, with no gate of their own.
		probe := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
		authed.Get("/v1/workspaces/{wsID}/probe", probe)
		authed.Post("/v1/workspaces/{wsID}/probe", probe)
	})
	call := func(method, path, token, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token == "" {
			req.Header.Set(provisionSecretHeader, provisionSecret)
		} else {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	provision := func(identity, ws string) (*httptest.ResponseRecorder, provisionResponseBody) {
		t.Helper()
		return doProvision(t, r, provisionSecret, `{"identity":"`+identity+`","workspace_id":"`+ws+`"}`)
	}
	memberRows := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_members WHERE workspace_id = $1 AND identity = $2
			AND removed_at IS NULL`, wsA, idB).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	var ownerA string
	for _, id := range []string{idA, idB, idC} {
		w, out := provision(id, "")
		if w.Code != http.StatusOK || out.Token == "" {
			t.Fatalf("provision %s = %d %s", id, w.Code, w.Body.String())
		}
		if id == idA {
			ownerA = out.Token
		}
	}

	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	w := call(http.MethodPost, "/v1/workspaces/"+wsA+"/member-invites", ownerA, `{"max_uses":1,"expires_at":"`+exp+`"}`)
	var inv members.Invite
	if err := json.Unmarshal(w.Body.Bytes(), &inv); w.Code != http.StatusCreated || err != nil || inv.Token == "" {
		t.Fatalf("A makes an invite = %d %s", w.Code, w.Body.String())
	}
	accept := `{"identity":"` + idB + `","token":"` + inv.Token + `"}`
	if w := call(http.MethodPost, "/v1/provision/invites/accept", "", accept); w.Code != http.StatusOK || memberRows() != 1 {
		t.Fatalf("B accepts = %d %s with %d member rows, want 200 and one", w.Code, w.Body.String(), memberRows())
	}
	if w := call(http.MethodPost, "/v1/provision/invites/accept", "", `{"identity":"`+idC+`","token":"`+inv.Token+`"}`); w.Code != http.StatusNotFound {
		t.Fatalf("C accepts the used-up single-use invite = %d %s, want 404", w.Code, w.Body.String())
	}
	if w := call(http.MethodPost, "/v1/provision/invites/accept", "", accept); w.Code != http.StatusNotFound || memberRows() != 1 {
		t.Fatalf("B accepts again = %d %s with %d member rows, want 404 and still one", w.Code, w.Body.String(), memberRows())
	}

	w = call(http.MethodPost, "/v1/provision/workspaces", "", `{"identity":"`+idB+`"}`)
	var list struct{ Workspaces []members.Workspace }
	if err := json.Unmarshal(w.Body.Bytes(), &list); w.Code != http.StatusOK || err != nil || len(list.Workspaces) != 2 ||
		list.Workspaces[0] != (members.Workspace{ID: wsB, Name: wsB, Role: "owner"}) ||
		list.Workspaces[1] != (members.Workspace{ID: wsA, Name: wsA, Role: "member"}) {
		t.Fatalf("B's workspaces = %d %s, want B's own then A's", w.Code, w.Body.String())
	}

	w, out := provision(idB, wsA)
	if w.Code != http.StatusOK || out.WorkspaceID != wsA || out.Token == "" {
		t.Fatalf("provision B for A = %d %s, want a token for A", w.Code, w.Body.String())
	}
	memberToken := out.Token
	if w := call(http.MethodGet, "/v1/workspaces/"+wsA+"/probe", memberToken, ""); w.Code != http.StatusOK {
		t.Fatalf("B's token reads A = %d %s, want 200", w.Code, w.Body.String())
	}
	if w := call(http.MethodPost, "/v1/workspaces/"+wsA+"/probe", memberToken, ""); w.Code != http.StatusForbidden {
		t.Fatalf("B's token writes A = %d %s, want 403: a member only reads", w.Code, w.Body.String())
	}
	if w := call(http.MethodGet, "/v1/workspaces/"+wsA+"/members", out.Token, ""); w.Code != http.StatusForbidden {
		t.Fatalf("B's member token lists A's members = %d %s, want 403: a member only reads", w.Code, w.Body.String())
	}
	if w, out := provision(idB, wsC); w.Code != http.StatusForbidden || out.Token != "" {
		t.Fatalf("provision B for C = %d %s, want 403 and no token", w.Code, w.Body.String())
	}

	w = call(http.MethodGet, "/v1/workspaces/"+wsA+"/members", ownerA, "")
	var ms struct{ Members []members.Member }
	if err := json.Unmarshal(w.Body.Bytes(), &ms); w.Code != http.StatusOK || err != nil || len(ms.Members) != 1 || ms.Members[0].Identity != idB {
		t.Fatalf("A's members = %d %s, want B", w.Code, w.Body.String())
	}
	if w := call(http.MethodDelete, "/v1/workspaces/"+wsA+"/members/"+ms.Members[0].ID, ownerA, ""); w.Code != http.StatusNoContent || memberRows() != 0 {
		t.Fatalf("A removes B = %d %s", w.Code, w.Body.String())
	}
	if w, out := provision(idB, wsA); w.Code != http.StatusForbidden || out.Token != "" {
		t.Fatalf("provision B for A after removal = %d %s, want 403 and no token", w.Code, w.Body.String())
	}
	if w := call(http.MethodGet, "/v1/workspaces/"+wsA+"/probe", memberToken, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("B's token reads A after removal = %d %s, want 401: removal ends it at once", w.Code, w.Body.String())
	}

	// A link made before the removal does not bring B back.
	before := call(http.MethodPost, "/v1/workspaces/"+wsA+"/member-invites", ownerA, `{"max_uses":5,"expires_at":"`+exp+`"}`)
	var old members.Invite
	if err := json.Unmarshal(before.Body.Bytes(), &old); err != nil || old.Token == "" {
		t.Fatalf("A makes a second invite = %d %s", before.Code, before.Body.String())
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_member_invites SET created_at = now() - interval '1 minute' WHERE id = $1`, old.ID); err != nil {
		t.Fatal(err)
	}
	if w := call(http.MethodPost, "/v1/provision/invites/accept", "", `{"identity":"`+idB+`","token":"`+old.Token+`"}`); w.Code != http.StatusNotFound || memberRows() != 0 {
		t.Fatalf("B rejoins on a link older than the removal = %d %s, want 404 and no member", w.Code, w.Body.String())
	}
}
