package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
)

// B28.338 — the owner adds push, a webhook and Slack as alert channels; email is refused, not faked. An agent crossing
// half of its daily limit raises one alert, which queues exactly one delivery per channel — once, however often the
// queue is read — and each is sent: a push to the owner's phone, a webhook POST signed with the channel's secret, and a
// Slack message.
func TestB28338_ATriggeredAlertQueuesOneDeliveryPerChannel(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b28338"
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			return sandboxMoneyLayer(t, pool, store, reg)
		})

	r := chi.NewRouter()
	mountAlertChannelRoutes(r, b.store)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT,
			UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	add := func(body string, want int) economy.AlertChannel {
		t.Helper()
		w := call(http.MethodPost, "/v1/money/alerts/channels", body)
		var c economy.AlertChannel
		if w.Code != want || json.Unmarshal(w.Body.Bytes(), &c) != nil {
			t.Fatalf("add %s = %d %s, want %d", body, w.Code, w.Body.String(), want)
		}
		return c
	}
	add(`{"kind": "email", "url": "owner@example.com"}`, http.StatusBadRequest)
	add(`{"kind": "webhook", "url": "http://169.254.169.254/latest"}`, http.StatusBadRequest)
	add(`{"kind": "slack", "url": "https://example.com/not-slack"}`, http.StatusBadRequest)
	push := add(`{"kind": "push"}`, http.StatusCreated)
	hook := add(`{"kind": "webhook", "url": "https://hooks.example.com/talyvor"}`, http.StatusCreated)
	slack := add(`{"kind": "slack", "url": "https://hooks.slack.com/services/T000/B000/abcdwxyz"}`, http.StatusCreated)
	add(`{"kind": "push"}`, http.StatusBadRequest) // once
	if !strings.HasPrefix(hook.Secret, "whsec_") || slack.URL != "https://hooks.slack.com/…wxyz" {
		t.Fatalf("webhook secret %q, Slack url shown as %q", hook.Secret, slack.URL)
	}
	w := call(http.MethodGet, "/v1/money/alerts/channels", "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), hook.Secret) || strings.Contains(w.Body.String(), "abcdwxyz") ||
		strings.Count(w.Body.String(), `"id"`) != 3 {
		t.Fatalf("channels = %d %s, want three, without the webhook's secret or the Slack hook", w.Code, w.Body.String())
	}

	// The agent pays out, then its daily limit is set so that payment is 60% of it: one alert, at 50%.
	p := b.pay(t, "inv-1", 400)
	if _, err := b.store.SetAgentRules(ctx, ws, p.AgentID, economy.AgentRules{DailyLimitULXC: p.ValueULXC * 100 / 60}); err != nil {
		t.Fatal(err)
	}
	alerts, err := b.store.CheckSpendAlerts(ctx, ws, time.Now())
	if err != nil || len(alerts) != 1 || alerts[0].Percent != 50 {
		t.Fatalf("alerts raised = %+v %v, want one at 50%%", alerts, err)
	}

	deliveries := func() []economy.AlertDelivery {
		t.Helper()
		w := call(http.MethodGet, "/v1/money/alerts/deliveries", "")
		var out struct{ Deliveries []economy.AlertDelivery }
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("deliveries = %d %s", w.Code, w.Body.String())
		}
		return out.Deliveries
	}
	deliveries()
	got := deliveries() // read twice: still one per channel
	perChannel := map[string]int{}
	for _, d := range got {
		perChannel[d.ChannelID]++
		if d.AlertKind != economy.AlertKindLimit || d.AlertID != alerts[0].ID || d.Status != "queued" || d.Title != "Agent limit" ||
			!strings.Contains(d.Body, "50% of its daily limit") {
			t.Errorf("delivery %+v, want the 50%% agent-limit alert, queued", d)
		}
	}
	if len(got) != 3 || perChannel[push.ID] != 1 || perChannel[hook.ID] != 1 || perChannel[slack.ID] != 1 {
		t.Fatalf("deliveries per channel = %v (%d rows), want one on each of the three", perChannel, len(got))
	}

	// Sent: the webhook and Slack reach a server of the owner's, the push the owner's phone.
	posts := make(chan *http.Request, 2)
	bodies := make(chan []byte, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		posts <- req
		bodies <- body
	}))
	defer srv.Close()
	if _, err := pool.Exec(ctx, `UPDATE alert_channels SET url = $2 || '/' || kind WHERE workspace_id = $1 AND kind <> 'push'`, ws, srv.URL); err != nil {
		t.Fatal(err)
	}
	sent, err := b.store.SendAlertDeliveries(ctx, ws, srv.Client(), time.Now())
	if err != nil || sent != 3 {
		t.Fatalf("sent %d, %v; want all three", sent, err)
	}
	for range 2 {
		req, body := <-posts, <-bodies
		switch req.URL.Path {
		case "/webhook":
			ts := req.Header.Get("X-Talyvor-Timestamp")
			if req.Header.Get("X-Talyvor-Signature") != "v1="+economy.AlertSignature(hook.Secret, ts, body) ||
				!strings.Contains(string(body), alerts[0].ID) {
				t.Errorf("webhook POST %s, signature %q: not signed with the channel's secret", body, req.Header.Get("X-Talyvor-Signature"))
			}
		case "/slack":
			if !strings.Contains(string(body), `"text":"*Agent limit*\n`) {
				t.Errorf("Slack POST %s", body)
			}
		default:
			t.Errorf("POST to %s", req.URL.Path)
		}
	}
	for deadline := time.After(5 * time.Second); ; {
		select {
		case m := <-b.pushes:
			if m.Data["type"] != "alert" {
				continue // the payment's own push
			}
			if m.Title != "Agent limit" || m.Data["alert_id"] != alerts[0].ID {
				t.Errorf("push %+v", m)
			}
		case <-deadline:
			t.Fatal("the alert was not pushed to the owner's phone")
		}
		break
	}
	for _, d := range deliveries() {
		if d.Status != "sent" || d.Attempts != 1 || d.SentAt == nil {
			t.Errorf("delivery %+v, want sent at the first attempt", d)
		}
	}
}
