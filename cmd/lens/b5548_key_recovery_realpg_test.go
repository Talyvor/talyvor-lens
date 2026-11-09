package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/tenant"
)

const (
	b5548StillHasAKey = "This workspace still has a key. Add the new one from Approvals and devices on a device that has a key."
	b5548SignInAgain  = "Sign in again, then try again."
)

var b5548CodeShape = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}( [0-9A-HJKMNP-TV-Z]{4}){4}$`)

// b5548Env is one router with the key, device and recovery routes on a migrated database, and the people who call it.
type b5548Env struct {
	t     *testing.T
	r     chi.Router
	pool  *pgxpool.Pool
	owner func(ws string) *auth.AuthContext
}

func b5548NewEnv(t *testing.T) (b5548Env, *economy.DualTokenStore) {
	t.Helper()
	pool := agentRoutesDB(t)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: "app.talyvor.com", Origins: []string{"https://app.talyvor.com"}}, nil)
	store.SetDeviceKeys(devicekey.Verifier{AllowTest: true})
	r := chi.NewRouter()
	r.Use(clientIPMiddleware())
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountDeviceKeyRoutes(r, store)
	mountKeyRecoveryRoutes(r, store)
	return b5548Env{t: t, r: r, pool: pool, owner: func(ws string) *auth.AuthContext {
		return &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	}}, store
}

// call sends body as actx from ip, signed in signedIn ago (a negative signedIn sends no X-Talyvor-Signed-In-At).
func (e b5548Env) call(actx *auth.AuthContext, ip string, signedIn time.Duration, method, path string, body any) (int, map[string]any, http.Header) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
	req = req.WithContext(auth.WithAuthContext(req.Context(), actx))
	req.Header.Set("X-Forwarded-For", ip)
	if signedIn >= 0 {
		req.Header.Set("X-Talyvor-Signed-In-At", time.Now().Add(-signedIn).UTC().Format(time.RFC3339))
	}
	rec := httptest.NewRecorder()
	e.r.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Header()
}

// passkeyRegistration makes a new passkey over a fresh registration challenge of ws, as POST …/agents/passkeys takes it.
func (e b5548Env) passkeyRegistration(ws string) map[string]any {
	e.t.Helper()
	const rpID, origin = "app.talyvor.com", "https://app.talyvor.com"
	b64 := base64.RawURLEncoding.EncodeToString
	_, c, _ := e.call(e.owner(ws), "198.51.100.1", time.Minute, http.MethodPost, "/v1/workspaces/"+ws+"/agents/passkeys/challenge", nil)
	pk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&pk.PublicKey)
	cred := make([]byte, 16)
	_, _ = rand.Read(cred)
	rpHash := sha256.Sum256([]byte(rpID))
	ad := binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x45), 0)
	ad = append(append(binary.BigEndian.AppendUint16(append(ad, make([]byte, 16)...), uint16(len(cred))), cred...), 0xa0)
	cdj := `{"type":"webauthn.create","challenge":"` + c["challenge"].(string) + `","origin":"` + origin + `"}`
	return map[string]any{"credential_id": b64(cred), "name": "This Mac", "public_key": b64(spki), "client_data_json": b64([]byte(cdj)),
		"authenticator_data": b64(ad)}
}

// deviceRegistration makes a new phone key over a fresh registration challenge of ws, as POST /v1/money/devices takes it.
func (e b5548Env) deviceRegistration(ws string) map[string]any {
	e.t.Helper()
	b64 := base64.RawURLEncoding.EncodeToString
	_, c, _ := e.call(e.owner(ws), "198.51.100.1", time.Minute, http.MethodPost, "/v1/money/devices/challenge", nil)
	dk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&dk.PublicKey)
	h := sha256.Sum256(devicekey.Message(c["challenge"].(string)))
	sig, _ := ecdsa.SignASN1(rand.Reader, dk, h[:])
	return map[string]any{"platform": "test", "name": "Ada's phone", "public_key": b64(spki), "challenge": c["challenge"], "signature": b64(sig),
		"attestation": ""}
}

// lockedOut gives ws its first passkey, returns the recovery code that answer carried, and revokes the passkey, as a
// lockdown does: the workspace has a key and none is live.
func (e b5548Env) lockedOut(ctx context.Context, store *economy.DualTokenStore, ws string) string {
	e.t.Helper()
	code, out, _ := e.call(e.owner(ws), "198.51.100.1", time.Minute, http.MethodPost, "/v1/workspaces/"+ws+"/agents/passkeys", e.passkeyRegistration(ws))
	if code != http.StatusCreated {
		e.t.Fatalf("first passkey = %d %v", code, out)
	}
	rc, _ := out["recovery_code"].(string)
	if !b5548CodeShape.MatchString(rc) {
		e.t.Fatalf("first passkey's answer recovery_code = %q, want 20 Crockford base32 characters in five groups of four", rc)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE workspace_passkeys SET revoked_at = now() WHERE workspace_id = $1`, ws); err != nil {
		e.t.Fatal(err)
	}
	return rc
}

