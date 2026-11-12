package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// b5241Connect is a router behind AuthMiddleware with the Connect mint, the agent routes and the money-request routes,
// and a store whose two agents of ws are owned by user-owner: buyer funded with 10 LXC, seller with none.
type b5241Connect struct {
	t              *testing.T
	pool           *pgxpool.Pool
	store          *economy.DualTokenStore
	r              chi.Router
	ws, buyer, sel string
}

const b5241MintKey = "test-connect-mint-key-long-enough"

func newB5241Connect(t *testing.T, ws string) *b5241Connect {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified) VALUES ($1, $1, $1, true)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetOwnerVerifier(earnverify.New(false))
	jwtKey, err := auth.GenerateECKey()
	if err != nil {
		t.Fatal(err)
	}
	am := auth.NewManager(mintTestGlobalKey, jwtKey, auth.New(pool), nil).WithConnectMintKey(b5241MintKey)
	r := chi.NewRouter()
	r.Use(auth.AuthMiddleware(auth.New(pool), am))
	r.Post("/v1/auth/connect-token", newConnectTokenMintHandler(am, store))
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountAgentTransferRoutes(r, store)
	c := &b5241Connect{t: t, pool: pool, store: store, r: r, ws: ws}
	agent := func(name string, fund int64) string {
		a, err := store.CreateAgent(ctx, ws, name, "user-owner")
		if err != nil {
			t.Fatal(err)
		}
		if fund > 0 {
			if _, err := store.FundAgent(ctx, ws, a.ID, fund); err != nil {
				t.Fatal(err)
			}
		}
		return a.ID
	}
	c.buyer, c.sel = agent("buyer", 10_000_000), agent("seller", 0)
	return c
}

func (c *b5241Connect) call(method, bearer, path, body, idem string) (int, string) {
	c.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	w := httptest.NewRecorder()
	c.r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func (c *b5241Connect) mint(agentID, scope string) string {
	c.t.Helper()
	code, body := c.call(http.MethodPost, b5241MintKey, "/v1/auth/connect-token",
		`{"workspace_id":"`+c.ws+`","user_id":"user-owner","client_id":"https://chatgpt.com","agent_id":"`+agentID+`","scopes":["`+scope+`"]}`, "")
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &minted); code != http.StatusCreated || err != nil || minted.Token == "" {
		c.t.Fatalf("mint a %s Connect token = %d %s", scope, code, body)
	}
	return minted.Token
}

func (c *b5241Connect) count(query string, args ...any) (n int64) {
	c.t.Helper()
	if err := c.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		c.t.Fatal(err)
	}
	return n
}

