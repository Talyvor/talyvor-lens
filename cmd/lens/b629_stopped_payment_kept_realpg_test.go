package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// B62.9, red first — a payment stopped because it is not the one approved is kept, so its approval can say why. An
// approval signed for €150.00 to Ledger is pointed at a payment to another payee, then of another amount, then (its
// signed statement saying US dollars) of another currency: each is stopped with nothing moved, and the approval reads
// back stopped with the field that differed. On main nothing is kept, so it reads back no `stopped`, and there is no
// read of one approval at all. The payment approved then goes out, and its proof is read by the approval's id, which
// on main is 404. Another workspace's owner gets 404 from both reads.
func TestB629_AStoppedPaymentIsKeptAndAProofIsReadByItsApproval(t *testing.T) {
	w := newB621World(t, "ws-b629-stopped")
	ctx := context.Background()
	go func() {
		for range w.b.pushes { // what the owner is told is B62.1's; drained so no push waits on a full channel
		}
	}()
	get := func(who *auth.AuthContext, ws, path string, into any) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/agents/"+path, nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), who))
		rec := httptest.NewRecorder()
		w.router.ServeHTTP(rec, req)
		_ = json.Unmarshal(rec.Body.Bytes(), into)
		return rec.Code
	}

	if code := w.pay(t, "k-approved", w.same, 15_000); code != http.StatusAccepted {
		t.Fatalf("pay €150.00: %d %v, want 202 for the owner's approval", code, w.lastBody)
	}
	approval := w.approvalOf["k-approved"]
	w.approveSigned(t, approval)
	var signed string
	if err := w.pool.QueryRow(ctx, `SELECT owner_statement FROM agent_approvals WHERE id = $1`, approval).Scan(&signed); err != nil || signed == "" {
		t.Fatalf("the signed statement = %q (%v)", signed, err)
	}
	// The approval as the apps read it.
	type approvalRead struct {
		Status  string `json:"status"`
		Stopped *struct {
			Changed string    `json:"changed"`
			At      time.Time `json:"at"`
		} `json:"stopped"`
	}
	set := func(fp, statement string) {
		t.Helper()
		if _, err := w.pool.Exec(ctx, `UPDATE agent_approvals SET fingerprint = $2, owner_statement = $3 WHERE id = $1`, approval, fp, statement); err != nil {
			t.Fatal(err)
		}
	}

	for _, c := range []struct {
		changed, key, to string
		minor            int64
		statement        string
	}{
		{"payee", "k-payee", w.far, 15_000, signed},
		{"amount", "k-more", w.same, 19_000, signed},
		{"currency", "k-currency", w.same, 15_000, strings.Replace(signed, `"currency":"EUR"`, `"currency":"USD"`, 1)},
	} {
		set(w.fingerprint(c.key, c.to, c.minor), c.statement)
		if code := w.pay(t, c.key, c.to, c.minor); code != http.StatusForbidden {
			t.Fatalf("%s changed: %d %v, want 403 as not the payment approved", c.changed, code, w.lastBody)
		}
		w.balances(t, 50_000, 0, 0) // nothing moved and nothing is held
		var a approvalRead
		if code := get(w.owner, w.ws, "approvals/"+approval, &a); code != http.StatusOK || a.Status != "approved" || a.Stopped == nil ||
			a.Stopped.Changed != c.changed || a.Stopped.At.IsZero() {
			t.Fatalf("%s changed: the approval reads %d %+v stopped=%+v, want still approved and stopped because the %s changed",
				c.changed, code, a, a.Stopped, c.changed)
		}
		var changed []string
		var shownMinor, sentMinor int64
		var sentPayee string
		if err := w.pool.QueryRow(ctx, `SELECT changed, (shown->>'amount_minor')::bigint, (sent->>'amount_minor')::bigint, sent->'payee'->>'id'
			FROM stopped_payments WHERE approval_id = $1 AND workspace_id = $2 ORDER BY created_at DESC LIMIT 1`, approval, w.ws).
			Scan(&changed, &shownMinor, &sentMinor, &sentPayee); err != nil || len(changed) != 1 || changed[0] != c.changed ||
			shownMinor != 15_000 || sentMinor != c.minor || sentPayee != c.to {
			t.Fatalf("%s changed: the stop kept = %v %d %d %s (%v), want what was shown, what would have been sent and what differed",
				c.changed, changed, shownMinor, sentMinor, sentPayee, err)
		}
	}
	if _, err := w.pool.Exec(ctx, `DELETE FROM stopped_payments WHERE approval_id = $1`, approval); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("delete a kept stop: %v, want refused as append-only", err)
	}

	// Another workspace's owner: 404 on the approval and on its proof.
	var none map[string]any
	if code := get(w.otherOwner, w.other, "approvals/"+approval, &none); code != http.StatusNotFound {
		t.Fatalf("another workspace reads the approval: %d, want 404", code)
	}

	// The payment approved goes out, and its proof is read by the approval's id; the approval, used, is no longer stopped.
	set(w.fingerprint("k-approved", w.same, 15_000), signed)
	if code := w.pay(t, "k-approved", w.same, 15_000); code != http.StatusOK {
		t.Fatalf("the payment approved: %d %v, want 200", code, w.lastBody)
	}
	w.balances(t, 35_000, 15_000, 0)
	var p economy.PaymentProof
	if code := get(w.owner, w.ws, "payment-proofs/"+approval, &p); code != http.StatusOK || p.ApprovalID != approval || !p.Verified ||
		p.Sent.AmountMinor != 15_000 || p.Sent.Payee.ID != w.same {
		t.Fatalf("the proof read by its approval = %d %+v, want the verified record of the €150.00 sent", code, p)
	}
	var a approvalRead
	if code := get(w.owner, w.ws, "approvals/"+approval, &a); code != http.StatusOK || a.Status != "used" || a.Stopped != nil {
		t.Fatalf("the used approval reads %d %+v, want used and not stopped", code, a)
	}
	for _, path := range []string{"approvals/" + approval, "payment-proofs/" + approval} {
		if code := get(w.otherOwner, w.other, path, &none); code != http.StatusNotFound {
			t.Fatalf("another workspace reads %s: %d, want 404", path, code)
		}
	}
}
