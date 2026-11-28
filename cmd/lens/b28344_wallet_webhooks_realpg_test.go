package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
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
)

// B28.344 — the owner adds two wallet webhooks: one for every event, one for transfers only. A transfer queues one
// event to each, whose amount and postings are the transfer's rows in agent_postings, however often the queue is read;
// an approval asked for and denied queues two more to the first. Each POST is signed the Standard Webhooks way. An
// endpoint that answers 503 is tried again after the backoff, not before; once it answers 200 the event is delivered
// and never sent again, and every attempt is a row. After a rotation the old and the new secret both sign. Another
// workspace can neither see nor remove the endpoints.
func TestB28344_ATransferQueuesOneSignedEventWhoseAmountIsThePosting(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, other = "ws-b28344", "ws-b28344-other"
	store, ag := b501Agents(t, pool, ws)

	r := chi.NewRouter()
	mountWalletWebhookRoutes(r, store)
	call := func(as, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: as, AuthMethod: auth.MethodJWT,
			UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	add := func(body string, want int) economy.WalletWebhook {
		t.Helper()
		code, out := call(ws, http.MethodPost, "/v1/money/event-webhooks", body)
		var h economy.WalletWebhook
		if code != want || json.Unmarshal([]byte(out), &h) != nil {
			t.Fatalf("add %s = %d %s, want %d", body, code, out, want)
		}
		return h
	}
	add(`{"url": "http://hooks.example.com/talyvor"}`, http.StatusBadRequest)
	add(`{"url": "https://169.254.169.254/latest"}`, http.StatusBadRequest)
	add(`{"url": "https://hooks.example.com/t", "events": ["transfer.made"]}`, http.StatusBadRequest)
	all := add(`{"url": "https://hooks.example.com/all"}`, http.StatusCreated)
	transfers := add(`{"url": "https://hooks.example.com/transfers", "events": ["transfer.created"]}`, http.StatusCreated)
	add(`{"url": "https://hooks.example.com/all"}`, http.StatusBadRequest) // once
	if !strings.HasPrefix(all.Secret, "whsec_") || all.Secret == transfers.Secret {
		t.Fatalf("secrets %q and %q", all.Secret, transfers.Secret)
	}
	if code, out := call(ws, http.MethodGet, "/v1/money/event-webhooks", ""); code != http.StatusOK || strings.Contains(out, all.Secret) ||
		strings.Count(out, `"id"`) != 2 {
		t.Fatalf("webhooks = %d %s, want two, without their secrets", code, out)
	}
	if code, out := call(other, http.MethodGet, "/v1/money/event-webhooks", ""); code != http.StatusOK || strings.Contains(out, all.ID) {
		t.Fatalf("another workspace's list = %d %s", code, out)
	}
	if code, _ := call(other, http.MethodDelete, "/v1/money/event-webhooks/"+all.ID, ""); code != http.StatusNotFound {
		t.Fatalf("another workspace's delete = %d, want 404", code)
	}

	// A transfer, and a payment over the approval line, denied.
	xfer, err := store.SendCredits(ctx, ws, ag[0].ID, ag[1].ID, 1_250_000, "lunch")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, ag[0].ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PayAgent(ctx, ws, ag[0].ID, ag[2].ID, 5_000_000, "invoice A"); !errors.Is(err, economy.ErrApprovalRequired) {
		t.Fatalf("pay over the line: %v, want ErrApprovalRequired", err)
	}
	var approvalID string
	if err := pool.QueryRow(ctx, `SELECT id FROM agent_approvals WHERE workspace_id = $1`, ws).Scan(&approvalID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DecideAgentApproval(ctx, ws, approvalID, false); err != nil {
		t.Fatal(err)
	}

	events := func() []economy.WalletWebhookEvent {
		t.Helper()
		code, out := call(ws, http.MethodGet, "/v1/money/event-webhooks/events", "")
		var got struct{ Events []economy.WalletWebhookEvent }
		if code != http.StatusOK || json.Unmarshal([]byte(out), &got) != nil {
			t.Fatalf("events = %d %s", code, out)
		}
		return got.Events
	}
	events()
	got := events() // read twice: still one of each per endpoint
	per := map[string][]string{}
	var transferData []json.RawMessage
	for _, e := range got {
		per[e.WebhookID] = append(per[e.WebhookID], e.Type)
		if e.Type == economy.WalletEventTransfer {
			transferData = append(transferData, e.Data)
			if e.Ref != xfer.ID || e.Status != "queued" {
				t.Errorf("transfer event %+v", e)
			}
		}
	}
	if len(per[all.ID]) != 3 || len(per[transfers.ID]) != 1 || per[transfers.ID][0] != economy.WalletEventTransfer {
		t.Fatalf("events per endpoint = %v, want transfer, approval asked and decided on the first, the transfer alone on the second", per)
	}

	// The event's amount and postings are the ledger's: the transfer's agent_postings rows, to the µLXC.
	type posting struct {
		Account    string `json:"account"`
		AmountULXC int64  `json:"amount_ulxc"`
	}
	var ledger []posting
	rows, err := pool.Query(ctx, `SELECT account, amount_ulxc FROM agent_postings WHERE ref = $1 AND kind = 'transfer' ORDER BY id`, xfer.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p posting
		if err := rows.Scan(&p.Account, &p.AmountULXC); err != nil {
			t.Fatal(err)
		}
		ledger = append(ledger, p)
	}
	rows.Close()
	if len(ledger) != 2 || ledger[0].AmountULXC+ledger[1].AmountULXC != 0 {
		t.Fatalf("ledger postings %+v", ledger)
	}
	for _, raw := range transferData {
		var d struct {
			AmountULXC int64     `json:"amount_ulxc"`
			Currency   string    `json:"currency"`
			EntryID    string    `json:"entry_id"`
			Direction  string    `json:"direction"`
			Postings   []posting `json:"postings"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		if d.AmountULXC != ledger[1].AmountULXC || d.Currency != "LXC" || d.EntryID != xfer.EntryID || d.Direction != "internal" ||
			len(d.Postings) != 2 || d.Postings[0] != ledger[0] || d.Postings[1] != ledger[1] {
			t.Fatalf("transfer event data %s, want the amount and postings of %+v", raw, ledger)
		}
	}

	// Sent to a server of the owner's that answers 503 to each event's first POST and 200 after.
	type post struct {
		path string
		hdr  http.Header
		body []byte
	}
	var mu sync.Mutex
	var posts []post
	seen, failFirst := map[string]bool{}, true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		mu.Lock()
		defer mu.Unlock()
		posts = append(posts, post{req.URL.Path, req.Header.Clone(), body})
		if id := req.Header.Get("webhook-id"); failFirst && !seen[id] {
			seen[id] = true
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	if _, err := pool.Exec(ctx, `UPDATE wallet_webhooks SET url = $2 || '/' || split_part(url, '/', 4) WHERE workspace_id = $1`, ws, srv.URL); err != nil {
		t.Fatal(err)
	}
	send := func(at time.Time) int {
		t.Helper()
		n, err := store.SendWalletWebhookEvents(ctx, ws, srv.Client(), at)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	now := time.Now()
	if n := send(now); n != 0 || len(posts) != 4 {
		t.Fatalf("first send delivered %d with %d POSTs, want none of four", n, len(posts))
	}
	if n := send(now.Add(30 * time.Second)); n != 0 || len(posts) != 4 {
		t.Fatalf("before the backoff: delivered %d, %d POSTs; want nothing sent", n, len(posts))
	}
	if n := send(now.Add(2 * time.Minute)); n != 4 || len(posts) != 8 {
		t.Fatalf("after the backoff: delivered %d, %d POSTs; want all four", n, len(posts))
	}
	if n := send(now.Add(3 * time.Hour)); n != 0 || len(posts) != 8 {
		t.Fatalf("once delivered: %d more, %d POSTs; want none ever sent again", n, len(posts))
	}
	var attempts, delivered int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM wallet_webhook_attempts WHERE workspace_id = $1),
		(SELECT count(*) FROM wallet_webhook_events WHERE workspace_id = $1 AND status = 'delivered')`, ws).Scan(&attempts, &delivered); err != nil {
		t.Fatal(err)
	}
	if attempts != 8 || delivered != 4 {
		t.Fatalf("%d attempt rows, %d delivered; want 8 and 4", attempts, delivered)
	}

	// Standard Webhooks: v1,base64(HMAC-SHA256(base64-decoded secret, id.timestamp.body)).
	sign := func(secret string, p post) string {
		key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(p.hdr.Get("webhook-id") + "." + p.hdr.Get("webhook-timestamp") + "."))
		mac.Write(p.body)
		return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}
	for _, p := range posts {
		secret := all.Secret
		if p.path == "/transfers" {
			secret = transfers.Secret
		}
		if !strings.HasPrefix(p.hdr.Get("webhook-id"), "msg_") || p.hdr.Get("webhook-signature") != sign(secret, p) {
			t.Fatalf("POST to %s: id %q, signature %q; not signed with its endpoint's secret", p.path, p.hdr.Get("webhook-id"),
				p.hdr.Get("webhook-signature"))
		}
	}

	// Rotated: the next event is signed by the new secret and, for a day, the old one.
	code, out := call(ws, http.MethodPost, "/v1/money/event-webhooks/"+transfers.ID+"/rotate", "")
	var rotated economy.WalletWebhook
	if code != http.StatusOK || json.Unmarshal([]byte(out), &rotated) != nil || rotated.Secret == transfers.Secret ||
		!strings.HasPrefix(rotated.Secret, "whsec_") {
		t.Fatalf("rotate = %d %s", code, out)
	}
	if _, err := store.SendCredits(ctx, ws, ag[0].ID, ag[1].ID, 400_000, "coffee"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.QueueWalletWebhookEvents(ctx, ws); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	failFirst, posts = false, nil // answer 200 at once
	mu.Unlock()
	if n := send(time.Now()); n != 2 {
		t.Fatalf("after the rotation delivered %d, want the coffee transfer to both endpoints", n)
	}
	for _, p := range posts {
		if p.path != "/transfers" {
			continue
		}
		if p.hdr.Get("webhook-signature") != sign(rotated.Secret, p)+" "+sign(transfers.Secret, p) {
			t.Fatalf("after the rotation the signature is %q, want the new and the old", p.hdr.Get("webhook-signature"))
		}
	}

	if code, _ := call(ws, http.MethodDelete, "/v1/money/event-webhooks/"+transfers.ID, ""); code != http.StatusOK {
		t.Fatalf("delete = %d", code)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM wallet_webhook_events WHERE webhook_id = $1`, transfers.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d events left of a removed endpoint, %v", left, err)
	}
}
