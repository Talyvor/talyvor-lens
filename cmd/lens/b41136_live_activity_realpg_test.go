package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/passkey"
)

// B41.136 — with a fake APNs: an iPhone gives its ActivityKit push-to-start token; holding a payment sends that token
// one Live Activity start, with expo-widgets' attributes and the app's props; the phone gives the activity's own push
// token; approving the payment sends that token one end, showing approved.
func TestB41136_AHeldPaymentStartsItsLiveActivityAndApprovingItEndsIt(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const rpID, origin = "app.talyvor.com", "https://app.talyvor.com"
	const startToken = "5a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f6071"
	const activityToken = "ac1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9"

	type push struct {
		header http.Header
		path   string
		aps    map[string]any
	}
	var mu sync.Mutex
	var got []push
	apnsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			APS map[string]any `json:"aps"`
		}
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		got = append(got, push{r.Header.Clone(), r.URL.Path, body.APS})
		mu.Unlock()
	}))
	t.Cleanup(apnsServer.Close)
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p8, _ := x509.MarshalPKCS8PrivateKey(ecKey)
	apns, err := nativepush.NewAPNs(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p8}), "KEY123", "TEAM456", "com.talyvor.app", apnsServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	wait := func(n int) []push {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			mu.Lock()
			k := len(got)
			mu.Unlock()
			if k >= n {
				time.Sleep(200 * time.Millisecond) // a push that should not come has had its chance
				mu.Lock()
				defer mu.Unlock()
				if len(got) != n {
					t.Fatalf("APNs got %d pushes, want %d", len(got), n)
				}
				return append([]push(nil), got...)
			}
		}
		t.Fatalf("fewer than %d APNs pushes arrived", n)
		return nil
	}

	b := b501Payer(t, pool, "ws-b41136-live")
	ws := b.acct.WorkspaceID
	b.store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	if _, err := pool.Exec(ctx, `DELETE FROM workspace_device_keys WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	sign := b5018Passkey(t, b.store, ws, rpID, origin)
	b.store.SetNativePush(nativepush.Senders{APNs: apns})
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, person, platform, public_key, attestation)
		VALUES ('dev_b41136_iphone', $1, 'jwt:user:ada', 'ios', '\x00', '\x00')`, ws); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountDeviceKeyRoutes(r, b.store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	call := func(path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	if code, body := call("/v1/money/devices/dev_b41136_iphone/live-activity-token", `{"push_to_start_token":"`+startToken+`"}`); code != http.StatusOK {
		t.Fatalf("push-to-start token = %d %s", code, body)
	}

	// Held: one start to the push-to-start token.
	held, _, err := b.store.PreparePaymentRequest(ctx, ws, "ada", "b41136-1", economy.PaymentRequestIn{Client: "chat", AgentID: b.acct.AgentID,
		Items: []economy.PaymentRequestItemIn{{PayeeID: b.payee.ID, Currency: "GBP", AmountMinor: 34000, Reference: "Invoice 7"}}})
	if err != nil || held.Status != "pending_approval" {
		t.Fatalf("prepare = %v %v", held.Status, err)
	}
	var agent string
	if err := pool.QueryRow(ctx, `SELECT name FROM agent_accounts WHERE id = $1`, b.acct.AgentID).Scan(&agent); err != nil {
		t.Fatal(err)
	}
	props := func(p push) map[string]any {
		t.Helper()
		cs, _ := p.aps["content-state"].(map[string]any)
		if cs["name"] != "TalyvorApproval" {
			t.Errorf("content-state = %v, want the TalyvorApproval layout", cs)
		}
		var out map[string]any
		if s, _ := cs["props"].(string); json.Unmarshal([]byte(s), &out) != nil {
			t.Fatalf("content-state props = %v, want the layout's props as JSON", cs["props"])
		}
		return out
	}
	start := wait(1)[0]
	if start.path != "/3/device/"+startToken || start.header.Get("apns-push-type") != "liveactivity" ||
		start.header.Get("apns-topic") != "com.talyvor.app.push-type.liveactivity" || start.aps["event"] != "start" ||
		start.aps["attributes-type"] != "LiveActivityAttributes" {
		t.Errorf("the start = %s %v %v", start.path, start.header, start.aps)
	}
	if attrs, _ := start.aps["attributes"].(map[string]any); attrs["url"] != "talyvor://approve/"+held.ID {
		t.Errorf("the start's attributes = %v, want the request's approve link", start.aps["attributes"])
	}
	if alert, _ := start.aps["alert"].(map[string]any); alert["title"] == "" || alert["body"] != "Ada Lovelace · £340.00" {
		t.Errorf("the start's alert = %v", start.aps["alert"])
	}
	p := props(start)
	if p["state"] != "waiting" || p["title"] != "Ada Lovelace" || p["amount"] != "£340.00" || p["detail"] != agent+" asks" ||
		p["hint"] != "Tap to approve with Face ID" {
		t.Errorf("the start's props = %v", p)
	}
	if pal, _ := p["palette"].(map[string]any); pal["light"] == nil || pal["dark"] == nil {
		t.Errorf("the start's props carry no palette: %v", p)
	}

	// The activity's own push token, then the approval: one end to it, showing approved.
	if code, body := call("/v1/money/devices/dev_b41136_iphone/live-activities", `{"key":"pr:`+held.ID+`","push_token":"`+activityToken+`"}`); code != http.StatusOK {
		t.Fatalf("activity token = %d %s", code, body)
	}
	statement, challenge, _, err := b.store.PrepareRequestApproval(ctx, ws, held.ID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.ApprovePaymentRequest(ctx, ws, held.ID, "ada", statement, sign(challenge)); err != nil {
		t.Fatal(err)
	}
	end := wait(2)[1]
	if end.path != "/3/device/"+activityToken || end.header.Get("apns-push-type") != "liveactivity" || end.aps["event"] != "end" {
		t.Errorf("the end = %s %v %v", end.path, end.header, end.aps)
	}
	if p := props(end); p["state"] != "approved" || p["title"] != "Ada Lovelace" || p["amount"] != "£340.00" {
		t.Errorf("the end's props = %v", p)
	}
	// Ended once: the same token registered again is sent nothing more.
	if code, body := call("/v1/money/devices/dev_b41136_iphone/live-activities", `{"key":"pr:`+held.ID+`","push_token":"`+activityToken+`"}`); code != http.StatusOK {
		t.Fatalf("activity token again = %d %s", code, body)
	}
	wait(2)
}
