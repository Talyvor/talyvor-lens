package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/passkey"
)

// B52.16 — through the routes: an owner finds a creator's listed playbook, runs it on a signed negotiation, and from
// then on Counter plans with the playbook's rounds and words its counter in the playbook's words, still short of the
// walk-away; once the playbook is taken down, it words nothing more.
func TestB5216_CounterPlaybookThroughTheRoutes(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, creator, rpID, origin = "ws-b5216-owner", "ws-b5216-creator", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	mstore := market.NewStore(pool)
	pb, _ := json.Marshal(map[string]any{"deal_type": "renewal", "rounds": 2, "open_pct": 30, "limit_pct": 60, "wording": map[string]string{
		"ask": "Hi. Your best price for {what}, please.", "counter": "Thanks. For {what} we are at {offer}.",
		"walk_away": "Thanks for your time on {what}. We will leave it there."}})
	l, err := mstore.Publish(ctx, creator, market.Draft{Kind: market.KindCounterPlaybook, Title: "Two rounds", Visibility: "public", Artifact: pb})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountNegotiationRoutes(r, store)
	mountNegotiationMoveRoutes(r, store, nil, "")
	mountNegotiationPlaybookRoutes(r, mstore, nil, store)
	who := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string, out any) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil && w.Code < 300 {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("%s: %v in %s", path, err, w.Body.String())
			}
		}
		return w.Code, w.Body.String()
	}
	var listed struct{ Playbooks []market.CounterPlaybook }
	if code, msg := call(http.MethodGet, "/v1/negotiations/playbooks?deal_type=renewal", "", &listed); code != http.StatusOK ||
		len(listed.Playbooks) != 1 || listed.Playbooks[0].ID != l.ID || listed.Playbooks[0].Gym.Deals == 0 {
		t.Fatalf("playbooks = %d %s", code, msg)
	}

	sign := b5018Passkey(t, store, ws, rpID, origin)
	var prep struct{ Canonical, Challenge string }
	if code, msg := call(http.MethodPost, "/v1/negotiations/prepare", `{"task":{"kind":"renewal","what":"Acme CRM, 40 seats"},"side":"buy",
		"currency":"GBP","target_minor":1000000,"walk_away_minor":1200000,"must_haves":["SSO"],"deadline":"`+
		time.Now().Add(48*time.Hour).Format(time.RFC3339)+`","counterparties":["acme.example"],"channels":["email"],
		"authority":"negotiate"}`, &prep); code != http.StatusOK {
		t.Fatalf("prepare = %d %s", code, msg)
	}
	b, _ := json.Marshal(map[string]any{"canonical": prep.Canonical, "assertion": sign(prep.Challenge)})
	var m economy.NegotiationMandate
	if code, msg := call(http.MethodPost, "/v1/negotiations", string(b), &m); code != http.StatusCreated {
		t.Fatalf("keep = %d %s", code, msg)
	}
	base := "/v1/negotiations/" + m.ID
	var run market.CounterRun
	if code, msg := call(http.MethodPost, base+"/playbook", `{"listing_id":"`+l.ID+`"}`, &run); code != http.StatusOK || run.UseID == "" || run.Charge != market.ChargeFree {
		t.Fatalf("run = %d %s", code, msg)
	}
	// target 10,000.00 and walk-away 12,000.00: the playbook opens 30% of the gap under the target and goes to 60% of it.
	var plan economy.NegotiationPlan
	if code, msg := call(http.MethodPost, base+"/plan", `{}`, &plan); code != http.StatusOK || plan.OpeningMinor != 940000 ||
		len(plan.Schedule) != 2 || plan.Schedule[1] != 1120000 {
		t.Fatalf("plan = %d %s", code, msg)
	}
	if code, msg := call(http.MethodPost, base+"/terms", `{"counterparty":"acme.example","party":"theirs","channel":"email",
		"terms":{"price_minor":1300000},"message":"13,000.00 GBP with SSO"}`, nil); code != http.StatusCreated {
		t.Fatalf("terms = %d %s", code, msg)
	}
	var move economy.NegotiationMove
	if code, msg := call(http.MethodPost, base+"/move", `{"counterparty":"acme.example","channel":"email"}`, &move); code != http.StatusOK ||
		move.Action != economy.MoveCounter || move.PriceMinor != 940000 || move.WrittenBy != "playbook" ||
		move.Message != "Thanks. For Acme CRM, 40 seats we are at "+economy.FormatMinor(940000, "GBP")+"." {
		t.Fatalf("move = %d %s", code, msg)
	}
	// Taken down since, it words nothing more: the negotiation goes on under the engine's own settings.
	if _, err := pool.Exec(ctx, `UPDATE market_listings SET review_status = 'taken_down' WHERE id = $1`, l.ID); err != nil {
		t.Fatal(err)
	}
	if code, msg := call(http.MethodPost, base+"/plan", `{}`, &plan); code != http.StatusOK || len(plan.Schedule) != 4 {
		t.Fatalf("plan after the takedown = %d %s", code, msg)
	}
}