// B52.41 (1) — a payments:prepare Connect token's payment inside the rules files one pending approval naming the
// assistant and posts nothing; once the owner approves it, the retry pays once.
func TestB5241_PrepareOnlyTokenFilesAnApprovalAndPostsNothing(t *testing.T) {
	c := newB5241Connect(t, "ws-b5241-prepare")
	ctx := context.Background()
	// The rules would let 0.1 LXC through on a payments:send token.
	if _, err := c.store.SetAgentRules(ctx, c.ws, c.buyer, economy.AgentRules{ApprovalAboveULXC: 5_000_000}); err != nil {
		t.Fatal(err)
	}
	tok := c.mint(c.buyer, auth.ScopePaymentsPrepare)
	pay := "/v1/workspaces/" + c.ws + "/agents/" + c.buyer + "/pay"
	small := `{"to_agent_id":"` + c.sel + `","amount_ulxc":100000}`
	pays := func() int64 {
		return c.count(`SELECT count(*) FROM agent_postings WHERE kind = 'pay' AND account = $1`, "agent:"+c.buyer)
	}

	code, body := c.call(http.MethodPost, tok, pay, small, "prep-1")
	if code != http.StatusAccepted || !strings.Contains(body, `"pending_approval"`) {
		t.Fatalf("a prepare-only payment inside the rules = %d %s, want 202 pending_approval", code, body)
	}
	if n := pays(); n != 0 {
		t.Fatalf("a prepare-only token moved money: %d pay postings, want 0", n)
	}
	if n := c.count(`SELECT count(*) FROM agent_approvals WHERE agent_id = $1 AND status = 'pending' AND connect_client_id = 'https://chatgpt.com'`, c.buyer); n != 1 {
		t.Fatalf("%d pending approvals naming the assistant, want 1", n)
	}
	// A retry files no second approval and still moves nothing.
	if code, body := c.call(http.MethodPost, tok, pay, small, "prep-1"); code != http.StatusAccepted || pays() != 0 {
		t.Fatalf("a retry = %d %s with %d pay postings, want 202 and 0", code, body, pays())
	}
	if n := c.count(`SELECT count(*) FROM agent_approvals WHERE agent_id = $1`, c.buyer); n != 1 {
		t.Fatalf("%d approvals after a retry, want 1", n)
	}
	// The approvals list names the assistant, so an approval screen can.
	code, body = c.call(http.MethodGet, mintTestGlobalKey, "/v1/workspaces/"+c.ws+"/agents/approvals", "", "")
	if code != http.StatusOK || !strings.Contains(body, `"connect_client_id":"https://chatgpt.com"`) {
		t.Fatalf("GET approvals = %d %s, want connect_client_id on the approval", code, body)
	}
	// The owner approves it; the retry pays once and the ledger has the pair.
	var approvalID string
	if err := c.pool.QueryRow(ctx, `SELECT id FROM agent_approvals WHERE agent_id = $1`, c.buyer).Scan(&approvalID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.DecideAgentApproval(ctx, c.ws, approvalID, true); err != nil {
		t.Fatal(err)
	}
	if code, body := c.call(http.MethodPost, tok, pay, small, "prep-1"); code != http.StatusOK {
		t.Fatalf("the retry after the owner approved = %d %s, want 200", code, body)
	}
	if n := pays(); n != 1 {
		t.Fatalf("%d pay postings from the payer after approval, want 1", n)
	}
	if n := c.count(`SELECT COALESCE(sum(amount_ulxc), 0) FROM agent_postings WHERE kind = 'pay' AND account IN ($1, $2)`,
		"agent:"+c.buyer, "agent:"+c.sel); n != 0 {
		t.Fatalf("the payment's postings sum to %d, want 0", n)
	}
	// Another payment, approved nowhere, files its own approval again.
	if code, body := c.call(http.MethodPost, tok, pay, `{"to_agent_id":"`+c.sel+`","amount_ulxc":200000}`, "prep-2"); code != http.StatusAccepted || pays() != 1 {
		t.Fatalf("a second prepare-only payment = %d %s with %d pay postings, want 202 and 1", code, body, pays())
	}
}

// B52.41 (2) — a Connect token accepts a money request made of its own agent once (one transfer pair), answers a
// retry with the same Idempotency-Key as made, and is 403 on a request made of another agent.
func TestB5241_ConnectTokenAcceptsAMoneyRequestOfItsOwnAgentOnce(t *testing.T) {
	c := newB5241Connect(t, "ws-b5241-accept")
	ctx := context.Background()
	other, err := c.store.CreateAgent(ctx, c.ws, "other", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.FundAgent(ctx, c.ws, other.ID, 5_000_000); err != nil {
		t.Fatal(err)
	}
	ofBuyer, err := c.store.RequestCredits(ctx, c.ws, c.sel, c.buyer, 300_000, "invoice 7")
	if err != nil {
		t.Fatal(err)
	}
	ofOther, err := c.store.RequestCredits(ctx, c.ws, c.sel, other.ID, 300_000, "invoice 8")
	if err != nil {
		t.Fatal(err)
	}
	tok := c.mint(c.buyer, auth.ScopePaymentsSend)
	accept := func(id string) string { return "/v1/workspaces/" + c.ws + "/money-requests/" + id + "/accept" }
	transfers := func() int64 {
		return c.count(`SELECT count(*) FROM agent_postings WHERE kind = 'transfer' AND account IN ($1, $2, $3)`,
			"agent:"+c.buyer, "agent:"+c.sel, "agent:"+other.ID)
	}

	if code, body := c.call(http.MethodPost, tok, accept(ofBuyer.ID), "", ""); code != http.StatusBadRequest || transfers() != 0 {
		t.Fatalf("an accept without an Idempotency-Key = %d %s with %d postings, want 400 and 0", code, body, transfers())
	}
	if code, body := c.call(http.MethodPost, tok, accept(ofOther.ID), "", "acc-other"); code != http.StatusForbidden || transfers() != 0 {
		t.Fatalf("accepting another agent's request = %d %s with %d postings, want 403 and 0", code, body, transfers())
	}
	code, body := c.call(http.MethodPost, tok, accept(ofBuyer.ID), "", "acc-1")
	if code != http.StatusOK || !strings.Contains(body, `"accepted"`) {
		t.Fatalf("accept the request made of the token's agent = %d %s, want 200 accepted", code, body)
	}
	if code, body := c.call(http.MethodPost, tok, accept(ofBuyer.ID), "", "acc-1"); code != http.StatusOK || !strings.Contains(body, `"accepted"`) {
		t.Fatalf("a retry with the same Idempotency-Key = %d %s, want 200 accepted", code, body)
	}
	if code, body := c.call(http.MethodPost, tok, accept(ofBuyer.ID), "", "acc-2"); code == http.StatusOK {
		t.Fatalf("a second accept under another key = %d %s, want a refusal", code, body)
	}
	if n := transfers(); n != 2 {
		t.Fatalf("%d transfer postings, want one pair", n)
	}
	if n := c.count(`SELECT COALESCE(sum(amount_ulxc), 0) FROM agent_postings WHERE kind = 'transfer' AND account = $1`, "agent:"+c.buyer); n != -300_000 {
		t.Fatalf("the buyer's transfer postings sum to %d, want -300000", n)
	}
	// The token reaches no other money-request route.
	if code, body := c.call(http.MethodPost, tok, "/v1/workspaces/"+c.ws+"/money-requests/"+ofOther.ID+"/decline", "", "dec-1"); code != http.StatusForbidden {
		t.Fatalf("a Connect token declining a request = %d %s, want 403", code, body)
	}
}

// B52.41 (3) — a raise filed as a proposal changes nothing until it is approved with the workspace's key over its
// challenge; a proposal that only tightens applies at once.
func TestB5241_ARaiseProposalWaitsForThePasskey(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetDeviceKeys(devicekey.Verifier{AllowTest: true})
	r := chi.NewRouter()
	mountDeviceKeyRoutes(r, store)
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	const ws = "ws-b5241-raise"
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	call := func(path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	a, err := store.CreateAgent(ctx, ws, "spender", "user-ada")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, a.ID, economy.AgentRules{DailyLimitULXC: 5_000_000}); err != nil {
		t.Fatal(err)
	}
	daily := func() int64 {
		t.Helper()
		got, err := store.GetAgentRules(ctx, ws, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.DailyLimitULXC
	}
	b64 := base64.RawURLEncoding.EncodeToString
	phone, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&phone.PublicKey)
	sign := func(challenge string) string {
		h := sha256.Sum256(devicekey.Message(challenge))
		sig, _ := ecdsa.SignASN1(rand.Reader, phone, h[:])
		return b64(sig)
	}
	_, ch := call("/v1/money/devices/challenge", "")
	regChallenge, _ := ch["challenge"].(string)
	code, dev := call("/v1/money/devices", jsonOf(map[string]string{"platform": "test", "name": "Ada's phone",
		"public_key": b64(spki), "challenge": regChallenge, "signature": sign(regChallenge), "attestation": ""}))
	if code != http.StatusCreated {
		t.Fatalf("register device = %d %v", code, dev)
	}
	proposals := "/v1/workspaces/" + ws + "/agents/" + a.ID + "/rules/proposals"

	// A raise is kept as a proposal and changes nothing.
	code, p := call(proposals, `{"rules":{"daily_limit_ulxc":9000000}}`)
	if code != http.StatusCreated || p["status"] != "pending" || p["id"] == nil || daily() != 5_000_000 {
		t.Fatalf("file a raise = %d %v, daily %d; want 201 pending and 5000000 kept", code, p, daily())
	}
	id := p["id"].(string)
	// Approving it unsigned, or with a signature over another challenge, changes nothing.
	if code, out := call(proposals+"/"+id+"/approve", `{}`); code != http.StatusForbidden || daily() != 5_000_000 {
		t.Fatalf("approve unsigned = %d %v, daily %d; want 403 and 5000000 kept", code, out, daily())
	}
	_, other := call("/v1/workspaces/"+ws+"/agents/"+a.ID+"/rules/challenge", `{"rules":{"daily_limit_ulxc":20000000}}`)
	wrong := other["challenge"].(string)
	if code, out := call(proposals+"/"+id+"/approve", jsonOf(map[string]any{"assertion": map[string]string{
		"device_id": dev["id"].(string), "challenge": wrong, "signature": sign(wrong)}})); code != http.StatusForbidden || daily() != 5_000_000 {
		t.Fatalf("approve on another change's signature = %d %v, daily %d; want 403 and 5000000 kept", code, out, daily())
	}
	// Signed over its own challenge, it applies, and the rules read the new limit.
	code, c := call(proposals+"/"+id+"/challenge", "")
	if code != http.StatusOK || c["looser"] != true || c["challenge"] == nil || jsonOf(c["changes"]) != `["Daily limit: was $0.50 (5 LXC), now $0.90 (9 LXC)"]` {
		t.Fatalf("the proposal's challenge = %d %v, want looser with the change in words", code, c)
	}
	challenge := c["challenge"].(string)
	if code, out := call(proposals+"/"+id+"/approve", jsonOf(map[string]any{"assertion": map[string]string{
		"device_id": dev["id"].(string), "challenge": challenge, "signature": sign(challenge)}})); code != http.StatusOK || daily() != 9_000_000 {
		t.Fatalf("approve signed = %d %v, daily %d; want 200 and 9000000", code, out, daily())
	}
	// It applies once.
	if code, out := call(proposals+"/"+id+"/approve", `{}`); code != http.StatusNotFound {
		t.Fatalf("approve it again = %d %v, want 404", code, out)
	}
	// A proposal that only tightens applies at once.
	if code, out := call(proposals, `{"rules":{"daily_limit_ulxc":2000000}}`); code != http.StatusOK || out["status"] != "applied" || daily() != 2_000_000 {
		t.Fatalf("file a lowering = %d %v, daily %d; want 200 applied and 2000000", code, out, daily())
	}
}
