package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/issuerfake"
	"github.com/talyvor/lens/internal/partners"
)

// B53.19 — the owner's physical card on the fake issuer. Switched off, every route is 404 and nothing is written.
// Switched on: an order to a bad postcode is 422; a good one is 201, inactive and ordered, with no card number; it
// cannot be activated before it ships (409) nor with the wrong last four digits (422); shipped, it activates at the
// issuer; replaced as lost, the old card is frozen at the issuer and a new inactive one is ordered to the same address.
func TestB5319_ThePhysicalCardIsOrderedShippedActivatedAndReplacedOnlyWhileSwitchedOn(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, syntheticKey = "ws-b5319", "b5319-synthetic-key"
	var fake *issuerfake.Issuer
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			fake = issuerfake.New(store.AuthorizeCardCharge)
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b5319"), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
				Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool), Issuer: fake.Adapter()})
			if err != nil {
				t.Fatal(err)
			}
			return layer
		})
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, true)`, ws); err != nil {
		t.Fatal(err)
	}
	router := func(on bool) chi.Router {
		r := chi.NewRouter()
		mountPhysicalCardRoutes(r, b.store, on)
		due := syntheticDueDeps{db: pool}
		if on {
			due.physicalCards = b.store
		}
		mountSyntheticDueRoutes(r, syntheticKey, syntheticDeps{audit: pool, due: due})
		return r
	}
	call := func(r chi.Router, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if strings.HasPrefix(path, "/v1/synthetic/") {
			req.Header.Set(syntheticKeyHeader, syntheticKey)
		} else {
			req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT,
				UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	rows := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_physical_cards WHERE workspace_id = $1`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	const order = `{"name_on_card": "Ada Lovelace", "address": {"line1": "1 High Street", "city": "London", "postcode": "sw1a 1aa", "country": "GB"}}`

	off := router(false)
	for _, c := range []struct{ method, path, body string }{{http.MethodGet, "/v1/money/physical-cards", ""}, {http.MethodPost, "/v1/money/physical-cards", order},
		{http.MethodPost, "/v1/money/physical-cards/pcard_x/activate", `{"last4": "0001"}`},
		{http.MethodPost, "/v1/money/physical-cards/pcard_x/replace", `{"reason": "lost"}`},
		{http.MethodPost, "/v1/synthetic/workspaces/" + ws + "/money/physical-cards/pcard_x/delivery", `{"status": "shipped"}`}} {
		if code, body := call(off, c.method, c.path, c.body); code != http.StatusNotFound {
			t.Fatalf("switched off, %s %s = %d %s, want 404", c.method, c.path, code, body)
		}
	}
	if n := rows(); n != 0 {
		t.Fatalf("switched off, %d physical cards were written, want none", n)
	}

	on := router(true)
	if code, body := call(on, http.MethodPost, "/v1/money/physical-cards", strings.Replace(order, "sw1a 1aa", "NOT A CODE", 1)); code != http.StatusUnprocessableEntity || rows() != 0 {
		t.Fatalf("an order to a bad postcode = %d %s with %d rows, want 422 and none", code, body, rows())
	}
	code, body := call(on, http.MethodPost, "/v1/money/physical-cards", order)
	var card economy.PhysicalCard
	if err := json.Unmarshal([]byte(body), &card); code != http.StatusCreated || err != nil || card.State != "inactive" || card.Design != "standard" ||
		card.Delivery.Status != "ordered" || len(card.Last4) != 4 || card.Address.Postcode != "SW1A 1AA" || card.NameOnCard != "Ada Lovelace" {
		t.Fatalf("an order = %d %s, want 201 an inactive standard card, ordered to SW1A 1AA", code, body)
	}
	if strings.Contains(body, "number") || strings.Contains(body, "pan") {
		t.Fatalf("the card answered %s: a card number never leaves Lens", body)
	}
	var ref string
	if err := pool.QueryRow(ctx, `SELECT partner_ref FROM money_physical_cards WHERE id = $1 AND partner = $2`, card.ID, issuerfake.Name).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if c, ok := fake.Card(ref); !ok || c.FormFactor != money.CardPhysical || c.State != money.CardInactive {
		t.Fatalf("the issuer holds %+v (%v), want a physical inactive card", c, ok)
	}

	base := "/v1/money/physical-cards/" + card.ID
	if code, body := call(on, http.MethodPost, base+"/activate", `{"last4": "`+card.Last4+`"}`); code != http.StatusConflict {
		t.Fatalf("activating before it shipped = %d %s, want 409", code, body)
	}
	if code, body := call(on, http.MethodPost, "/v1/synthetic/workspaces/"+ws+"/money/physical-cards/"+card.ID+"/delivery", `{"status": "shipped"}`); code != http.StatusOK ||
		!strings.Contains(body, `"status":"shipped"`) {
		t.Fatalf("the shipping event = %d %s, want 200 shipped", code, body)
	}
	if code, body := call(on, http.MethodPost, base+"/activate", `{"last4": "9999"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("activating with the wrong last four = %d %s, want 422", code, body)
	}
	if code, body := call(on, http.MethodPost, base+"/activate", `{"last4": "`+card.Last4+`"}`); code != http.StatusOK || !strings.Contains(body, `"state":"active"`) {
		t.Fatalf("activating = %d %s, want 200 active", code, body)
	}
	if c, _ := fake.Card(ref); c.State != money.CardActive {
		t.Fatalf("the issuer holds the card %s, want active", c.State)
	}

	code, body = call(on, http.MethodPost, base+"/replace", `{"reason": "lost"}`)
	var replaced struct{ Old, Card economy.PhysicalCard }
	if err := json.Unmarshal([]byte(body), &replaced); code != http.StatusCreated || err != nil || replaced.Old.State != "frozen" ||
		replaced.Old.ReplacedBy != replaced.Card.ID || replaced.Card.Replaces != card.ID || replaced.Card.State != "inactive" ||
		replaced.Card.Delivery.Status != "ordered" || replaced.Card.Address != card.Address || replaced.Card.ID == card.ID {
		t.Fatalf("replacing a lost card = %d %s, want 201 the old frozen and a new inactive one to the same address", code, body)
	}
	if c, _ := fake.Card(ref); c.State != money.CardFrozen {
		t.Fatalf("the issuer holds the lost card %s, want frozen", c.State)
	}
	if code, body := call(on, http.MethodPost, base+"/replace", `{"reason": "lost"}`); code != http.StatusConflict {
		t.Fatalf("replacing it again = %d %s, want 409", code, body)
	}
	if code, body := call(on, http.MethodGet, "/v1/money/physical-cards", ""); code != http.StatusOK || strings.Count(body, `"design":"standard"`) != 2 {
		t.Fatalf("the list = %d %s, want both cards", code, body)
	}
}
