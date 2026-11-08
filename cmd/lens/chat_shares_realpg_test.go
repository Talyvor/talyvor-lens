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
)

// B28.452 — a chat shared from Chat is read signed out by its link until the link is turned off, and then the copy is
// gone and the link answers 404. Another workspace can neither list nor turn it off.
func TestChatShares_ReadSignedOutUntilTurnedOff(t *testing.T) {
	pool := agentRoutesDB(t)
	r := chi.NewRouter()
	r.Group(func(authed chi.Router) {
		authed.Use(workspaceIsolationMiddleware)
		mountChatShareRoutes(authed, pool)
	})
	r.Get("/v1/public/chat-shares/{token}", publicChatShareHandler(pool))

	memberA := &auth.AuthContext{WorkspaceID: "ws-a", AuthMethod: auth.MethodSessionKey, UserID: "u-a", Scopes: []string{auth.ScopeProxy}}
	memberB := &auth.AuthContext{WorkspaceID: "ws-b", AuthMethod: auth.MethodSessionKey, UserID: "u-b", Scopes: []string{auth.ScopeProxy}}
	call := func(who *auth.AuthContext, method, path, body string) (int, []byte) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if who != nil {
			req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.Bytes()
	}

	code, raw := call(memberA, http.MethodPost, "/v1/workspaces/ws-a/chat-shares",
		`{"conversation_id":"c-1","title":"Trip plan","messages":[{"role":"user","content":"Where to?"},{"role":"assistant","content":"Lisbon."}]}`)
	if code != http.StatusCreated {
		t.Fatalf("share = %d %s", code, raw)
	}
	var made struct {
		ID             string `json:"id"`
		Token          string `json:"token"`
		ConversationID string `json:"conversation_id"`
		Title          string `json:"title"`
		CreatedAt      string `json:"created_at"`
	}
	if err := json.Unmarshal(raw, &made); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{32}$`).MatchString(made.Token) || made.ID == "" || made.ConversationID != "c-1" || made.Title != "Trip plan" || made.CreatedAt == "" {
		t.Fatalf("share answered %s", raw)
	}
	if code, _ := call(memberA, http.MethodPost, "/v1/workspaces/ws-a/chat-shares", `{"conversation_id":"c-1","title":"x","messages":[]}`); code != http.StatusBadRequest {
		t.Fatalf("an empty chat was shared: %d", code)
	}

	if code, raw := call(memberA, http.MethodGet, "/v1/workspaces/ws-a/chat-shares", ""); code != http.StatusOK || !strings.Contains(string(raw), made.Token) {
		t.Fatalf("ws-a's links = %d %s", code, raw)
	}
	if code, raw := call(memberB, http.MethodGet, "/v1/workspaces/ws-b/chat-shares", ""); code != http.StatusOK || strings.Contains(string(raw), made.Token) {
		t.Fatalf("ws-b's links = %d %s", code, raw)
	}

	code, raw = call(nil, http.MethodGet, "/v1/public/chat-shares/"+made.Token, "")
	if code != http.StatusOK {
		t.Fatalf("signed-out read = %d %s", code, raw)
	}
	var public map[string]json.RawMessage
	_ = json.Unmarshal(raw, &public)
	if len(public) != 3 || public["title"] == nil || public["created_at"] == nil || !strings.Contains(string(public["messages"]), "Lisbon.") {
		t.Fatalf("signed-out read answered %s, want only {title, messages, created_at}", raw)
	}

	if code, _ := call(memberB, http.MethodDelete, "/v1/workspaces/ws-b/chat-shares/"+made.ID, ""); code != http.StatusNotFound {
		t.Fatalf("ws-b turned off ws-a's link: %d", code)
	}
	if code, raw := call(memberA, http.MethodDelete, "/v1/workspaces/ws-a/chat-shares/"+made.ID, ""); code != http.StatusNoContent {
		t.Fatalf("turn off = %d %s", code, raw)
	}
	if code, _ := call(nil, http.MethodGet, "/v1/public/chat-shares/"+made.Token, ""); code != http.StatusNotFound {
		t.Fatalf("the turned-off link answered %d, not 404", code)
	}
	var left int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM chat_shares`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the copy was kept after the link was turned off: %d rows, %v", left, err)
	}
}
