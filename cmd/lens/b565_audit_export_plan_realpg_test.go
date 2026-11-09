package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/audit"
	"github.com/talyvor/lens/internal/auth"
)

type noopAuditWebhook struct{}

func (noopAuditWebhook) ExportWebhook(context.Context, string, audit.ExportFilter) error { return nil }

// B56.5 — audit export follows the plan card: GET /v1/audit/export and POST /v1/audit/webhook refuse Free and Team
// with 402 in words naming Business, and serve Business and Enterprise as before; a synthetic workspace by its plan.
// The refusal names no setting.
func TestB565_AuditExportFollowsThePlanCard(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO subscriptions (workspace_id, stripe_subscription_id, stripe_customer_id, price_id, status, livemode, last_event_at, plan)
			VALUES ('ws-b565-team', 'sub_b565_team', 'cus_b565_team', 'price_team', 'active', false, NOW(), 'team'),
			       ('ws-b565-business', 'sub_b565_business', 'cus_b565_business', 'price_business', 'active', false, NOW(), 'business')`,
		`INSERT INTO workspace_contracts (workspace_id, plan, set_by) VALUES ('ws-b565-enterprise', 'enterprise', 'nicolai')`,
		`INSERT INTO workspaces (id, name, cache_prefix, synthetic, synthetic_plan)
			VALUES ('ws-b565-synth-team', 'ws-b565-synth-team', 'ws-b565-synth-team', true, 'team'),
			       ('ws-b565-synth-business', 'ws-b565-synth-business', 'ws-b565-synth-business', true, 'business')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	r := chi.NewRouter()
	r.Get("/v1/audit/export", newAuditExportHandler(audit.New(pool), pool))
	r.Post("/v1/audit/webhook", newAuditWebhookHandler(noopAuditWebhook{}, pool))
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "admin"}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	for _, c := range []struct {
		ws, plan string
		served   bool
	}{
		{"ws-b565-free", "Free", false},
		{"ws-b565-team", "Team", false},
		{"ws-b565-synth-team", "Team", false},
		{"ws-b565-business", "Business", true},
		{"ws-b565-enterprise", "Enterprise", true},
		{"ws-b565-synth-business", "Business", true},
	} {
		exportCode, exportBody := call(c.ws, http.MethodGet, "/v1/audit/export", "")
		hookCode, hookBody := call(c.ws, http.MethodPost, "/v1/audit/webhook", `{"webhook_url":"https://siem.example/hook"}`)
		if c.served {
			if exportCode != http.StatusOK || hookCode != http.StatusAccepted {
				t.Fatalf("%s: export %d %s, webhook %d %s; want 200 and 202 as before", c.ws, exportCode, exportBody, hookCode, hookBody)
			}
			continue
		}
		want := c.plan + " does not include audit export. Business includes it."
		for _, got := range []struct {
			code int
			body string
		}{{exportCode, exportBody}, {hookCode, hookBody}} {
			var out map[string]any
			_ = json.Unmarshal([]byte(got.body), &out)
			if got.code != http.StatusPaymentRequired || out["error"] != want || out["allows"] != "business" || out["gate"] != "audit_export" {
				t.Fatalf("%s: %d %s; want 402 saying %q", c.ws, got.code, got.body, want)
			}
			if strings.Contains(got.body, "LENS_") || strings.Contains(got.body, "setting") {
				t.Fatalf("%s: the refusal %s names a setting", c.ws, got.body)
			}
		}
	}
}
