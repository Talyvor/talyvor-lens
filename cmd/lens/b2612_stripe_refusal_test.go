package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stripe/stripe-go/v81"

	"github.com/talyvor/lens/internal/agentcard"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
)

// b2612Stripe is Stripe's API answering every request with one error, as Stripe sends it.
func b2612Stripe(t *testing.T, status int, errType, msg string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": errType, "message": msg}})
	}))
	t.Cleanup(srv.Close)
	prev := stripe.GetBackend(stripe.APIBackend)
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL: stripe.String(srv.URL), MaxNetworkRetries: stripe.Int64(0),
	}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prev) })
}

func b2612Post(t *testing.T, r http.Handler, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{
		WorkspaceID: "ws-b2612", AuthMethod: auth.MethodJWT, UserID: "user-owner", Scopes: []string{auth.ScopeKeys}}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out struct{ Error string }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out.Error
}

// b2612Bank holds an agent with no card yet.
type b2612Bank struct{}

func (b2612Bank) AgentBook(context.Context, string) (economy.AgentBook, error) {
	return economy.AgentBook{}, nil
}
func (b2612Bank) GetAgentCard(context.Context, string, string) (economy.AgentCard, error) {
	return economy.AgentCard{}, economy.ErrNoAgentCard
}
func (b2612Bank) SaveAgentCard(_ context.Context, _ string, c economy.AgentCard) (economy.AgentCard, error) {
	return c, nil
}
func (b2612Bank) ListAgentCardAuthorizations(context.Context, string, string, int) ([]economy.CardAuthorizationRecord, error) {
	return nil, nil
}
func (b2612Bank) SetAgentCardFrozen(context.Context, string, string, bool) (economy.AgentCard, error) {
	return economy.AgentCard{}, economy.ErrNoAgentCard
}
func (b2612Bank) SetAgentCardDrawsOn(context.Context, string, string, string) (economy.AgentCard, error) {
	return economy.AgentCard{}, economy.ErrNoAgentCard
}

// B26.12 — a cardholder Stripe refuses answers 400 with Stripe's reason, its ids taken out; Stripe refusing
// Lens's own key stays a 502 that says nothing.
func TestB2612_ACardholderStripeRefusesAnswers400WithStripesReason(t *testing.T) {
	const path = "/v1/workspaces/ws-b2612/agents/agt-1/card"
	holder := `{"first_name":"Nicolai","last_name":"Gab0rac","line1":"1 High Street","city":"London","postal_code":"EC1A 1BB"}`

	b2612Stripe(t, http.StatusBadRequest, "invalid_request_error",
		"Invalid individual[last_name]: must not contain numbers (cardholder ich_1PzT3stAbCdEf0123).")
	r := chi.NewRouter()
	mountAgentCardRoutes(r, b2612Bank{}, agentcard.NewStripe("sk_test_b2612", "gbp"))
	code, why := b2612Post(t, r, path, holder)
	if code != http.StatusBadRequest || why != "Stripe says: Invalid individual[last_name]: must not contain numbers (cardholder …)." {
		t.Fatalf("a cardholder Stripe refuses = %d %q, want 400 with Stripe's reason and no id", code, why)
	}

	b2612Stripe(t, http.StatusUnauthorized, "invalid_request_error", "Invalid API Key provided: sk_test_****b2612")
	r = chi.NewRouter()
	mountAgentCardRoutes(r, b2612Bank{}, agentcard.NewStripe("sk_test_b2612", "gbp"))
	if code, why := b2612Post(t, r, path, holder); code != http.StatusBadGateway || strings.Contains(why, "sk_") {
		t.Fatalf("Stripe refusing Lens's key = %d %q, want 502 naming no key", code, why)
	}
}

// B26.12 — a seller account Stripe will not open answers 400 with Stripe's reason.
func TestB2612_APayoutAccountStripeRefusesAnswers400WithStripesReason(t *testing.T) {
	pool := agentRoutesDB(t)
	b2612Stripe(t, http.StatusBadRequest, "invalid_request_error", "Connect is not available to accounts in AQ.")
	r := chi.NewRouter()
	mountMarketPayoutRoutes(r, market.NewStore(pool), everyWorkspace(billing.NewTestModeStripe("sk_test_b2612", "", "")), nil, marketPayoutURLs{})
	code, why := b2612Post(t, r, "/v1/workspaces/ws-b2612/marketplace/payouts/connect", `{"country":"AQ","email":"owner@b2612.example"}`)
	if code != http.StatusBadRequest || why != "Stripe says: Connect is not available to accounts in AQ." {
		t.Fatalf("a seller account Stripe refuses = %d %q, want 400 with Stripe's reason", code, why)
	}
}
