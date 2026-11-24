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

// B62.8 — the owner signs the agent's task too, when there is one: the statement the phone key signs holds the task
// of the approval exactly, so a record whose shown task is changed afterwards does not verify. The task still decides
// nothing: the approved payment, retried with another task, goes through on the approval. A record signed before the
// statement carried the task (an approval signed with none in its statement) still verifies, its task read as "".
func TestB628_TheOwnerSignsTheAgentsTask(t *testing.T) {
	w := newB621World(t, "ws-b628-task")
	ctx := context.Background()
	pay := func(key, task string) int {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"to_agent_id": w.same, "amount_minor": 15_000, "currency": "EUR", "memo": "Invoice " + key, "task": task})
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+w.ws+"/agents/"+w.payer+"/pay", strings.NewReader(string(body)))
		req.Header.Set("Idempotency-Key", key)
		req = req.WithContext(auth.WithAuthContext(req.Context(), w.owner))
		rec := httptest.NewRecorder()
		w.router.ServeHTTP(rec, req)
		w.lastBody = map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &w.lastBody)
		if id, ok := w.lastBody["approval_id"].(string); ok {
			w.approvalOf[key] = id
		}
		if id, ok := w.lastBody["id"].(string); ok && rec.Code == http.StatusOK {
			w.paymentOf[key] = id
		}
		return rec.Code
	}
	proof := func(key string) economy.PaymentProof {
		t.Helper()
		p, err := w.b.store.PaymentProof(ctx, w.ws, w.paymentOf[key])
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	// withShownTask is p with its shown task set to task, through its JSON as a reader of the record would see it.
	withShownTask := func(p economy.PaymentProof, task string) economy.PaymentProof {
		t.Helper()
		var m map[string]any
		_ = json.Unmarshal(b621JSON(json.Marshal(p)), &m)
		m["shown"].(map[string]any)["task"] = task
		var q economy.PaymentProof
		if err := json.Unmarshal(b621JSON(json.Marshal(m)), &q); err != nil {
			t.Fatal(err)
		}
		return q
	}

	// Signed with the task: the statement holds it exactly, and the record verifies.
	const task = "Paying Ledger's invoice for the October books"
	if code := pay("k-task", task); code != http.StatusAccepted {
		t.Fatalf("pay €150.00 with a task: %d %v, want 202", code, w.lastBody)
	}
	w.approveSigned(t, w.approvalOf["k-task"])
	if code := pay("k-task", "Another task, said on the retry"); code != http.StatusOK {
		t.Fatalf("the approved payment retried with another task: %d %v, want 200 on the approval", code, w.lastBody)
	}
	p := proof("k-task")
	var st economy.DecisionStatement
	if err := json.Unmarshal([]byte(p.Statement), &st); err != nil || len(st.Approvals) != 1 {
		t.Fatalf("the statement signed = %s (%v)", p.Statement, err)
	}
	var signed map[string]any
	_ = json.Unmarshal(b621JSON(json.Marshal(st.Approvals[0])), &signed)
	if !p.Verified || p.KeyKind != "device" || signed["task"] != task || p.Sent.Task != "Another task, said on the retry" {
		t.Fatalf("the record = %+v, want verified by the phone key, its statement holding the task %q", p, task)
	}
	var shown map[string]any
	_ = json.Unmarshal(b621JSON(json.Marshal(p.Shown)), &shown)
	if shown["task"] != task {
		t.Fatalf("the record shows %v, want the task %q", shown, task)
	}

	// The task changed in shown afterwards does not verify.
	if w.b.store.VerifyPaymentProof(ctx, withShownTask(p, "Paying Ledger's invoice, and a bonus")) {
		t.Errorf("the record with its shown task changed still verifies")
	}

	// Signed before the statement carried the task: the approval's statement is signed with none, and still verifies.
	if code := pay("k-before", task); code != http.StatusAccepted {
		t.Fatalf("pay €150.00 again: %d %v, want 202", code, w.lastBody)
	}
	before := w.approvalOf["k-before"]
	if _, err := w.pool.Exec(ctx, `UPDATE agent_approvals SET task = '' WHERE id = $1`, before); err != nil {
		t.Fatal(err)
	}
	w.approveSigned(t, before)
	if _, err := w.pool.Exec(ctx, `UPDATE agent_approvals SET task = $2 WHERE id = $1`, before, task); err != nil {
		t.Fatal(err)
	}
	if code := pay("k-before", task); code != http.StatusOK {
		t.Fatalf("the payment approved before: %d %v, want 200", code, w.lastBody)
	}
	old := proof("k-before")
	if !old.Verified || strings.Contains(old.Statement, `"task"`) {
		t.Fatalf("the record signed without the task = %+v, want verified, its statement with no task", old)
	}
	if w.b.store.VerifyPaymentProof(ctx, withShownTask(old, task)) {
		t.Errorf("the record signed without the task verifies with a task it never signed")
	}
}
