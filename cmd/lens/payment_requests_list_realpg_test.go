package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// B40.38 — GET /v1/money/payment-requests?status=pending_approval lists only the caller's workspace's requests waiting
// for approval, newest first, each as GET /{id} returns it: another workspace's, an approved one and a cancelled one are
// absent, and any other status is 400.
func TestPaymentRequests_ListPending(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, other = "ws-b4038-mine", "ws-b4038-other"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetPayeeScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	r := chi.NewRouter()
	mountPaymentRequestRoutes(r, store)
	list := func(workspace, status string) (int, []map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/money/payment-requests?status="+status, nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: workspace, AuthMethod: auth.MethodJWT,
			UserID: "ada", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out struct {
			Requests []map[string]any `json:"requests"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Requests
	}

	// No mandate names the payee, so every request needs approval.
	prepare := func(workspace, key string) string {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO money_payees (id, workspace_id, name, country, sort_code, account_number, check_result, checked_by)
			VALUES ($1, $2, 'Ada Plumbing', 'GB', '20-00-00', '55779911', 'exact_match', 'test') ON CONFLICT DO NOTHING`, "payee_"+workspace, workspace); err != nil {
			t.Fatal(err)
		}
		pr, _, err := store.PreparePaymentRequest(ctx, workspace, "ada", key, economy.PaymentRequestIn{Client: "chat",
			Items: []economy.PaymentRequestItemIn{{PayeeID: "payee_" + workspace, Currency: "GBP", AmountMinor: 4200, Reference: key}}})
		if err != nil || pr.Status != "pending_approval" {
			t.Fatalf("prepare %s = %v %v, want pending_approval", key, pr.Status, err)
		}
		return pr.ID
	}
	older, approved, cancelled, newer := prepare(ws, "older"), prepare(ws, "approved"), prepare(ws, "cancelled"), prepare(ws, "newer")
	prepare(other, "theirs")
	if _, err := pool.Exec(ctx, `UPDATE payment_requests SET status = 'approved' WHERE id = $1`, approved); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CancelPaymentRequest(ctx, ws, "", cancelled, "ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE payment_requests SET created_at = now() - interval '1 hour' WHERE id = $1`, older); err != nil {
		t.Fatal(err)
	}

	code, got := list(ws, "pending_approval")
	if code != http.StatusOK || len(got) != 2 || got[0]["id"] != newer || got[1]["id"] != older {
		t.Fatalf("the pending list = %d %v, want [%s %s]", code, got, newer, older)
	}
	one, err := store.PaymentRequest(ctx, ws, "", newer)
	if err != nil {
		t.Fatal(err)
	}
	if got[0]["payload_hash"] != one.PayloadHash || got[0]["summary"] != one.Summary || len(got[0]["items"].([]any)) != 1 {
		t.Errorf("the listed request %v is not as GET /{id} returns it", got[0])
	}
	if code, _ := list(ws, "approved"); code != http.StatusBadRequest {
		t.Errorf("?status=approved = %d, want 400", code)
	}
}