func b5548LiveKeys(t *testing.T, store *economy.DualTokenStore, ws string) int {
	t.Helper()
	keys, err := store.ListKeys(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	return len(keys)
}

func b5548Audit(t *testing.T, e b5548Env, ws, kind string) []map[string]any {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT body FROM audit_chain WHERE workspace_id = $1 AND kind = $2 ORDER BY seq`, ws, kind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(b), &m)
		out = append(out, m)
	}
	return out
}

// B55.48 — a workspace whose only key was revoked gets a key back with its recovery code: the free first key's answer
// carries the code, which is kept only as a hash; the ordinary routes still let no key in; the right code adds a live
// key once and answers a new code; the spent code is then refused; and with a live key every recovery route is 409.
func TestB5548_TheRecoveryCodeAddsAKeyOnce(t *testing.T) {
	e, store := b5548NewEnv(t)
	ctx := context.Background()
	const ws = "ws-b5548-code"
	owner, ip := e.owner(ws), "203.0.113.10"
	base := "/v1/workspaces/" + ws + "/keys/recover"
	rc := e.lockedOut(ctx, store, ws)

	var stored string
	if err := e.pool.QueryRow(ctx, `SELECT hash FROM workspace_recovery_codes WHERE workspace_id = $1 AND used_at IS NULL`, ws).Scan(&stored); err != nil {
		t.Fatalf("the live recovery code's row: %v", err)
	}
	if strings.Contains(stored, strings.ReplaceAll(rc, " ", "")) || len(stored) != 64 {
		t.Errorf("stored recovery code = %q, want only a sha256", stored)
	}

	// The ordinary routes let nothing in without a live key's signature.
	if code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, "/v1/workspaces/"+ws+"/agents/passkeys", e.passkeyRegistration(ws)); code == http.StatusCreated {
		t.Fatalf("an unsigned passkey after every key was revoked = %d %v, want refused", code, out)
	}
	if n := b5548LiveKeys(t, store, ws); n != 0 {
		t.Fatalf("live keys before recovery = %d, want 0", n)
	}

	// A recovery waits too; the code ends it, so it never adds a key later.
	if code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, base+"/start", nil); code != http.StatusCreated {
		t.Fatalf("start a recovery = %d %v, want 201", code, out)
	}

	// The right code, typed in lower case and without spaces, adds the key at once.
	typed := strings.ToLower(strings.ReplaceAll(rc, " ", ""))
	code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, base, map[string]any{"code": typed, "passkey": e.passkeyRegistration(ws)})
	if code != http.StatusCreated {
		t.Fatalf("recover with the right code = %d %v, want 201", code, out)
	}
	next, _ := out["recovery_code"].(string)
	if !b5548CodeShape.MatchString(next) || next == rc {
		t.Errorf("recover answer's recovery_code = %q, want a new one", next)
	}
	if n := b5548LiveKeys(t, store, ws); n != 1 {
		t.Errorf("live keys after recovery = %d, want 1", n)
	}
	if a := b5548Audit(t, e, ws, "key_recovered"); len(a) != 1 || a[0]["way"] != "code" {
		t.Errorf("key_recovered audit entries = %v, want one with way code", a)
	}
	if code, out, _ := e.call(owner, ip, -1, http.MethodGet, base, nil); code != http.StatusOK || out["recovery"] != nil {
		t.Errorf("GET recovery after the code added a key = %d %v, want none waiting", code, out)
	}

	// With a live key, every recovery route answers 409 in words.
	for _, c := range []struct{ path, what string }{{base, "recover"}, {base + "/start", "start"}, {base + "/complete", "complete"}} {
		code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, c.path, map[string]any{"code": next, "passkey": e.passkeyRegistration(ws)})
		if code != http.StatusConflict || out["error"] != b5548StillHasAKey {
			t.Errorf("%s with a live key = %d %v, want 409 %q", c.what, code, out, b5548StillHasAKey)
		}
	}

	// Locked out again, the spent code is refused and the new one works.
	if _, err := e.pool.Exec(ctx, `UPDATE workspace_passkeys SET revoked_at = now() WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, base, map[string]any{"code": rc, "passkey": e.passkeyRegistration(ws)}); code != http.StatusForbidden {
		t.Errorf("recover with the spent code = %d %v, want 403", code, out)
	}
	if code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, base, map[string]any{"code": next, "device": e.deviceRegistration(ws)}); code != http.StatusCreated {
		t.Errorf("recover with the new code and a phone key = %d %v, want 201", code, out)
	}
	if n := b5548LiveKeys(t, store, ws); n != 1 {
		t.Errorf("live keys after the second recovery = %d, want 1", n)
	}
}

