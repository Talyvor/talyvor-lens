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
	"github.com/talyvor/lens/internal/tenant"
)

// B50.58 — a locked-down workspace gets no working card by cancelling one and issuing another: the issue is refused
// with 403 before Stripe is asked for a card, no card is recorded, and a purchase on a card Stripe might hold anyway is
// declined and writes no posting. Lifting is B41.116's; this is the gap it left on the issue route.
func TestB5058_LockedDownWorkspaceIssuesNoCard(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5058"
	for _, q := range []string{
		`INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc, test_funded_ulxc) VALUES ('ws-b5058', 1000000000, 1000000000, 1000000000)`,
		`INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ('ws-b5058', 'ws-b5058', 'ws-b5058', true)`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	issuer := &fakeCardIssuer{}
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	mountAgentCardRoutes(r, store, issuer)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-ada", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	base := "/v1/workspaces/" + ws + "/agents"
	code, body := call(http.MethodPost, base, `{"name":"buyer"}`)
	if code != http.StatusCreated {
		t.Fatalf("create agent = %d %s", code, body)
	}
	var agent economy.Agent
	_ = json.Unmarshal([]byte(body), &agent)
	card := base + "/" + agent.ID + "/card"
	if code, body := call(http.MethodPost, base+"/"+agent.ID+"/fund", `{"amount_ulxc":800000000}`); code != http.StatusOK {
		t.Fatalf("fund = %d %s", code, body)
	}
	holder := `{"first_name":"Test","last_name":"Shopper","line1":"1 High Street","city":"London","postal_code":"EC1A 1BB"}`
	if code, body := call(http.MethodPost, card, holder); code != http.StatusCreated {
		t.Fatalf("issue card = %d %s", code, body)
	}

	// The phone is lost: lockdown freezes the card. Cancelling it is still allowed; issuing another is not.
	if _, err := store.LockDown(ctx, ws, owner.UserID, "", "lost my phone", nil, nil); err != nil {
		t.Fatal(err)
	}
	if code, body := call(http.MethodPost, card+"/cancel", ""); code != http.StatusOK {
		t.Fatalf("cancel during lockdown = %d %s, want 200", code, body)
	}
	if code, body := call(http.MethodPost, card, holder); code != http.StatusForbidden || !strings.Contains(body, "locked down") {
		t.Errorf("issue during lockdown = %d %s, want 403 locked down", code, body)
	}
	if issuer.issued != 1 {
		t.Errorf("Stripe was asked for %d cards; want 1, none during the lockdown", issuer.issued)
	}
	var working int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_cards WHERE agent_id = $1 AND cancelled_at IS NULL`, agent.ID).Scan(&working); err != nil {
		t.Fatal(err)
	}
	if working != 0 {
		t.Errorf("after an issue during lockdown the agent holds %d cards; want none", working)
	}
	// Recorded straight to the store, as a request racing the lockdown past the route's check would be.
	if _, err := store.SaveAgentCard(ctx, ws, economy.AgentCard{ID: "ic_b5058_race", AgentID: agent.ID, StripeCardholderID: "ich_test_1",
		Last4: "4242", ExpMonth: 9, ExpYear: 2029, Currency: "gbp"}); err != economy.ErrLockedDown {
		t.Errorf("SaveAgentCard during lockdown = %v; want ErrLockedDown", err)
	}
	d, err := store.AuthorizeAgentCard(ctx, economy.CardAuthorization{EventID: "evt_b5058", AuthorizationID: "iauth_b5058",
		CardID: "ic_b5058_race", AmountMinor: 40, Currency: "gbp", MerchantName: "Shop", At: time.Now(), USDMicros: 500_000})
	if err != nil {
		t.Fatal(err)
	}
	var postings int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE workspace_id = $1 AND kind = 'card'`, ws).Scan(&postings); err != nil {
		t.Fatal(err)
	}
	if d.Approved || postings != 0 {
		t.Errorf("a purchase on a card issued during lockdown = %+v with %d card postings; want declined and none", d, postings)
	}
}
