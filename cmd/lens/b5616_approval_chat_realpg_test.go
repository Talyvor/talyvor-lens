package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/approvalchat"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/envelope"
)

// B56.16 — a Team workspace's owner saves a Slack incoming webhook and a Business one a Teams one; each approval filed
// is posted once to its own, with the agent, the payee, the amount, the reason, the expiry and one button opening it
// in Talyvor, through a 503 the poster retries. Free is refused and posts nothing, the webhook is in no answer and no
// log, and no route takes a decision from Slack or Teams.
func TestB5616_ApprovalsPostToSlackAndTeams(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const slackURL = "https://hooks.slack.com/services/T0B5616/B0B5616/slacksecretb5616"
	const teamsURL = "https://prod-01.westeurope.logic.azure.com:443/workflows/b5616/triggers/manual/paths/invoke?sig=teamssecretb5616"
	for _, q := range []string{
		`INSERT INTO subscriptions (workspace_id, stripe_subscription_id, stripe_customer_id, price_id, status, livemode, last_event_at, plan)
			VALUES ('ws-b5616-team', 'sub_b5616_team', 'cus_b5616_team', 'price_team', 'active', false, NOW(), 'team'),
			       ('ws-b5616-business', 'sub_b5616_business', 'cus_b5616_business', 'price_business', 'active', false, NOW(), 'business')`,
		`INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ('ws-b5616-team', 20000000, 20000000),
			('ws-b5616-business', 20000000, 20000000), ('ws-b5616-free', 20000000, 20000000)`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	// The fake Slack and Teams: every request is kept; the first one answers 503, as a busy webhook does.
	type post struct {
		host, path string
		body       map[string]any
	}
	var mu sync.Mutex
	var got []post
	tries, failing := 0, false
	fake := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if tries++; tries == 1 || failing {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		got = append(got, post{r.Host, r.URL.Path, body})
	}))
	t.Cleanup(fake.Close)
	// Every host dials the fake, so the webhooks keep their real hosts; the fake's certificate is for example.com.
	roots := x509.NewCertPool()
	roots.AddCert(fake.Certificate())
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, fake.Listener.Addr().String())
		},
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com"},
	}}
	ring, err := envelope.NewKeyring(bytes.Repeat([]byte{7}, envelope.KEKLen))
	if err != nil {
		t.Fatal(err)
	}
	chat := approvalchat.New(pool, ring, client)

	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	r := chi.NewRouter()
	mountApprovalChatRoutes(r, chat)
	var answers strings.Builder
	call := func(method, ws, body string, a *auth.AuthContext) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, "/v1/workspaces/"+ws+"/approval-chat", strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), a))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		answers.WriteString(w.Body.String())
		return w.Code, w.Body.String()
	}
	owner := func(ws string) *auth.AuthContext {
		return &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	}

	// Only the owner or an admin saves one; only a Slack or Teams webhook; only on a plan with it.
	session := &auth.AuthContext{WorkspaceID: "ws-b5616-team", AuthMethod: auth.MethodSessionKey}
	if code, body := call(http.MethodPut, "ws-b5616-team", `{"kind":"slack","url":"`+slackURL+`"}`, session); code != http.StatusForbidden {
		t.Errorf("a session key saving the webhook = %d %s, want 403", code, body)
	}
	for _, bad := range []string{`{"kind":"slack","url":"https://127.0.0.1/services/x"}`, `{"kind":"slack","url":"http://hooks.slack.com/services/x"}`,
		`{"kind":"teams","url":"https://hooks.slack.com/services/x"}`, `{"kind":"discord","url":"https://hooks.slack.com/services/x"}`} {
		if code, body := call(http.MethodPut, "ws-b5616-team", bad, owner("ws-b5616-team")); code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d %s, want 400", bad, code, body)
		}
	}
	if code, body := call(http.MethodPut, "ws-b5616-free", `{"kind":"slack","url":"`+slackURL+`"}`, owner("ws-b5616-free")); code != http.StatusPaymentRequired ||
		!strings.Contains(body, "Free does not include Slack and Teams approvals. Team includes them.") {
		t.Errorf("Free saving the webhook = %d %s, want 402 naming Team", code, body)
	}
	if code, body := call(http.MethodPut, "ws-b5616-team", `{"kind":"slack","url":"`+slackURL+`"}`, owner("ws-b5616-team")); code != http.StatusOK ||
		!strings.Contains(body, `"kind":"slack"`) || !strings.Contains(body, `"host":"hooks.slack.com"`) {
		t.Fatalf("Team saving a Slack webhook = %d %s", code, body)
	}
	if code, body := call(http.MethodPut, "ws-b5616-business", `{"kind":"teams","url":"`+teamsURL+`"}`, owner("ws-b5616-business")); code != http.StatusOK {
		t.Fatalf("Business saving a Teams webhook = %d %s", code, body)
	}
	if code, body := call(http.MethodGet, "ws-b5616-team", "", owner("ws-b5616-team")); code != http.StatusOK || !strings.Contains(body, `"on":true`) {
		t.Errorf("GET = %d %s", code, body)
	}

	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalChat(chat)
	agents := map[string][2]string{}
	file := func(ws string, amount int64) economy.AgentApproval {
		t.Helper()
		if _, ok := agents[ws]; !ok {
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
			agents[ws] = [2]string{agent.ID, payee.ID}
		}
		a, err := store.RequestPaymentApproval(ctx, ws, agents[ws][0], agents[ws][1], amount, "invoice 7", "October <!channel> hosting")
		if err != nil {
			t.Fatal(err)
		}
		// Asked again, it is the same approval and nobody is told twice.
		if _, err := store.RequestPaymentApproval(ctx, ws, agents[ws][0], agents[ws][1], amount, "invoice 7", "October <!channel> hosting"); err != nil {
			t.Fatal(err)
		}
		return a
	}
	wait := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			mu.Lock()
			k := len(got)
			mu.Unlock()
			if k >= n {
				break
			}
		}
		time.Sleep(300 * time.Millisecond) // a post that should not come has had its chance
		mu.Lock()
		defer mu.Unlock()
		if len(got) != n {
			t.Fatalf("%d posts arrived, want %d: %v", len(got), n, got)
		}
	}

	// Team's approval reaches Slack once, after one 503.
	slackApproval := file("ws-b5616-team", 2_000_000)
	wait(1)
	s := got[0]
	raw := dump(s.body)
	link := economy.ApprovalURLBase + slackApproval.ID
	if s.host != "hooks.slack.com" || s.path != "/services/T0B5616/B0B5616/slacksecretb5616" || tries != 2 {
		t.Errorf("the Slack post went to %s%s after %d tries", s.host, s.path, tries)
	}
	for _, want := range []string{"researcher wants to pay vendor $0.20 (2 LXC)", "October &lt;!channel&gt; hosting", "Expires ",
		`"url":"` + link + `"`, `"type":"button"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the Slack post lacks %q: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "<!channel>") || strings.Contains(string(raw), "action_id") {
		t.Errorf("the Slack post carries the agent's mention or an interactive action: %s", raw)
	}

	// Business's reaches Teams once, as an Adaptive Card whose one action opens the approval in Talyvor.
	teamsApproval := file("ws-b5616-business", 3_000_000)
	wait(2)
	raw = dump(got[1].body)
	if got[1].host != "prod-01.westeurope.logic.azure.com:443" || !strings.Contains(string(raw), "researcher wants to pay vendor $0.30 (3 LXC)") ||
		!strings.Contains(string(raw), `"type":"Action.OpenUrl"`) || !strings.Contains(string(raw), economy.ApprovalURLBase+teamsApproval.ID) ||
		strings.Count(string(raw), `"type":"Action.`) != 1 {
		t.Errorf("the Teams post to %s = %s", got[1].host, raw)
	}

	// Team's subscription ends: the workspace is on Free and its approvals post nothing.
	if _, err := pool.Exec(ctx, `UPDATE subscriptions SET status = 'canceled' WHERE workspace_id = 'ws-b5616-team'`); err != nil {
		t.Fatal(err)
	}
	file("ws-b5616-team", 4_000_000)
	wait(2)
	if code, body := call(http.MethodGet, "ws-b5616-team", "", owner("ws-b5616-team")); code != http.StatusOK || !strings.Contains(body, `"on":false`) {
		t.Errorf("GET on Free = %d %s", code, body)
	}

	// A webhook that keeps failing is logged without itself.
	mu.Lock()
	failing = true
	mu.Unlock()
	file("ws-b5616-business", 5_000_000)
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline) && !strings.Contains(logs.String(), "approval chat"); {
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "approval chat") {
		t.Errorf("a failing webhook was not logged: %s", logs.String())
	}
	for _, secret := range []string{"slacksecretb5616", "teamssecretb5616", "B0B5616", "workflows/b5616"} {
		if strings.Contains(logs.String(), secret) || strings.Contains(answers.String(), secret) {
			t.Errorf("the webhook's %q is in a log or an answer:\nlogs %s\nanswers %s", secret, logs.String(), answers.String())
		}
	}

	// The owner removes it; and the routes are these three, none of which decides an approval.
	if code, body := call(http.MethodDelete, "ws-b5616-business", "", owner("ws-b5616-business")); code != http.StatusNoContent {
		t.Errorf("DELETE = %d %s", code, body)
	}
	var routes []string
	_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, method+" "+route)
		return nil
	})
	sort.Strings(routes)
	if strings.Join(routes, ",") != "DELETE /v1/workspaces/{wsID}/approval-chat,GET /v1/workspaces/{wsID}/approval-chat,PUT /v1/workspaces/{wsID}/approval-chat" {
		t.Errorf("the approval chat routes = %v", routes)
	}
}

// dump is v as JSON, & < > as they were sent.
func dump(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return b.Bytes()
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
