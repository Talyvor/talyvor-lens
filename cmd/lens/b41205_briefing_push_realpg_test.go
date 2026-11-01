package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/internal/webpush"
)

// B41.205 — with the push hour unset no briefing is pushed; with it set and one subscription, the minute ticks that
// morning push exactly one, and it decrypts to the briefing's Line.
func TestBriefingPush_OneAMorningReadingTheLine(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-briefing-push"

	var mu sync.Mutex
	var pushes [][]byte
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		pushes = append(pushes, body)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(service.Close)
	vapidKey, _, err := webpush.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	sender, err := webpush.NewSender(vapidKey, "mailto:ops@example.com")
	if err != nil {
		t.Fatal(err)
	}
	sender.AllowEndpoint = func(u *url.URL) bool { return strings.HasPrefix(u.String(), service.URL) }
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: "app.talyvor.com", Origins: []string{"https://app.talyvor.com"}}, sender)

	// The owner's phone subscribes, through the route Home uses.
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	device, _ := ecdh.P256().GenerateKey(rand.Reader)
	secret := make([]byte, 16)
	_, _ = rand.Read(secret)
	b64 := base64.RawURLEncoding.EncodeToString
	sub := `{"endpoint":"` + service.URL + `/phone","keys":{"p256dh":"` + b64(device.PublicKey().Bytes()) + `","auth":"` + b64(secret) + `"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+ws+"/agents/push/subscriptions", strings.NewReader(sub))
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("subscribe = %d %s", w.Code, w.Body.String())
	}

	tick := func(at time.Time, hour int) int {
		t.Helper()
		n, err := store.RunBriefingPushes(ctx, at, hour)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	morning := time.Date(2026, 11, 2, 7, 0, 0, 0, time.UTC)

	// Unset (the default), nothing goes out, all morning.
	for _, at := range []time.Time{morning, morning.Add(time.Minute), morning.Add(3 * time.Hour)} {
		if n := tick(at, -1); n != 0 {
			t.Fatalf("hour unset: %d pushed at %s", n, at)
		}
	}
	// Set to 07: nothing before it, one at it, none on the ticks after.
	if n := tick(morning.Add(-time.Minute), 7); n != 0 {
		t.Fatalf("06:59: %d pushed before the hour", n)
	}
	if n := tick(morning, 7); n != 1 {
		t.Fatalf("07:00: %d pushed, want 1", n)
	}
	if n := tick(morning.Add(time.Minute), 7); n != 0 {
		t.Fatalf("07:01: %d pushed, want 0 (once a day)", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(pushes) != 1 {
		t.Fatalf("the push service got %d pushes, want 1", len(pushes))
	}
	plain, err := webpush.Decrypt(pushes[0], device, secret)
	if err != nil {
		t.Fatalf("the push does not decrypt: %v", err)
	}
	var got struct{ Type, Line string }
	if err := json.Unmarshal(plain, &got); err != nil {
		t.Fatal(err)
	}
	want, err := store.DailyBriefing(ctx, ws, morning)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "daily_briefing" || got.Line == "" || got.Line != want.Line {
		t.Fatalf("push = %s, want type daily_briefing and line %q", plain, want.Line)
	}
}
