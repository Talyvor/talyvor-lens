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

// B52.45 — before B joins, the BFF reads what A's invite is to: A's workspace and its name, as often as it asks, with
// nobody added and nothing used; a revoked invite and one that never existed are 404, as accept answers them.
func TestB5245_InvitePreviewNamesTheWorkspaceAndUsesNothing(t *testing.T) {
	mgr, pool := provisionManager(t)
	ctx := context.Background()
	keys := newKeyFixture(t)
	store := members.New(pool)
	const idA, idB = "b5245|alice", "b5245|bob"
	wsA := deriveWorkspaceID(idA)
	if _, err := pool.Exec(ctx, `DELETE FROM workspace_members WHERE workspace_id = $1`, wsA); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountProvisionRoute(r, provisionSecret, mgr, nil, keys.mint, store.MemberID)
	mountMemberProvisionRoutes(r, provisionSecret, store)
	r.Group(func(authed chi.Router) {
		authed.Use(auth.AuthMiddleware(auth.New(pool), keys.mgr))
		authed.Use(workspaceIsolationMiddleware)
		mountWorkspaceMemberRoutes(authed, store)
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
	w, owner := doProvision(t, r, provisionSecret, `{"identity":"`+idA+`"}`)
	if w.Code != http.StatusOK || owner.Token == "" {
		t.Fatalf("provision A = %d %s", w.Code, w.Body.String())
	}
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET name = 'Alice Ltd' WHERE id = $1`, wsA); err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	w = call(http.MethodPost, "/v1/workspaces/"+wsA+"/member-invites", owner.Token, `{"max_uses":1,"expires_at":"`+exp+`"}`)
	var inv members.Invite
	if err := json.Unmarshal(w.Body.Bytes(), &inv); w.Code != http.StatusCreated || err != nil || inv.Token == "" {
		t.Fatalf("A makes an invite = %d %s", w.Code, w.Body.String())
	}
	state := func() (uses, memberRows int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT uses FROM workspace_member_invites WHERE id = $1`, inv.ID).Scan(&uses); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_members WHERE workspace_id = $1`, wsA).Scan(&memberRows); err != nil {
			t.Fatal(err)
		}
		return uses, memberRows
	}

	preview := `{"identity":"` + idB + `","token":"` + inv.Token + `"}`
	for i := 1; i <= 2; i++ {
		w := call(http.MethodPost, "/v1/provision/invites/preview", "", preview)
		var got map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &got); w.Code != http.StatusOK || err != nil ||
			len(got) != 2 || got["workspace_id"] != wsA || got["name"] != "Alice Ltd" {
			t.Fatalf("B previews the invite (%d) = %d %s, want 200 {workspace_id: %s, name: Alice Ltd}", i, w.Code, w.Body.String(), wsA)
		}
		if uses, rows := state(); uses != 0 || rows != 0 {
			t.Fatalf("after preview %d: invite uses %d and %d member rows, want 0 and 0", i, uses, rows)
		}
	}
	if w := call(http.MethodPost, "/v1/provision/invites/preview", "", `{"identity":"`+idB+`","token":"wsinvtok_nothing"}`); w.Code != http.StatusNotFound {
		t.Fatalf("B previews an invite that never existed = %d %s, want 404", w.Code, w.Body.String())
	}
	if w := call(http.MethodPost, "/v1/provision/invites/preview", "", `{"token":"`+inv.Token+`"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a preview naming no identity = %d %s, want 400", w.Code, w.Body.String())
	}
	if w := call(http.MethodDelete, "/v1/workspaces/"+wsA+"/member-invites/"+inv.ID, owner.Token, ""); w.Code != http.StatusOK {
		t.Fatalf("A revokes the invite = %d %s", w.Code, w.Body.String())
	}
	if w := call(http.MethodPost, "/v1/provision/invites/preview", "", preview); w.Code != http.StatusNotFound {
		t.Fatalf("B previews the revoked invite = %d %s, want 404", w.Code, w.Body.String())
	}
	if w := call(http.MethodPost, "/v1/provision/invites/accept", "", preview); w.Code != http.StatusNotFound {
		t.Fatalf("B accepts the revoked invite = %d %s, want 404, as the preview answered", w.Code, w.Body.String())
	}
}
