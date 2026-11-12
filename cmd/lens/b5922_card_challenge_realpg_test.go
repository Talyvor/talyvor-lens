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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
)

// B59.22 — 3-D Secure, against the Sandbox's 3-D Secure on real Postgres. A Sandbox challenge on an agent's card
// appears in the approvals inbox as an approval of its own kind — the merchant, the amount and the card — and is pushed
// to the owner. An unsigned answer is refused, even in a workspace that has no key; the owner's signed yes goes back
// to the issuer; a challenge nobody answers is declined to the issuer at its deadline.
func TestB5922_CardChallengeWaitsForTheOwnersSignedAnswer(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5922"
	threeDS := &money.SandboxThreeDS{Partner: money.SandboxName}
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			sandbox := money.NewSandbox(reg, "whsec_b5922")
			sandbox.ThreeDS = threeDS
			layer, err := money.NewRegistry(money.Config{Sandbox: sandbox, Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
				Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool)})
			if err != nil {
				t.Fatal(err)
			}
			store.SetSandboxThreeDS(threeDS)
			return layer
		})
	b.store.SetDeviceKeys(devicekey.Verifier{AllowTest: true})
	agent := b.acct.AgentID
	r := chi.NewRouter()
	mountCardSpendRoutes(r, b.store)
	mountCardChallengeRoutes(r, b.store)
	mountAgentAccountRoutes(r, b.store, nil)
	mountDeviceKeyRoutes(r, b.store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string, out any) int {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if out != nil {
			_ = json.Unmarshal(w.Body.Bytes(), out)
		}
		return w.Code
	}

	// The agent's one-time card for Figma, from the Sandbox, within its rules.
	var spend economy.CardSpend
	if code := call(http.MethodPost, "/v1/money/card-spends", `{"agent_id":"`+agent+`","merchant":"Figma","amount_minor":4500,"currency":"GBP",
		"idempotency_key":"figma"}`, &spend); code != http.StatusCreated || spend.Card == nil {
		t.Fatalf("the agent's card = %d %+v, want 201 and a card", code, spend)
	}
	challenge := func() economy.CardChallenge {
		t.Helper()
		var c economy.CardChallenge
		if code := call(http.MethodPost, "/v1/money/card-challenges", `{"card_id":"`+spend.Card.ID+`","merchant":"Figma","amount_minor":4500,
			"currency":"GBP"}`, &c); code != http.StatusCreated || c.Status != economy.CardChallengePending || c.ApprovalID == "" {
			t.Fatalf("the Sandbox challenges a payment = %d %+v, want 201, pending on an approval", code, c)
		}
		return c
	}
	approval := func(id string) economy.AgentApproval {
		t.Helper()
		var list struct {
			Approvals []economy.AgentApproval `json:"approvals"`
		}
		call(http.MethodGet, "/v1/workspaces/"+ws+"/agents/approvals", "", &list)
		for _, a := range list.Approvals {
			if a.ID == id {
				return a
			}
		}
		t.Fatalf("approval %s is not in the inbox: %+v", id, list.Approvals)
		return economy.AgentApproval{}
	}
	base := "/v1/workspaces/" + ws + "/agents/approvals/"

	// A Sandbox challenge appears as an approval of its own kind, and the owner is told.
	first := challenge()
	a := approval(first.ApprovalID)
	if a.Kind != economy.ApprovalCardChallenge || a.Status != "pending" || a.AgentID != agent || a.Payee == nil || a.Payee.Name != "Figma" ||
		a.PaidFrom == nil || a.PaidFrom.Kind != "card" || a.PaidFrom.AccountID != spend.Card.ID || a.PaidFrom.AmountMinor != 4500 || a.PaidFrom.Currency != "GBP" {
		t.Fatalf("the challenge's approval = %+v (paid from %+v), want a pending card_challenge for £45.00 at Figma on the agent's card", a, a.PaidFrom)
	}
	if m := b.pushes.next(t); m.Data["type"] != economy.ApprovalCardChallenge || m.Data["approval_id"] != first.ApprovalID ||
		m.Data["amount_minor"] != "4500" || m.Data["currency"] != "GBP" || m.Data["payee_name"] != "Figma" {
		t.Fatalf("pushed to the owner's phone: %+v, want the card challenge for £45.00 at Figma", m)
	}
	if again, err := b.store.ReceiveCardChallenge(ctx, money.SandboxName, money.ThreeDSChallenge{ID: first.ID, Card: "any",
		Merchant: "Figma", Amount: money.Amount{Minor: 4500, Asset: "GBP"}, ExpiresAt: first.ExpiresAt}); err != nil || again.ApprovalID != first.ApprovalID {
		t.Fatalf("the same challenge sent again = %+v %v, want the same approval", again, err)
	}

	// An unsigned answer is refused — even with no key in the workspace, where other approvals are decided unsigned.
	if _, err := pool.Exec(ctx, `DELETE FROM workspace_device_keys WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	for _, decision := range []string{"approve", "deny"} {
		if code := call(http.MethodPost, base+first.ApprovalID+"/"+decision, `{}`, nil); code != http.StatusForbidden {
			t.Fatalf("%s the challenge unsigned = %d, want 403", decision, code)
		}
	}
	if _, answered := threeDS.Answered(first.ID); answered || approval(first.ApprovalID).Status != "pending" {
		t.Fatalf("after unsigned answers the challenge is %q and the issuer answered %v, want it pending and the issuer told nothing",
			approval(first.ApprovalID).Status, answered)
	}

	// The owner's phone signs yes: the issuer is told yes.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	b64 := base64.RawURLEncoding.EncodeToString
	sign := func(c string) string {
		h := sha256.Sum256(devicekey.Message(c))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, h[:])
		return b64(sig)
	}
	var ch map[string]any
	call(http.MethodPost, "/v1/money/devices/challenge", "", &ch)
	var dev map[string]any
	if code := call(http.MethodPost, "/v1/money/devices", jsonOf(map[string]string{"platform": "test", "name": "Owner's phone",
		"public_key": b64(spki), "challenge": ch["challenge"].(string), "signature": sign(ch["challenge"].(string)), "attestation": ""}), &dev); code != http.StatusCreated {
		t.Fatalf("register the owner's phone = %d %v", code, dev)
	}
	call(http.MethodPost, base+first.ApprovalID+"/challenge", "", &ch)
	signed := jsonOf(map[string]any{"assertion": map[string]string{"device_id": dev["id"].(string), "challenge": ch["challenge"].(string),
		"signature": sign(ch["challenge"].(string))}})
	if code := call(http.MethodPost, base+first.ApprovalID+"/approve", signed, nil); code != http.StatusOK {
		t.Fatalf("approve the challenge signed by the owner's phone = %d, want 200", code)
	}
	if yes, answered := threeDS.Answered(first.ID); !yes || !answered {
		t.Fatalf("the issuer was answered %v (answered: %v), want yes", yes, answered)
	}
	var status string
	var told bool
	if err := pool.QueryRow(ctx, `SELECT status, answered FROM money_card_challenges WHERE id = $1`, first.ID).Scan(&status, &told); err != nil ||
		status != economy.CardChallengeApproved || !told {
		t.Fatalf("the challenge is %q, issuer told %v (%v), want approved and told", status, told, err)
	}

	// No answer: declined to the issuer at its deadline, and not before.
	second := challenge()
	if n, err := b.store.RunCardChallengeTimeouts(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("timeouts before the deadline = %d %v, want none", n, err)
	}
	if _, answered := threeDS.Answered(second.ID); answered {
		t.Fatal("the issuer was answered before the deadline")
	}
	if n, err := b.store.RunCardChallengeTimeouts(ctx, second.ExpiresAt.Add(time.Second)); err != nil || n != 1 {
		t.Fatalf("timeouts after the deadline = %d %v, want 1", n, err)
	}
	if yes, answered := threeDS.Answered(second.ID); yes || !answered {
		t.Fatalf("at the deadline the issuer was answered %v (answered: %v), want no", yes, answered)
	}
	if s := approval(second.ApprovalID).Status; s != "expired" {
		t.Fatalf("the unanswered challenge's approval is %q, want expired", s)
	}
}
