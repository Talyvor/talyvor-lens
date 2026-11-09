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
	"github.com/talyvor/lens/internal/passkey"
)

// B54.5 — a passkey revoked in a lockdown signs no negotiation mandate: the mandate the lost phone signed before the
// lockdown admits no further offer, a mandate it signs after is not kept, and prepare no longer names it as a signer.
func TestB545_ARevokedPasskeySignsNoNegotiationMandate(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b545", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	r := chi.NewRouter()
	mountNegotiationRoutes(r, store)
	who := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string, out any) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil && w.Code < 300 {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatalf("%s %s: %v in %s", method, path, err, w.Body.String())
			}
		}
		return w.Code, w.Body.String()
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	sign := b5018Passkey(t, store, ws, rpID, origin) // the phone that is lost
	keys, err := store.ListPasskeys(ctx, ws)
	if err != nil || len(keys) != 1 {
		t.Fatalf("passkeys = %v %v", keys, err)
	}
	lost := keys[0].CredentialID

	terms := func(deal string) string {
		return `{"task":{"kind":"renewal","what":"Acme CRM, 40 seats"},"side":"buy","deal":"` + deal + `","currency":"GBP",
			"target_minor":1000000,"walk_away_minor":1200000,"deadline":"` + time.Now().Add(48*time.Hour).Format(time.RFC3339) + `",
			"counterparties":["acme.example"],"channels":["email"],"authority":"negotiate"}`
	}
	type prepared struct {
		Canonical, Challenge string
		CredentialIDs        []string `json:"allow_credentials"`
	}
	prepare := func(deal string) (prepared, int, string) {
		t.Helper()
		var out prepared
		code, msg := call(http.MethodPost, "/v1/negotiations/prepare", terms(deal), &out)
		return out, code, msg
	}
	keep := func(canonical string, a *passkey.Assertion, out any) (int, string) {
		b, _ := json.Marshal(map[string]any{"canonical": canonical, "assertion": a})
		return call(http.MethodPost, "/v1/negotiations", string(b), out)
	}
	offer := func(id string) (int, string) {
		b, _ := json.Marshal(map[string]any{"counterparty": "acme.example", "party": "ours", "channel": "email",
			"terms": map[string]any{"price_minor": 1100000, "term_months": 12}, "message": "We can renew 40 seats at £11,000 for 12 months."})
		return call(http.MethodPost, "/v1/negotiations/"+id+"/terms", string(b), nil)
	}

	// Before the lockdown, the phone's passkey signs a mandate and Counter makes an offer under it.
	p, code, msg := prepare("rfq-b545-1")
	if code != http.StatusOK || len(p.CredentialIDs) != 1 || p.CredentialIDs[0] != lost {
		t.Fatalf("prepare = %d %s %+v", code, msg, p)
	}
	var m economy.NegotiationMandate
	if code, msg := keep(p.Canonical, sign(p.Challenge), &m); code != http.StatusCreated {
		t.Fatalf("keep the signed mandate = %d %s", code, msg)
	}
	if code, msg := offer(m.ID); code != http.StatusCreated {
		t.Fatalf("an offer under the live mandate = %d %s", code, msg)
	}
	next, code, msg := prepare("rfq-b545-2") // prepared before the lockdown, signed after it
	if code != http.StatusOK {
		t.Fatalf("prepare = %d %s", code, msg)
	}

	if _, err := store.LockDown(ctx, ws, who.UserID, "", "phone lost", nil, []string{lost}); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM workspace_passkeys WHERE credential_id = $1 AND revoked_at IS NOT NULL`, lost); n != 1 {
		t.Fatalf("the lockdown left the phone's passkey live")
	}

	// The mandate the lost phone signed admits no offer, and the offer refused is not on the term sheet.
	if code, msg := offer(m.ID); code != http.StatusBadRequest || !strings.Contains(msg, "allows nothing") {
		t.Errorf("an offer under a mandate signed by a revoked passkey = %d %s, want 400: it allows nothing", code, msg)
	}
	if n := count(`SELECT count(*) FROM negotiation_terms WHERE mandate_id = $1 AND party = 'ours'`, m.ID); n != 1 {
		t.Errorf("%d outbound offers under the revoked passkey's mandate, want still the 1 made before the lockdown", n)
	}

	// It signs no new mandate, and prepare names no revoked passkey as one that may sign.
	if code, msg := keep(next.Canonical, sign(next.Challenge), nil); code != http.StatusBadRequest {
		t.Errorf("a mandate signed by a revoked passkey = %d %s, want 400", code, msg)
	}
	if n := count(`SELECT count(*) FROM negotiation_mandates WHERE workspace_id = $1`, ws); n != 1 {
		t.Errorf("%d mandates kept, want the 1 signed before the lockdown", n)
	}
	if p, code, _ := prepare("rfq-b545-3"); code == http.StatusOK && len(p.CredentialIDs) != 0 {
		t.Errorf("prepare names %v as passkeys that may sign, want none: the only one is revoked", p.CredentialIDs)
	}
}
