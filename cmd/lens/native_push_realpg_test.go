package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
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
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B40.15 — with a fake APNs and a fake FCM: an iPhone and an Android phone give their device keys push tokens; a held
// payment is pushed once to each, naming the payee and the amount; once the iPhone's key is revoked it gets none; and
// a request prepared for approval is pushed with the payee and the total, and never the payee's account number.
func TestNativePush_ApprovalsReachEachPhoneUntilRevoked(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b4015-native-push"
	const iosToken = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	const fcmToken = "fcm-token:APA91bH_test-registration.token"

	type push struct {
		header http.Header
		path   string
		body   map[string]any
	}
	var mu sync.Mutex
	var apnsGot, fcmGot []push
	record := func(into *[]push, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		*into = append(*into, push{r.Header.Clone(), r.URL.Path, body})
		mu.Unlock()
	}
	apnsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { record(&apnsGot, r) }))
	t.Cleanup(apnsServer.Close)
	fcmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || r.Form.Get("assertion") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"ya29.test","expires_in":3600}`))
			return
		}
		record(&fcmGot, r)
		_, _ = w.Write([]byte(`{"name":"projects/talyvor-test/messages/1"}`))
	}))
	t.Cleanup(fcmServer.Close)

	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p8, _ := x509.MarshalPKCS8PrivateKey(ecKey)
	apns, err := nativepush.NewAPNs(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p8}), "KEY123", "TEAM456", "com.talyvor.app", apnsServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(rsaKey)
	creds, _ := json.Marshal(map[string]string{"project_id": "talyvor-test", "client_email": "push@talyvor-test.iam.gserviceaccount.com",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": fcmServer.URL + "/token"})
	fcm, err := nativepush.NewFCM(creds)
	if err != nil {
		t.Fatal(err)
	}
	fcm.Host = fcmServer.URL

	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetPayeeScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	store.SetNativePush(nativepush.Senders{APNs: apns, FCM: fcm})
	r := chi.NewRouter()
	mountDeviceKeyRoutes(r, store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	call := func(path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	// Two phones' keys, as B40.4 keeps them once their attestations check out.
	for _, d := range [][2]string{{"dev_b4015_iphone", "ios"}, {"dev_b4015_pixel", "android"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, person, platform, public_key, attestation)
			VALUES ($1, $2, 'jwt:user:ada', $3, '\x00', '\x00')`, d[0], ws, d[1]); err != nil {
			t.Fatal(err)
		}
	}
	if code, body := call("/v1/money/devices/dev_b4015_iphone/push-token", `{"push_token":"not hex"}`); code != http.StatusBadRequest {
		t.Fatalf("a bad APNs token = %d %s, want 400", code, body)
	}
	for id, token := range map[string]string{"dev_b4015_iphone": iosToken, "dev_b4015_pixel": fcmToken} {
		if code, body := call("/v1/money/devices/"+id+"/push-token", `{"push_token":"`+token+`"}`); code != http.StatusOK {
			t.Fatalf("push token for %s = %d %s", id, code, body)
		}
	}

	wait := func(n, m int) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			mu.Lock()
			a, f := len(apnsGot), len(fcmGot)
			mu.Unlock()
			if a >= n && f >= m {
				time.Sleep(200 * time.Millisecond) // a push that should not come has had its chance
				mu.Lock()
				a, f = len(apnsGot), len(fcmGot)
				mu.Unlock()
				if a != n || f != m {
					t.Fatalf("APNs got %d and FCM %d pushes, want %d and %d", a, f, n, m)
				}
				return
			}
		}
		t.Fatalf("fewer than %d APNs and %d FCM pushes arrived", n, m)
	}

	// A held payment: one push to each phone, with the payee and the amount.
	agent, err := store.CreateAgent(ctx, ws, "researcher", "user-ada")
	if err != nil {
		t.Fatal(err)
	}
	payee, err := store.CreateAgent(ctx, ws, "vendor", "user-ada")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, agent.ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	held, err := store.RequestPaymentApproval(ctx, ws, agent.ID, payee.ID, 2_000_000, "invoice 7", "October hosting")
	if err != nil {
		t.Fatal(err)
	}
	wait(1, 1)
	a := apnsGot[0]
	alert, _ := a.body["aps"].(map[string]any)["alert"].(map[string]any)
	if a.path != "/3/device/"+iosToken || a.header.Get("apns-topic") != "com.talyvor.app" || a.header.Get("apns-push-type") != "alert" ||
		!strings.HasPrefix(a.header.Get("Authorization"), "bearer ey") || alert["body"] != "researcher wants to pay vendor 2 LXC" ||
		a.body["approval_id"] != held.ID {
		t.Errorf("the APNs push = %s %v %v", a.path, a.header, a.body)
	}
	f := fcmGot[0]
	msg, _ := f.body["message"].(map[string]any)
	note, _ := msg["notification"].(map[string]any)
	data, _ := msg["data"].(map[string]any)
	if f.path != "/v1/projects/talyvor-test/messages:send" || f.header.Get("Authorization") != "Bearer ya29.test" || msg["token"] != fcmToken ||
		note["body"] != "researcher wants to pay vendor 2 LXC" || data["approval_id"] != held.ID {
		t.Errorf("the FCM push = %s %v %v", f.path, f.header, f.body)
	}

	// The iPhone's key is revoked: the next held payment reaches only the Android phone.
	if code, body := call("/v1/money/devices/dev_b4015_iphone/revoke", ""); code != http.StatusOK {
		t.Fatalf("revoke = %d %s", code, body)
	}
	if _, err := store.RequestPaymentApproval(ctx, ws, agent.ID, payee.ID, 3_000_000, "invoice 8", "November hosting"); err != nil {
		t.Fatal(err)
	}
	wait(1, 2)

	// A request prepared for approval: the payee and the total, never the account number.
	if _, err := pool.Exec(ctx, `INSERT INTO money_payees (id, workspace_id, name, country, sort_code, account_number, check_result, checked_by)
		VALUES ('payee_b4015', $1, 'Carol Consulting', 'GB', '20-00-00', '55779911', 'exact_match', 'test')`, ws); err != nil {
		t.Fatal(err)
	}
	prepared, _, err := store.PreparePaymentRequest(ctx, ws, "ada", "b4015-1", economy.PaymentRequestIn{Client: "chat",
		Items: []economy.PaymentRequestItemIn{{PayeeID: "payee_b4015", Currency: "GBP", AmountMinor: 5000, Reference: "INV-9"}}})
	if err != nil || prepared.Status != "pending_approval" {
		t.Fatalf("prepare = %v %v", prepared.Status, err)
	}
	wait(1, 3)
	msg, _ = fcmGot[2].body["message"].(map[string]any)
	note, _ = msg["notification"].(map[string]any)
	data, _ = msg["data"].(map[string]any)
	if note["body"] != "Pay Carol Consulting · GBP 50.00" || data["url"] != "talyvor://approve/"+prepared.ID {
		t.Errorf("the prepared request's push = %v", msg)
	}
	if raw, _ := json.Marshal(fcmGot); strings.Contains(string(raw), "55779911") {
		t.Errorf("a push carried the payee's account number: %s", raw)
	}
}