// B55.48 — a new recovery code is signed like any key change and voids the old one.
func TestB5548_ANewRecoveryCodeIsSignedAndVoidsTheOld(t *testing.T) {
	e, _ := b5548NewEnv(t)
	ctx := context.Background()
	const ws = "ws-b5548-new-code"
	owner, ip := e.owner(ws), "203.0.113.11"
	path := "/v1/workspaces/" + ws + "/keys/recovery-code"

	// A workspace's first key, a phone key: its answer carries the code too.
	code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, "/v1/money/devices", e.deviceRegistration(ws))
	if code != http.StatusCreated || !b5548CodeShape.MatchString(stringOf(out["recovery_code"])) {
		t.Fatalf("first phone key = %d %v, want 201 with a recovery_code", code, out)
	}
	old := out["recovery_code"].(string)
	deviceID := out["id"].(string)
	if code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, path, map[string]any{}); code != http.StatusForbidden {
		t.Errorf("a new recovery code unsigned = %d %v, want 403", code, out)
	}
	_, c, _ := e.call(owner, ip, time.Minute, http.MethodPost, path+"/challenge", nil)
	challenge, _ := c["challenge"].(string)
	if challenge == "" {
		t.Fatalf("recovery code challenge = %v", c)
	}
	dk := b5548SignWithNewDevice(t, e, ws, deviceID)
	code, out, _ = e.call(owner, ip, time.Minute, http.MethodPost, path, map[string]any{"assertion": dk(challenge)})
	fresh := stringOf(out["recovery_code"])
	if code != http.StatusCreated || !b5548CodeShape.MatchString(fresh) || fresh == old {
		t.Fatalf("a new recovery code signed = %d %v, want 201 with a new code", code, out)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE workspace_device_keys SET revoked_at = now() WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, "/v1/workspaces/"+ws+"/keys/recover",
		map[string]any{"code": old, "passkey": e.passkeyRegistration(ws)}); code != http.StatusForbidden {
		t.Errorf("recover with the voided code = %d %v, want 403", code, out)
	}
	if code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, "/v1/workspaces/"+ws+"/keys/recover",
		map[string]any{"code": fresh, "passkey": e.passkeyRegistration(ws)}); code != http.StatusCreated {
		t.Errorf("recover with the new code = %d %v, want 201", code, out)
	}
}

