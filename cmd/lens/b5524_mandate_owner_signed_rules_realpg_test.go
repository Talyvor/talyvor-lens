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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// b5524Bank is the store, with the test's agent key named as its agent's.
type b5524Bank struct {
	*economy.DualTokenStore
	ws, agentID string
}

func (b b5524Bank) AgentOfKey(_ context.Context, key string) (string, string, error) {
	if key == "key-b5524-agent" {
		return b.agentID, b.ws, nil
	}
	return "", "", nil
}

// B55.24 — a mandate an agent's own key asks for pulls nothing until the owner approves it, and once the workspace
// has a key the owner's approval and the owner's own grant are refused unsigned; signed, the mandate pulls. Each pull
// is judged by the agent's rules: over its daily limit, or to a payee it may not pay, nothing moves. The owner's signed
// mandate stands in for the approval a pull over the approval amount would need. A declined request pulls nothing.
func TestMandate_NeedsTheOwnersSignedSayAndKeepsTheAgentsRules(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, payeeWS = "ws-b5524-payer", "ws-b5524-payee"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetScreener(screener)
	store.SetPayeeScreener(screener)
	agent, err := store.CreateAgent(ctx, ws, "Subscriber", "owner")
	if err != nil {
		t.Fatal(err)
	}
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "USD", "test")
	if err != nil {
		t.Fatal(err)
	}
	agentUSD, err := store.OpenCurrencyAccount(ctx, ws, agent.ID, "USD", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenCurrencyAccount(ctx, payeeWS, "", "USD", "test"); err != nil {
		t.Fatal(err)
	}
	details, err := store.CurrencyAccountDetails(ctx, ws, "", agentUSD.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReceivePayment(ctx, economy.InboundPayment{PartnerAccountRef: company.PartnerAccountRef, PartnerRef: "test_in_b5524",
		Payer: "Owner Ltd", Reference: details.PaymentReference, AmountMinor: 10000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	mountMandateRoutes(r, b5524Bank{store, ws, agent.ID})
	mountMandatePullRoute(r, store, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			id := &auth.AuthContext{WorkspaceID: payeeWS, AuthMethod: auth.MethodJWT, UserID: "payee", Scopes: []string{auth.ScopeKeys}}
			h.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), id)))
		})
	})
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	agentKey := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "key-b5524-agent"}
	call := func(who *auth.AuthContext, method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if who != nil {
			req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	pulled := func() int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(p.amount_minor), 0) FROM money_postings p JOIN money_entries e ON e.id = p.entry_id
			WHERE e.kind = 'mandate_pull' AND p.account_id = $1`, agentUSD.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return -n
	}
	pulls := 0
	pull := func(id string, cents int) int {
		t.Helper()
		pulls++
		code, _ := call(nil, http.MethodPost, "/v1/money/mandates/"+id+"/pull", fmt.Sprintf(`{"amount_minor":%d,"idempotency_key":"p%d"}`, cents, pulls))
		return code
	}
	grant := `{"account_id":"` + agentUSD.ID + `","payee_workspace_id":"` + payeeWS + `","max_per_pull_minor":3000,"max_per_month_minor":9000,"expires_at":"` +
		time.Now().Add(30*24*time.Hour).UTC().Format(time.RFC3339) + `"}`

	// The agent's own key asks: the mandate is requested, and pulls nothing.
	code, m := call(agentKey, http.MethodPost, "/v1/money/mandates", grant)
	if code != http.StatusCreated || m["status"] != "requested" {
		t.Fatalf("the agent's request = %d %v, want 201 and a requested mandate", code, m)
	}
	id := m["id"].(string)
	if code := pull(id, 500); code != http.StatusConflict || pulled() != 0 {
		t.Fatalf("a pull on a requested mandate = %d, %d cents pulled; want 409 and nothing", code, pulled())
	}
	if code, out := call(owner, http.MethodGet, "/v1/money/mandates?status=requested", ""); code != http.StatusOK ||
		len(out["granted"].([]any)) != 1 || out["granted"].([]any)[0].(map[string]any)["id"] != id {
		t.Fatalf("the requested mandates = %d %v, want the one the agent asked for", code, out)
	}
	if code, _ := call(agentKey, http.MethodPost, "/v1/money/mandates/"+id+"/approve", `{}`); code != http.StatusForbidden {
		t.Fatalf("the agent approving its own request = %d, want 403", code)
	}

	// The workspace now has a key: unsigned, the owner's approval is refused and the mandate still pulls nothing.
	phone, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&phone.PublicKey)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, platform, public_key, attestation)
		VALUES ('dev-b5524', $1, 'test', $2, '\x00')`, ws, spki); err != nil {
		t.Fatal(err)
	}
	signed := func(body string) string {
		t.Helper()
		code, ch := call(owner, http.MethodPost, "/v1/money/mandates/challenge", body)
		challenge, _ := ch["challenge"].(string)
		if code != http.StatusOK || challenge == "" {
			t.Fatalf("the challenge for %s = %d %v, want 200 with a challenge", body, code, ch)
		}
		h := sha256.Sum256(devicekey.Message(challenge))
		sig, _ := ecdsa.SignASN1(rand.Reader, phone, h[:])
		a, _ := json.Marshal(map[string]string{"device_id": "dev-b5524", "challenge": challenge, "signature": base64.RawURLEncoding.EncodeToString(sig)})
		return string(a)
	}
	if code, out := call(owner, http.MethodPost, "/v1/money/mandates/"+id+"/approve", `{}`); code != http.StatusForbidden {
		t.Fatalf("the owner's unsigned approval = %d %v, want 403", code, out)
	}
	if code := pull(id, 500); code != http.StatusConflict || pulled() != 0 {
		t.Fatalf("a pull after the unsigned approval = %d, %d cents pulled; want 409 and nothing", code, pulled())
	}
	if code, out := call(owner, http.MethodPost, "/v1/money/mandates/"+id+"/approve", `{"assertion":`+signed(`{"mandate_id":"`+id+`"}`)+`}`); code != http.StatusOK ||
		out["status"] != "active" {
		t.Fatalf("the owner's signed approval = %d %v, want 200 and an active mandate", code, out)
	}
	if code := pull(id, 500); code != http.StatusCreated || pulled() != 500 {
		t.Fatalf("a pull of $5.00 on the approved mandate = %d, %d cents pulled; want 201 and 500", code, pulled())
	}

	// Each pull keeps the agent's rules, at its LXC value: $5.00 pulled and $8.00 more is over a daily limit of $10.00.
	// Over the approval amount, the owner's signed mandate is the approval: $2.00 more is pulled. (The rules only
	// tighten here: loosening them is signed once the workspace has a key.)
	dollar := 100 * 10_000 * economy.ULXCPerUSDMicro
	rules := economy.AgentRules{DailyLimitULXC: 10 * dollar, ApprovalAboveULXC: dollar}
	if _, err := store.SetAgentRules(ctx, ws, agent.ID, rules); err != nil {
		t.Fatal(err)
	}
	if code := pull(id, 800); code != http.StatusForbidden || pulled() != 500 {
		t.Fatalf("a pull over the agent's daily limit = %d, %d cents pulled; want 403 and still 500", code, pulled())
	}
	if code := pull(id, 200); code != http.StatusCreated || pulled() != 700 {
		t.Fatalf("a $2.00 pull over a $1.00 approval amount = %d, %d cents pulled; want 201 and 700", code, pulled())
	}
	rules.BlockedPayees = []string{payeeWS}
	if _, err := store.SetAgentRules(ctx, ws, agent.ID, rules); err != nil {
		t.Fatal(err)
	}
	if code := pull(id, 100); code != http.StatusForbidden || pulled() != 700 {
		t.Fatalf("a pull to a payee the agent may not pay = %d, %d cents pulled; want 403 and still 700", code, pulled())
	}

	// The owner's own grant is signed too, over exactly that grant.
	if code, out := call(owner, http.MethodPost, "/v1/money/mandates", grant); code != http.StatusForbidden {
		t.Fatalf("the owner's unsigned grant = %d %v, want 403", code, out)
	}
	if code, out := call(owner, http.MethodPost, "/v1/money/mandates", `{"grant":`+grant+`,"assertion":`+signed(`{"grant":`+grant+`}`)+`}`); code != http.StatusCreated ||
		out["status"] != "active" {
		t.Fatalf("the owner's signed grant = %d %v, want 201 and an active mandate", code, out)
	}

	// A request the owner declines pulls nothing.
	_, m = call(agentKey, http.MethodPost, "/v1/money/mandates", grant)
	declined := m["id"].(string)
	if code, out := call(owner, http.MethodPost, "/v1/money/mandates/"+declined+"/decline", ""); code != http.StatusOK || out["status"] != "declined" {
		t.Fatalf("the owner's decline = %d %v, want 200 and a declined mandate", code, out)
	}
	if code := pull(declined, 100); code != http.StatusConflict || pulled() != 700 {
		t.Fatalf("a pull on a declined mandate = %d, %d cents pulled; want 409 and still 700", code, pulled())
	}
}
