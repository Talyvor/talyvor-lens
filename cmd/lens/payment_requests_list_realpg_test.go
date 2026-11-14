package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// B55.9 — a request nobody answered within the approval life reads expired: GET /{id} says so, the pending list leaves it
// out, approving it is refused in words, and it can still be cancelled. One inside its life is unchanged.
func TestPaymentRequests_AnUnansweredRequestExpires(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b559"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetPayeeScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	store.SetApprovalTTL(time.Second)
	r := chi.NewRouter()
	mountPaymentRequestRoutes(r, store)
	get := func(path string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT,
			UserID: "ada", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, w.Code, w.Body.String())
		}
		return out
	}
	if _, err := pool.Exec(ctx, `INSERT INTO money_payees (id, workspace_id, name, country, sort_code, account_number, check_result, checked_by)
		VALUES ('payee_b559', $1, 'Ada Plumbing', 'GB', '20-00-00', '55779911', 'exact_match', 'test')`, ws); err != nil {
		t.Fatal(err)
	}
	prepare := func(key string) string {
		t.Helper()
		pr, _, err := store.PreparePaymentRequest(ctx, ws, "ada", key, economy.PaymentRequestIn{Client: "chat",
			Items: []economy.PaymentRequestItemIn{{PayeeID: "payee_b559", Currency: "GBP", AmountMinor: 4200, Reference: key}}})
		if err != nil || pr.Status != "pending_approval" {
			t.Fatalf("prepare %s = %v %v, want pending_approval", key, pr.Status, err)
		}
		return pr.ID
	}
	stale, fresh := prepare("stale"), prepare("fresh")
	if _, err := pool.Exec(ctx, `UPDATE payment_requests SET created_at = now() - interval '2 seconds' WHERE id = $1`, stale); err != nil {
		t.Fatal(err)
	}

	if got := get("/v1/money/payment-requests/" + stale)["status"]; got != "expired" {
		t.Errorf("the unanswered request reads %v, want expired", got)
	}
	if got := get("/v1/money/payment-requests/" + fresh)["status"]; got != "pending_approval" {
		t.Errorf("the request inside its life reads %v, want pending_approval", got)
	}
	list := get("/v1/money/payment-requests?status=pending_approval")["requests"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != fresh {
		t.Errorf("the pending list = %v, want only %s", list, fresh)
	}
	const refusal = "this request waited 24 hours without an answer and expired: ask for it again"
	if _, _, _, err := store.PrepareRequestApproval(ctx, ws, stale, nil, ""); !errors.Is(err, economy.ErrPaymentRequest) ||
		!strings.HasSuffix(err.Error(), refusal) {
		t.Errorf("approving the unanswered request = %v, want %q", err, refusal)
	}
	if _, _, _, err := store.PrepareRequestApproval(ctx, ws, fresh, nil, ""); err != nil && strings.Contains(err.Error(), "expired") {
		t.Errorf("approving the request inside its life = %v", err)
	}
	got, err := store.CancelPaymentRequest(ctx, ws, "", stale, "ada")
	if err != nil || got.Status != "cancelled" || got.CancelledAt == nil || got.Items[0].PaymentID != "" {
		t.Errorf("cancelling the unanswered request = %v %+v, want it cancelled, unpaid", err, got)
	}
}

// B57.148 — GET /v1/money/payment-requests?status=decided lists the caller's workspace's requests that were approved,
// cancelled or expired, newest decision first, each with decided_at: when the signed approval was kept, when it was
// cancelled, or when its approval life ran out. One still waiting and another workspace's are absent.
func TestPaymentRequests_ListDecided(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, other = "ws-b57148-mine", "ws-b57148-other"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetPayeeScreener(screening.NewScreener(pool, partners.NewRegistry(nil)))
	store.SetApprovalTTL(time.Hour)
	r := chi.NewRouter()
	mountPaymentRequestRoutes(r, store)
	list := func(workspace string) (int, []map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/money/payment-requests?status=decided", nil)
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
	prepare := func(workspace, key string) string {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO money_payees (id, workspace_id, name, country, sort_code, account_number, check_result, checked_by)
			VALUES ($1, $2, 'Northwind Data', 'GB', '20-00-00', '55779911', 'exact_match', 'test') ON CONFLICT DO NOTHING`, "payee_"+workspace, workspace); err != nil {
			t.Fatal(err)
		}
		pr, _, err := store.PreparePaymentRequest(ctx, workspace, "ada", key, economy.PaymentRequestIn{Client: "chat",
			Items: []economy.PaymentRequestItemIn{{PayeeID: "payee_" + workspace, Currency: "GBP", AmountMinor: 4200, Reference: key}}})
		if err != nil || pr.Status != "pending_approval" {
			t.Fatalf("prepare %s = %v %v, want pending_approval", key, pr.Status, err)
		}
		return pr.ID
	}
	approved, cancelled, expired, pending := prepare(ws, "approved"), prepare(ws, "cancelled"), prepare(ws, "expired"), prepare(ws, "pending")
	theirs := prepare(other, "theirs")
	approvedAt := time.Date(2026, 11, 10, 10, 31, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `INSERT INTO payment_request_approvals (request_id, workspace_id, statement, signature, approved_by, approved_at)
		VALUES ($1, $2, '{}', 'sig', 'ada', $3)`, approved, ws, approvedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE payment_requests SET status = 'approved' WHERE id = $1`, approved); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{cancelled, theirs} {
		workspace := ws
		if id == theirs {
			workspace = other
		}
		if _, err := store.CancelPaymentRequest(ctx, workspace, "", id, "ada"); err != nil {
			t.Fatal(err)
		}
	}
	cancelledAt := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `UPDATE payment_requests SET cancelled_at = $2 WHERE id = $1`, cancelled, cancelledAt); err != nil {
		t.Fatal(err)
	}
	// Unanswered since 07:00 with an hour's approval life: it expired at 08:00.
	if _, err := pool.Exec(ctx, `UPDATE payment_requests SET created_at = $2 WHERE id = $1`, expired,
		time.Date(2026, 11, 10, 7, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}

	code, got := list(ws)
	if code != http.StatusOK || len(got) != 3 {
		t.Fatalf("the decided list = %d %v, want 3 requests", code, got)
	}
	want := []struct {
		id, status string
		at         time.Time
	}{{approved, "approved", approvedAt}, {cancelled, "cancelled", cancelledAt}, {expired, "expired", time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)}}
	for i, w := range want {
		at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(got[i]["decided_at"]))
		if got[i]["id"] != w.id || got[i]["status"] != w.status || err != nil || !at.Equal(w.at) {
			t.Errorf("decided[%d] = %v %v decided_at %v, want %s %s %s", i, got[i]["id"], got[i]["status"], got[i]["decided_at"], w.id, w.status, w.at)
		}
	}
	for _, g := range got {
		if g["id"] == pending || g["id"] == theirs {
			t.Errorf("the decided list holds %v, a request still waiting or another workspace's", g["id"])
		}
	}
	if one, err := store.PaymentRequest(ctx, ws, "", pending); err != nil || one.DecidedAt != nil {
		t.Errorf("the request still waiting reads decided_at %v (%v), want none", one.DecidedAt, err)
	}
	if code, got := list(other); code != http.StatusOK || len(got) != 1 || got[0]["id"] != theirs {
		t.Errorf("the other workspace's decided list = %d %v, want only %s", code, got, theirs)
	}
}
