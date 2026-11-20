package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B62.1 — every payment the rules let through has one record that cannot be changed: a payment the owner approved
// reads back what they were shown (the agent, the payee, the amount, the currency, the rule and the rules version), the
// phone key that signed it and its signature over exactly those fields, and what was sent; a payment inside the rules
// has the same record with no signature, naming the rule that passed it. Neither can be updated or deleted, a
// signature over other fields does not verify, and another workspace's owner gets 404.
func TestB621_EveryPaymentTheRulesLetThroughHasAProofRecord(t *testing.T) {
	w := newB621World(t, "ws-b621-proofs")
	ctx := context.Background()
	read := func(who *auth.AuthContext, ws, id string) (int, economy.PaymentProof) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/agents/payment-proofs/"+id, nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		rec := httptest.NewRecorder()
		w.router.ServeHTTP(rec, req)
		var p economy.PaymentProof
		_ = json.Unmarshal(rec.Body.Bytes(), &p)
		return rec.Code, p
	}

	// Inside the rules: €50.00 to Ledger, recorded unsigned under the rule that passed it.
	if code := w.pay(t, "k-inside", w.same, 5_000); code != http.StatusOK {
		t.Fatalf("pay €50.00: %d %v", code, w.lastBody)
	}
	code, inside := read(w.owner, w.ws, w.paymentOf["k-inside"])
	if code != http.StatusOK || inside.Rule != economy.ProofRuleWithinRules || inside.RulesVersion == nil || inside.Statement != "" ||
		len(inside.Signature) != 0 || inside.Verified || inside.Sent.Payee.ID != w.same || inside.Sent.AmountMinor != 5_000 ||
		inside.Sent.Currency != "EUR" || inside.Sent.Rail != "account" {
		t.Fatalf("the record of €50.00 inside the rules = %d %+v, want the rule within_rules, the rules version, no signature", code, inside)
	}

	// Approved: €150.00 to Ledger, signed with the owner's phone key.
	if code := w.pay(t, "k-approved", w.same, 15_000); code != http.StatusAccepted {
		t.Fatalf("pay €150.00: %d %v, want 202", code, w.lastBody)
	}
	approval := w.approvalOf["k-approved"]
	w.approveSigned(t, approval)
	if code := w.pay(t, "k-approved", w.same, 15_000); code != http.StatusOK {
		t.Fatalf("pay €150.00 after the owner's yes: %d %v", code, w.lastBody)
	}
	w.balances(t, 30_000, 20_000, 0)
	code, p := read(w.owner, w.ws, w.paymentOf["k-approved"])
	s := p.Shown
	if code != http.StatusOK || p.ApprovalID != approval || p.Rule != economy.ProofRuleApprovalAbove || p.RulesVersion == nil ||
		s.ApprovalID != approval || s.AgentID != w.payer || s.Payee.Kind != "agent" || s.Payee.ID != w.same || s.Payee.Name != "Ledger" ||
		s.AmountMinor != 15_000 || s.Currency != "EUR" || s.Rule != economy.ProofRuleApprovalAbove || s.RulesVersion == nil ||
		p.KeyKind != "device" || p.KeyID != "dev_"+w.ws || p.SignedAt == nil || !p.Verified ||
		p.Sent.Payee.ID != w.same || p.Sent.AmountMinor != 15_000 || p.Sent.Currency != "EUR" || p.Sent.Rail != "account" ||
		p.Sent.Reference == "" || p.Sent.Memo == "" {
		t.Fatalf("the record of the approved €150.00 = %d %+v, want what was shown, the rule, the phone key, verified, and what was sent", code, p)
	}
	var st economy.DecisionStatement
	signed, _ := json.Marshal(s)
	if err := json.Unmarshal([]byte(p.Statement), &st); err != nil || len(st.Approvals) != 1 || string(b621JSON(json.Marshal(st.Approvals[0]))) != string(signed) {
		t.Fatalf("the statement signed = %s (%v), want exactly the fields shown", p.Statement, err)
	}
	if code, byRef := read(w.owner, w.ws, p.PaymentRef); code != http.StatusOK || byRef.ID != p.ID {
		t.Fatalf("read by the payment's reference = %d %+v", code, byRef)
	}

	// A signature over other fields does not verify: the shown amount, the statement, or the payee changed after.
	for name, change := range map[string]func(*economy.PaymentProof){
		"a larger amount shown": func(q *economy.PaymentProof) { q.Shown.AmountMinor = 19_000 },
		"another payee shown":   func(q *economy.PaymentProof) { q.Shown.Payee.ID = w.far },
		"the statement changed": func(q *economy.PaymentProof) {
			q.Statement = strings.Replace(q.Statement, `"amount_minor":15000`, `"amount_minor":19000`, 1)
			q.Shown.AmountMinor = 19_000
		},
	} {
		q := p
		change(&q)
		if w.b.store.VerifyPaymentProof(ctx, q) {
			t.Errorf("%s: the record still verifies", name)
		}
	}

	// It can be neither updated nor deleted.
	for _, sql := range []string{`UPDATE payment_proofs SET sent = '{}' WHERE id = $1`, `DELETE FROM payment_proofs WHERE id = $1`} {
		if _, err := w.pool.Exec(ctx, sql, p.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%s: %v, want refused as append-only", sql, err)
		}
	}
	if code, again := read(w.owner, w.ws, p.ID); code != http.StatusOK || !again.Verified || again.Sent != p.Sent {
		t.Fatalf("the record after = %d %+v", code, again)
	}

	// Another workspace's owner: 404, by the record's id, the payment's reference or its entry.
	for _, id := range []string{p.ID, p.PaymentRef, w.paymentOf["k-approved"]} {
		if code, _ := read(w.otherOwner, w.other, id); code != http.StatusNotFound {
			t.Fatalf("another workspace reads %s: %d, want 404", id, code)
		}
	}
}

func b621JSON(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
