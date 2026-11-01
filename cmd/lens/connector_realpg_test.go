package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/connector"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/mcp"
	"github.com/talyvor/lens/internal/tenant"
)

type connectorMeter struct{}

func (connectorMeter) MeterMarketUse(context.Context, string, string, int64, time.Time) error {
	return nil
}

// B41.178 — what B41.78's connect page and an assistant do, through the routes main.go mounts: the listing's own MCP
// URL answers 401 naming its resource metadata, which names this authorisation server; the assistant registers,
// authorizes with PKCE, the owner reads the request and approves it for an agent, and the code buys a connector
// token once — a wrong verifier spends it, and a spent code buys nothing. The token opens only that listing's URL,
// where the listing is the one tool, and one call of it is one billed use on the buyer's marketplace bill, logged as
// the agent's market_use; revoking the key behind the token ends it.
func TestB41178_AListingAddedThroughTheConnectorIsOneBilledUse(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const seller, buyer, pub = "ws-b41178-seller", "ws-b41178-buyer", "https://lens.test"
	for _, ws := range []string{seller, buyer} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	store := market.NewStore(pool)
	adder, err := store.Publish(ctx, seller, market.Draft{Kind: "prompt", Title: "Adder", Description: "adds two numbers", Visibility: "public",
		Artifact: json.RawMessage(`{"template":"what is {{a}} + {{b}}?","model":"claude-haiku-4-5"}`),
		Offers:   []market.Offer{{Kind: market.OfferPerUse, Licence: market.LicenceCommercial, PriceUSDMicros: 50_000}}})
	if err != nil {
		t.Fatal(err)
	}
	bank := economy.NewDualTokenStore(nil, pool, nil)
	agent, err := bank.CreateAgent(ctx, buyer, "Claude", "owner-"+buyer)
	if err != nil {
		t.Fatal(err)
	}
	mcpServer := mcp.New(pool, nil, nil, nil, nil, "test")
	mcpServer.SetAgentBank(bank)
	// The model calls go through the in-process proxy, as the key behind the token; this one records who called and
	// answers 4. Through the router (lens) they would carry the token, which opens nothing there.
	var ranAs []string
	wallet := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if a := auth.GetAuthContext(req.Context()); a != nil {
			ranAs = append(ranAs, a.APIKeyID)
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"content": []map[string]string{{"type": "text", "text": "4"}}})
	})
	lens := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSONErr(w, http.StatusUnauthorized, "not this way") })
	mcpServer.SetMarket(store, mcpMarketDeps{lens: lens, wallet: wallet, meter: connectorMeter{}, agents: bank})
	grants := connector.NewStore(pool)
	keyStore := auth.New(pool)
	r := chi.NewRouter()
	mountConnectorPublicRoutes(r, grants, bank, tenant.NewStore(pool), pub)
	mountConnectorOwnerRoutes(r, grants, bank)
	r.With(listingChallenge(pub), connectorAuth(grants)).Post("/mcp/listings/{listingID}", func(w http.ResponseWriter, req *http.Request) {
		mcpServer.HandleListingRPC(w, req, chi.URLParam(req, "listingID"))
	})
	r.With(auth.AuthMiddleware(keyStore, auth.NewManager("", nil, keyStore, tenant.NewStore(pool)))).Post("/mcp", mcpServer.HandleRPC)

	owner := &auth.AuthContext{WorkspaceID: buyer, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	do := func(who *auth.AuthContext, bearer, method, path, contentType, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if who != nil {
			req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	decode := func(w *httptest.ResponseRecorder, want int, out any) {
		t.Helper()
		if w.Code != want {
			t.Fatalf("status %d, want %d: %s", w.Code, want, w.Body.String())
		}
		if out != nil {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("%v in %s", err, w.Body.String())
			}
		}
	}
	mcpPath := "/mcp/listings/" + adder.ID
	rpc := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	// Discovery, as an assistant does it from nothing but the listing's URL.
	w := do(nil, "", http.MethodPost, mcpPath, "application/json", rpc)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"),
		`resource_metadata="`+pub+"/.well-known/oauth-protected-resource"+mcpPath+`"`) {
		t.Fatalf("no key = %d, WWW-Authenticate %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
	var resource struct {
		Resource string   `json:"resource"`
		Servers  []string `json:"authorization_servers"`
	}
	decode(do(nil, "", http.MethodGet, "/.well-known/oauth-protected-resource"+mcpPath, "", ""), http.StatusOK, &resource)
	if resource.Resource != pub+mcpPath || len(resource.Servers) != 1 || resource.Servers[0] != pub {
		t.Fatalf("resource metadata = %+v", resource)
	}
	var meta map[string]any
	decode(do(nil, "", http.MethodGet, "/.well-known/oauth-authorization-server", "", ""), http.StatusOK, &meta)
	if meta["token_endpoint"] != pub+"/oauth/token" || meta["registration_endpoint"] != pub+"/oauth/register" {
		t.Fatalf("server metadata = %v", meta)
	}

	// Registration: a script redirect is refused; Claude's callback is not.
	decode(do(nil, "", http.MethodPost, "/oauth/register", "application/json", `{"client_name":"Claude","redirect_uris":["javascript:alert(1)"]}`), http.StatusBadRequest, nil)
	const callback = "https://claude.ai/api/mcp/auth_callback"
	var client connector.Client
	decode(do(nil, "", http.MethodPost, "/oauth/register", "application/json", `{"client_name":"Claude","redirect_uris":["`+callback+`"]}`), http.StatusCreated, &client)

	verifier := strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	connect := func() string { // authorize, the owner reads and approves, and the code comes back to the callback
		t.Helper()
		q := url.Values{"response_type": {"code"}, "client_id": {client.ID}, "redirect_uri": {callback}, "state": {"s1"},
			"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "resource": {pub + mcpPath}}
		w := do(nil, "", http.MethodGet, "/oauth/authorize?"+q.Encode(), "", "")
		to, _ := url.Parse(w.Header().Get("Location"))
		if w.Code != http.StatusFound || !strings.HasPrefix(to.String(), connectURLBase+"?request=") {
			t.Fatalf("authorize = %d to %q: %s", w.Code, to, w.Body.String())
		}
		reqPath := "/v1/workspaces/" + buyer + "/connector-requests/" + to.Query().Get("request")
		var pending connector.Request
		decode(do(owner, "", http.MethodGet, reqPath, "", ""), http.StatusOK, &pending)
		if pending.ClientName != "Claude" || pending.RedirectHost != "https://claude.ai" || pending.ListingID != adder.ID {
			t.Fatalf("the connect page reads %+v", pending)
		}
		var approved struct {
			RedirectTo string `json:"redirect_to"`
		}
		decode(do(owner, "", http.MethodPost, reqPath+"/approve", "application/json", `{"agent_id":"`+agent.ID+`"}`), http.StatusOK, &approved)
		back, _ := url.Parse(approved.RedirectTo)
		if !strings.HasPrefix(approved.RedirectTo, callback+"?") || back.Query().Get("state") != "s1" || back.Query().Get("code") == "" {
			t.Fatalf("approve sends the owner to %q", approved.RedirectTo)
		}
		decode(do(owner, "", http.MethodPost, reqPath+"/approve", "application/json", `{"agent_id":"`+agent.ID+`"}`), http.StatusNotFound, nil)
		return back.Query().Get("code")
	}
	exchange := func(code, verifier string, want int) string {
		t.Helper()
		form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {client.ID}, "redirect_uri": {callback}, "code_verifier": {verifier}}
		var tok struct {
			AccessToken string `json:"access_token"`
			Error       string `json:"error"`
		}
		decode(do(nil, "", http.MethodPost, "/oauth/token", "application/x-www-form-urlencoded", form.Encode()), want, &tok)
		if want != http.StatusOK && tok.Error != "invalid_grant" {
			t.Fatalf("a refused exchange says %q", tok.Error)
		}
		return tok.AccessToken
	}
	stolen := connect()
	exchange(stolen, strings.Repeat("x", 50), http.StatusBadRequest) // not the verifier: refused, and the code is spent
	exchange(stolen, verifier, http.StatusBadRequest)
	code := connect()
	key := exchange(code, verifier, http.StatusOK)
	exchange(code, verifier, http.StatusBadRequest) // a code buys one key
	if n := 0; pool.QueryRow(ctx, `SELECT count(*) FROM agent_account_keys WHERE agent_id = $1`, agent.ID).Scan(&n) != nil || n != 1 || !strings.HasPrefix(key, "tlc_") {
		t.Fatalf("the agent holds %d keys and the assistant %q; want one key behind a connector token", n, key)
	}
	// The token opens its listing's URL and nothing else: not /mcp, where the agent's other tools are.
	if w := do(nil, key, http.MethodPost, "/mcp", "application/json", rpc); w.Code != http.StatusUnauthorized {
		t.Fatalf("the connector token on /mcp = %d %s; want 401", w.Code, w.Body.String())
	}
	if w := do(nil, key, http.MethodPost, "/mcp/listings/"+adder.ID+"x", "application/json", rpc); w.Code != http.StatusUnauthorized {
		t.Fatalf("the connector token on another listing = %d; want 401", w.Code)
	}

	type rpcAnswer struct {
		Result struct {
			Tools   []map[string]any `json:"tools"`
			IsError bool             `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	var list rpcAnswer
	decode(do(nil, key, http.MethodPost, mcpPath, "application/json", rpc), http.StatusOK, &list)
	if len(list.Result.Tools) != 1 || list.Result.Tools[0]["name"] != "adder" {
		t.Fatalf("the listing's URL lists %v; want only its own tool, adder", list.Result.Tools)
	}
	var other rpcAnswer
	decode(do(nil, key, http.MethodPost, mcpPath, "application/json",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"market_use","arguments":{"listing_id":"`+adder.ID+`"}}}`), http.StatusOK, &other)
	if other.Error == nil {
		t.Fatalf("market_use on a listing's URL = %+v; want unknown tool", other)
	}
	var used rpcAnswer
	decode(do(nil, key, http.MethodPost, mcpPath, "application/json",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"adder","arguments":{"variables":{"a":"2","b":"2"},"max_price_usd_micros":50000}}}`), http.StatusOK, &used)
	if used.Error != nil || used.Result.IsError || len(used.Result.Content) == 0 || !strings.Contains(used.Result.Content[0].Text, `4`) {
		t.Fatalf("using the adder = %+v", used)
	}
	var billed, price int64
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(price_ulxc), 0) FROM market_uses
		WHERE listing_id = $1 AND buyer_workspace_id = $2 AND agent_id = $3 AND charge = 'billed'`, adder.ID, buyer, agent.ID).Scan(&billed, &price); err != nil {
		t.Fatal(err)
	}
	var all, logged int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM market_uses WHERE listing_id = $1),
		(SELECT count(*) FROM agent_tool_calls WHERE agent_id = $2 AND tool = 'market_use' AND outcome = 'ok')`, adder.ID, agent.ID).Scan(&all, &logged); err != nil {
		t.Fatal(err)
	}
	var behind string
	if err := pool.QueryRow(ctx, `SELECT key_id FROM connector_grants WHERE access_hash IS NOT NULL`).Scan(&behind); err != nil {
		t.Fatal(err)
	}
	if len(ranAs) != 1 || ranAs[0] != behind {
		t.Fatalf("the listing's model calls ran as %v; want once, as the key behind the token (%s)", ranAs, behind)
	}
	if billed != 1 || price <= 0 || all != 1 || logged != 1 {
		t.Fatalf("one use from the assistant wrote %d billed rows (%d µLXC) of %d, and %d logged calls; want one billed use", billed, price, all, logged)
	}
	// The owner revokes the key the connection holds, and the assistant can use the listing no more.
	if _, err := pool.Exec(ctx, `DELETE FROM workspace_api_keys WHERE id::text = (SELECT key_id FROM connector_grants WHERE access_hash IS NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if w := do(nil, key, http.MethodPost, mcpPath, "application/json", rpc); w.Code != http.StatusUnauthorized {
		t.Fatalf("after the key was revoked = %d; want 401", w.Code)
	}
}