// b5548SignWithNewDevice swaps device deviceID's public key for one the test holds and returns its signer, so the test
// can sign as the phone it registered through the route.
func b5548SignWithNewDevice(t *testing.T, e b5548Env, ws, deviceID string) func(string) *passkey.Assertion {
	t.Helper()
	dk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&dk.PublicKey)
	if _, err := e.pool.Exec(context.Background(), `UPDATE workspace_device_keys SET public_key = $3 WHERE id = $1 AND workspace_id = $2`,
		deviceID, ws, spki); err != nil {
		t.Fatal(err)
	}
	return func(challenge string) *passkey.Assertion {
		h := sha256.Sum256(devicekey.Message(challenge))
		sig, _ := ecdsa.SignASN1(rand.Reader, dk, h[:])
		return &passkey.Assertion{DeviceID: deviceID, Challenge: challenge, Signature: base64.RawURLEncoding.EncodeToString(sig)}
	}
}

func stringOf(v any) string { s, _ := v.(string); return s }

// B55.48 — five wrong codes an hour, per workspace and per IP: the sixth answers 429 with Retry-After, before any work,
// so even the right code is refused then; another IP for the same workspace is refused too, and the same IP for another
// workspace.
func TestB5548_SixWrongCodesAnswer429(t *testing.T) {
	e, store := b5548NewEnv(t)
	ctx := context.Background()
	const ws, other = "ws-b5548-limit", "ws-b5548-limit-other"
	rc := e.lockedOut(ctx, store, ws)
	otherCode := e.lockedOut(ctx, store, other)
	recoverAt := func(ws, ip, code string) (int, map[string]any, http.Header) {
		return e.call(e.owner(ws), ip, time.Minute, http.MethodPost, "/v1/workspaces/"+ws+"/keys/recover",
			map[string]any{"code": code, "passkey": e.passkeyRegistration(ws)})
	}
	for i := 1; i <= 5; i++ {
		if code, out, _ := recoverAt(ws, "203.0.113.20", "0000 0000 0000 0000 0000"); code != http.StatusForbidden {
			t.Fatalf("wrong code %d = %d %v, want 403", i, code, out)
		}
	}
	code, out, h := recoverAt(ws, "203.0.113.20", "0000 0000 0000 0000 0000")
	if retry, err := strconv.Atoi(h.Get("Retry-After")); code != http.StatusTooManyRequests || err != nil || retry <= 0 || retry > 3600 {
		t.Errorf("sixth wrong code = %d %v, Retry-After %q; want 429 with Retry-After within the hour", code, out, h.Get("Retry-After"))
	}
	if code, out, _ := recoverAt(ws, "203.0.113.21", rc); code != http.StatusTooManyRequests {
		t.Errorf("the right code from another IP after five wrong = %d %v, want 429", code, out)
	}
	if code, out, _ := recoverAt(other, "203.0.113.20", otherCode); code != http.StatusTooManyRequests {
		t.Errorf("another workspace's right code from the same IP = %d %v, want 429", code, out)
	}
	if n := b5548LiveKeys(t, store, ws); n != 0 {
		t.Errorf("live keys = %d, want 0", n)
	}
	if code, out, _ := recoverAt(other, "203.0.113.22", otherCode); code != http.StatusCreated {
		t.Errorf("another workspace's right code from a fresh IP = %d %v, want 201", code, out)
	}
}

