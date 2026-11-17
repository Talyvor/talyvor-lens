package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/tenant"
)

// b621World is an agent with a €500.00 euro account whose rules ask the owner above €100.00, an agent of the same
// workspace and one of another workspace to pay, and the owner's phone key that signs approvals.
type b621World struct {
	pool              *pgxpool.Pool
	b                 b468Payer
	ws, other         string
	payer, payerAcct  string
	same, far         string
	sameAcct, farAcct string
	phone             *ecdsa.PrivateKey
	router            chi.Router
	owner, otherOwner *auth.AuthContext
	approvalOf        map[string]string // idempotency key → the approval its payment filed
	paymentOf         map[string]string // idempotency key → the money entry it made
	lastCode          int
	lastBody          map[string]any
}

func newB621World(t *testing.T, ws string) *b621World {
	t.Helper()
	pool := agentRoutesDB(t)
	ctx := context.Background()
	w := &b621World{pool: pool, ws: ws, other: ws + "-other", approvalOf: map[string]string{}, paymentOf: map[string]string{}}
	w.b = newB468Payer(t, pool, ws, "EUR", economy.OutsidePayee{Name: "Grace Hopper", Country: "DE", IBAN: "DE89370400440532013000", BIC: "COBADEFFXXX"},
		func(store *economy.DualTokenStore, r *partners.Registry) *money.Registry {
			return sandboxMoneyLayer(t, pool, store, r)
		})
	w.payer, w.payerAcct = w.b.acct.AgentID, w.b.acct.ID
	if _, err := w.b.store.SetAgentRules(ctx, ws, w.payer, economy.AgentRules{Currency: "EUR", MaxPerPaymentMinor: 30_000, ApprovalAboveMinor: 10_000}); err != nil {
		t.Fatal(err)
	}
	agent := func(in, name string) (string, string) {
		t.Helper()
		a, err := w.b.store.CreateAgentOfKind(ctx, in, name, "owner-"+in, "", "EUR", economy.AgentLook{})
		if err != nil {
			t.Fatal(err)
		}
		var acct string
		_ = pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE agent_id = $1 AND purpose = 'agent' AND currency = 'EUR'`, a.ID).Scan(&acct)
		if acct == "" {
			c, err := w.b.store.OpenCurrencyAccount(ctx, in, a.ID, "EUR", "test")
			if err != nil {
				t.Fatal(err)
			}
			acct = c.ID
		}
		return a.ID, acct
	}
	w.same, w.sameAcct = agent(ws, "Ledger")
	w.far, w.farAcct = agent(w.other, "Supplier")
	w.phone, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&w.phone.PublicKey)
	if _, err := pool.Exec(ctx, `UPDATE workspace_device_keys SET public_key = $2 WHERE id = $1`, "dev_"+ws, spki); err != nil {
		t.Fatal(err)
	}
	w.router = chi.NewRouter()
	mountAgentAccountRoutes(w.router, w.b.store, tenant.NewStore(pool))
	mountAgentApproverRoutes(w.router, w.b.store, nil)
	w.owner = &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	w.otherOwner = &auth.AuthContext{WorkspaceID: w.other, AuthMethod: auth.MethodJWT, UserID: "owner-other", Scopes: []string{auth.ScopeKeys}}
	return w
}

// pay has the agent pay to minor euros under key, memo "Invoice <key>", through the real route.
func (w *b621World) pay(t *testing.T, key, to string, minor int64) int {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"to_agent_id": to, "amount_minor": minor, "currency": "EUR", "memo": "Invoice " + key})
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+w.ws+"/agents/"+w.payer+"/pay", strings.NewReader(string(body)))
	req.Header.Set("Idempotency-Key", key)
	req = req.WithContext(auth.WithAuthContext(req.Context(), w.owner))
	rec := httptest.NewRecorder()
	w.router.ServeHTTP(rec, req)
	w.lastCode, w.lastBody = rec.Code, map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &w.lastBody)
	if id, ok := w.lastBody["approval_id"].(string); ok {
		w.approvalOf[key] = id
	}
	if id, ok := w.lastBody["id"].(string); ok && rec.Code == http.StatusOK {
		w.paymentOf[key] = id
	}
	return rec.Code
}

// fingerprint is what PayAgentMoney binds an approval to for the payment pay(key, to, minor) makes.
func (w *b621World) fingerprint(key, to string, minor int64) string {
	fp := sha256.Sum256([]byte("agent_money_pay\x00" + w.payer + "\x00" + w.payerAcct + "\x00" + to + "\x00" +
		strconv.FormatInt(minor, 10) + "\x00EUR\x00" + economy.FundingTest + "\x00Invoice " + key))
	return hex.EncodeToString(fp[:])
}

// approveSigned approves approvalID, signed with the owner's phone key over the challenge Lens issues for it.
func (w *b621World) approveSigned(t *testing.T, approvalID string) {
	t.Helper()
	ctx := context.Background()
	challenge, _, err := w.b.store.ApprovalChallenge(ctx, w.ws, approvalID, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(devicekey.Message(challenge))
	sig, _ := ecdsa.SignASN1(rand.Reader, w.phone, h[:])
	if _, err := w.b.store.DecideAgentApprovalSigned(ctx, w.ws, approvalID, true, nil, passkey.Assertion{DeviceID: "dev_" + w.ws,
		Challenge: challenge, Signature: base64.RawURLEncoding.EncodeToString(sig)}); err != nil {
		t.Fatal(err)
	}
}

func (w *b621World) balances(t *testing.T, payer, same, far int64) {
	t.Helper()
	ctx := context.Background()
	for i, a := range []struct {
		ws, id string
		want   int64
	}{{w.ws, w.payerAcct, payer}, {w.ws, w.sameAcct, same}, {w.other, w.farAcct, far}} {
		got, err := w.b.store.Balance(ctx, a.ws, a.id)
		if err != nil || got.TestMinor != a.want {
			t.Fatalf("account %d holds %+v (%v), want %d", i, got, err, a.want)
		}
	}
}

// B62.1, red first — an approval the owner signed for €150.00 to Ledger is used only by that payment. Every route today
// binds an approval to its payment by a fingerprint, so to show what stops a changed payment the test points the
// approval's fingerprint at another payment: one to another payee, then one of a larger amount. On main the approval
// goes out on either, since nothing but the fingerprint is compared; now each is stopped, nothing moves, the approval
// is still the owner's to use, and the owner is told in words. The payment approved still goes out once.
func TestB621_AnApprovedPaymentChangedBeforeItIsSentIsStopped(t *testing.T) {
	w := newB621World(t, "ws-b621-changed")
	ctx := context.Background()
	if code := w.pay(t, "k-approved", w.same, 15_000); code != http.StatusAccepted {
		t.Fatalf("pay €150.00: %d %v, want 202 for the owner's approval", code, w.lastBody)
	}
	approval := w.approvalOf["k-approved"]
	w.approveSigned(t, approval)
	w.balances(t, 50_000, 0, 0)

	rebind := func(fp string) {
		t.Helper()
		if _, err := w.pool.Exec(ctx, `UPDATE agent_approvals SET fingerprint = $2 WHERE id = $1`, approval, fp); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name, key, to string
		minor         int64
		sent          string
	}{
		{"another payee", "k-payee", w.far, 15_000, "€150.00 to Supplier"},
		{"a larger amount", "k-more", w.same, 19_000, "€190.00 to Ledger"},
	} {
		rebind(w.fingerprint(c.key, c.to, c.minor))
		code := w.pay(t, c.key, c.to, c.minor)
		body, _ := json.Marshal(w.lastBody)
		if code != http.StatusForbidden || !strings.Contains(string(body), "not the one you approved") ||
			!strings.Contains(string(body), "You approved €150.00 to Ledger, and it would have sent "+c.sent) {
			t.Fatalf("%s, approved for €150.00 to Ledger: %d %s, want 403 saying it is not the payment approved", c.name, code, body)
		}
		w.balances(t, 50_000, 0, 0)
		var status string
		if err := w.pool.QueryRow(ctx, `SELECT status FROM agent_approvals WHERE id = $1`, approval).Scan(&status); err != nil || status != "approved" {
			t.Fatalf("%s: the approval is %q (%v), want still approved for its own payment", c.name, status, err)
		}
		told := false
		for deadline := time.After(5 * time.Second); !told; {
			select {
			case m := <-w.b.pushes:
				told = m.Title == "A payment was stopped" && strings.Contains(m.Body, "you approved €150.00 to Ledger, and it would have sent "+c.sent)
			case <-deadline:
				t.Fatalf("%s: the owner was not told the payment was stopped", c.name)
			}
		}
	}

	rebind(w.fingerprint("k-approved", w.same, 15_000))
	if code := w.pay(t, "k-approved", w.same, 15_000); code != http.StatusOK {
		t.Fatalf("the payment approved: %d %v, want 200", code, w.lastBody)
	}
	w.balances(t, 35_000, 15_000, 0)
}