// B55.48 — only the owner, signed in within 10 minutes, starts, completes or recovers with a code: an admin, an API
// key, an agent's key and a Connect token are refused, and so is a sign-in older than 10 minutes, or none.
func TestB5548_OnlyTheOwnerRecentlySignedIn(t *testing.T) {
	e, store := b5548NewEnv(t)
	ctx := context.Background()
	const ws = "ws-b5548-who"
	rc := e.lockedOut(ctx, store, ws)
	base := "/v1/workspaces/" + ws + "/keys/recover"
	// Each carries the workspace, the owner's user and the keys scope, so only how it signed in refuses it.
	callers := map[string]*auth.AuthContext{
		"an admin":        {WorkspaceID: ws, UserID: "ada", AuthMethod: auth.MethodGlobalKey, IsAdmin: true, Scopes: []string{auth.ScopeAdmin, auth.ScopeKeys}},
		"an API key":      {WorkspaceID: ws, UserID: "ada", AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-admin", Scopes: []string{auth.ScopeAdmin, auth.ScopeKeys}},
		"an agent's key":  {WorkspaceID: ws, UserID: "ada", AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-agent", Scopes: []string{auth.ScopeProxy, auth.ScopeKeys}},
		"a Connect token": {WorkspaceID: ws, UserID: "ada", AuthMethod: auth.MethodConnect, ConnectAgentID: "agt", Scopes: []string{auth.ScopeProxy, auth.ScopeKeys}},
	}
	for who, actx := range callers {
		for _, p := range []string{base, base + "/start", base + "/complete"} {
			if code, out, _ := e.call(actx, "203.0.113.30", time.Minute, http.MethodPost, p, map[string]any{"code": rc, "passkey": e.passkeyRegistration(ws)}); code != http.StatusForbidden {
				t.Errorf("%s POST %s = %d %v, want 403", who, p, code, out)
			}
		}
	}
	for _, signedIn := range []time.Duration{11 * time.Minute, -1} {
		for _, p := range []string{base, base + "/start", base + "/complete"} {
			code, out, _ := e.call(e.owner(ws), "203.0.113.31", signedIn, http.MethodPost, p, map[string]any{"code": rc, "passkey": e.passkeyRegistration(ws)})
			if code != http.StatusForbidden || out["error"] != b5548SignInAgain {
				t.Errorf("owner signed in %v ago POST %s = %d %v, want 403 %q", signedIn, p, code, out, b5548SignInAgain)
			}
		}
	}
	if n := b5548LiveKeys(t, store, ws); n != 0 {
		t.Errorf("live keys = %d, want 0", n)
	}
	var waiting int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM recovery_requests WHERE workspace_id = $1`, ws).Scan(&waiting); err != nil || waiting != 0 {
		t.Errorf("recovery requests = %d (%v), want none", waiting, err)
	}
}

// B55.48 — recovery without the code: one waiting request per workspace, shown to every session; refused before
// ready_at; cancelled by an admin; completed only by the person who started it, from ready_at; expired 24 hours later.
func TestB5548_RecoveryAfter72Hours(t *testing.T) {
	e, store := b5548NewEnv(t)
	ctx := context.Background()
	const ws = "ws-b5548-wait"
	owner, ip := e.owner(ws), "203.0.113.40"
	base := "/v1/workspaces/" + ws + "/keys/recover"
	e.lockedOut(ctx, store, ws)
	admin := &auth.AuthContext{AuthMethod: auth.MethodGlobalKey, IsAdmin: true, Scopes: []string{auth.ScopeAdmin, auth.ScopeKeys}}
	bob := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "bob", Scopes: []string{auth.ScopeKeys}}

	code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, base+"/start", nil)
	if code != http.StatusCreated {
		t.Fatalf("start = %d %v, want 201", code, out)
	}
	rec, _ := out["recovery"].(map[string]any)
	ready, _ := time.Parse(time.RFC3339Nano, stringOf(rec["ready_at"]))
	if d := time.Until(ready); rec["status"] != "waiting" || d < 71*time.Hour || d > 73*time.Hour {
		t.Errorf("started recovery = %v, want waiting and ready in 72 hours", rec)
	}
	if code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, base+"/start", nil); code != http.StatusConflict {
		t.Errorf("a second start = %d %v, want 409", code, out)
	}
	if code, out, _ := e.call(admin, ip, -1, http.MethodGet, base, nil); code != http.StatusOK || out["recovery"] == nil {
		t.Errorf("GET recovery as an admin = %d %v, want the waiting request", code, out)
	}
	if code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, base+"/complete", map[string]any{"passkey": e.passkeyRegistration(ws)}); code != http.StatusConflict {
		t.Errorf("complete before ready_at = %d %v, want 409", code, out)
	}

	// An admin cancels it; there is then nothing to complete.
	if code, out, _ := e.call(admin, ip, -1, http.MethodPost, base+"/cancel", nil); code != http.StatusOK {
		t.Errorf("cancel as an admin = %d %v, want 200", code, out)
	}
	if a := b5548Audit(t, e, ws, "key_recovery_cancelled"); len(a) != 1 {
		t.Errorf("key_recovery_cancelled audit entries = %v, want one", a)
	}
	if code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, base+"/complete", map[string]any{"passkey": e.passkeyRegistration(ws)}); code != http.StatusNotFound {
		t.Errorf("complete a cancelled recovery = %d %v, want 404", code, out)
	}

	// Started again and ready: another person is refused, the owner who started it adds a key.
	if code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, base+"/start", nil); code != http.StatusCreated {
		t.Fatalf("start again = %d %v, want 201", code, out)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE recovery_requests SET ready_at = now() - interval '1 minute' WHERE workspace_id = $1 AND status = 'waiting'`, ws); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := e.call(bob, ip, time.Minute, http.MethodPost, base+"/complete", map[string]any{"passkey": e.passkeyRegistration(ws)}); code != http.StatusForbidden {
		t.Errorf("complete by another person = %d %v, want 403", code, out)
	}
	code, out, _ = e.call(owner, ip, time.Minute, http.MethodPost, base+"/complete", map[string]any{"device": e.deviceRegistration(ws)})
	if code != http.StatusCreated || !b5548CodeShape.MatchString(stringOf(out["recovery_code"])) {
		t.Fatalf("complete when ready = %d %v, want 201 with a recovery_code", code, out)
	}
	if n := b5548LiveKeys(t, store, ws); n != 1 {
		t.Errorf("live keys after completing = %d, want 1", n)
	}
	if a := b5548Audit(t, e, ws, "key_recovered"); len(a) != 1 || a[0]["way"] != "wait" {
		t.Errorf("key_recovered audit entries = %v, want one with way wait", a)
	}

	// Expired 24 hours after ready_at.
	if _, err := e.pool.Exec(ctx, `UPDATE workspace_device_keys SET revoked_at = now() WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, base+"/start", nil); code != http.StatusCreated {
		t.Fatalf("start a third time = %d %v, want 201", code, out)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE recovery_requests SET ready_at = now() - interval '25 hours' WHERE workspace_id = $1 AND status = 'waiting'`, ws); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, base+"/complete", map[string]any{"passkey": e.passkeyRegistration(ws)}); code != http.StatusGone {
		t.Errorf("complete 25 hours after ready_at = %d %v, want 410", code, out)
	}
	if code, out, _ := e.call(owner, ip, -1, http.MethodGet, base, nil); code != http.StatusOK || out["recovery"] != nil {
		t.Errorf("GET recovery after it expired = %d %v, want none waiting", code, out)
	}
	if n := b5548LiveKeys(t, store, ws); n != 0 {
		t.Errorf("live keys after an expired recovery = %d, want 0", n)
	}

	// A key added another way since it began (after a lockdown was lifted, say), then lost: the recovery no longer stands.
	if code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, base+"/start", nil); code != http.StatusCreated {
		t.Fatalf("start a fourth time = %d %v, want 201", code, out)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO workspace_passkeys (credential_id, workspace_id, name, public_key, sign_count, revoked_at)
		VALUES ('cred-b5548-lift', $1, 'After the lift', '\x00', 0, now())`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE recovery_requests SET ready_at = now() - interval '1 minute' WHERE workspace_id = $1 AND status = 'waiting'`, ws); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := e.call(owner, ip, time.Minute, http.MethodPost, base+"/complete", map[string]any{"passkey": e.passkeyRegistration(ws)}); code != http.StatusNotFound {
		t.Errorf("complete a recovery a later key superseded = %d %v, want 404", code, out)
	}
	if n := b5548LiveKeys(t, store, ws); n != 0 {
		t.Errorf("live keys after a superseded recovery = %d, want 0", n)
	}
}
