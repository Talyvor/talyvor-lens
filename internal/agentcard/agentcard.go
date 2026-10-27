// Package agentcard issues agents virtual cards through Stripe Issuing and answers their authorisation
// requests in real time (B19.12). Cards are class RED (B22): TEST MODE ONLY until a licensed partner
// issues them, so a live Stripe key issues nothing and a live-mode authorisation is declined.
//
// Stripe sends each purchase to the real-time endpoint (Dashboard → Issuing → settings) as an
// issuing_authorization.request and waits up to two seconds for {"approved": true|false}. Handler verifies
// the signature, prices the request in USD at the day's ECB reference rate (internal/ecbrate), and lets
// economy.AuthorizeAgentCard judge it by the agent's rules and balance and record it.
package agentcard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	stripe "github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/client"
	"github.com/stripe/stripe-go/v81/webhook"

	"github.com/talyvor/lens/internal/ecbrate"
	"github.com/talyvor/lens/internal/economy"
)

// ErrLiveKey: the Stripe key is a live one, and cards are test money only.
var ErrLiveKey = errors.New("agentcard: agent cards take no real money until a licensed partner issues them (class RED) — Lens holds a live Stripe key, so it issues none")

// ErrNotConfigured: Lens has no Stripe key.
var ErrNotConfigured = errors.New("agentcard: no Stripe key is configured, so no card can be issued")

// Cardholder is the person a card is issued to: the agent's owner, who answers for it (B19.11).
type Cardholder struct {
	FirstName  string `json:"first_name"`
	LastName   string `json:"last_name"`
	Email      string `json:"email"`
	Phone      string `json:"phone_number"` // E.164; testPhone when empty (B17.19)
	Line1      string `json:"line1"`
	Line2      string `json:"line2"`
	City       string `json:"city"`
	PostalCode string `json:"postal_code"`
	Country    string `json:"country"` // ISO 3166-1 alpha-2; GB when empty
}

// testPhone is the cardholder's phone when they give none: Stripe issues no card to a cardholder without one
// (it sends 3-D Secure codes there), and every card here is test mode, where Stripe sends none. Ofcom keeps
// +44 7700 900000–900999 unallocated for drama, so it reaches nobody (B17.19).
const testPhone = "+447700900000"

// Validate says what a cardholder is missing, or "".
func (c Cardholder) Validate() string {
	var missing []string
	for _, f := range []struct{ name, v string }{
		{"first_name", c.FirstName}, {"last_name", c.LastName}, {"line1", c.Line1}, {"city", c.City}, {"postal_code", c.PostalCode},
	} {
		if strings.TrimSpace(f.v) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return "the cardholder needs " + strings.Join(missing, ", ")
	}
	return ""
}

// Issuer issues an agent's card.
type Issuer interface {
	IssueCard(ctx context.Context, workspaceID, agentID, agentName string, holder Cardholder) (economy.AgentCard, error)
}

// Stripe is the Issuer on Stripe's API.
type Stripe struct {
	key      string
	currency string
	api      *client.API
}

// NewStripe issues cards in currency (the platform's: gbp for a UK platform) with secretKey.
func NewStripe(secretKey, currency string) *Stripe {
	s := &Stripe{key: secretKey, currency: strings.ToLower(currency)}
	if secretKey != "" {
		s.api = client.New(secretKey, nil)
	}
	return s
}

// TestMode reports whether key is a Stripe test-mode key.
func TestMode(key string) bool {
	return strings.HasPrefix(key, "sk_test_") || strings.HasPrefix(key, "rk_test_")
}

// IssueCard creates the owner as a Stripe cardholder and issues them an active virtual card for the agent.
func (s *Stripe) IssueCard(ctx context.Context, workspaceID, agentID, agentName string, holder Cardholder) (economy.AgentCard, error) {
	if s.key == "" {
		return economy.AgentCard{}, ErrNotConfigured
	}
	if !TestMode(s.key) {
		return economy.AgentCard{}, ErrLiveKey
	}
	country := strings.ToUpper(holder.Country)
	if country == "" {
		country = "GB"
	}
	hp := &stripe.IssuingCardholderParams{
		Type: stripe.String("individual"),
		Name: stripe.String(holder.FirstName + " " + holder.LastName),
		Individual: &stripe.IssuingCardholderIndividualParams{
			FirstName: stripe.String(holder.FirstName),
			LastName:  stripe.String(holder.LastName),
		},
		Billing: &stripe.IssuingCardholderBillingParams{Address: &stripe.AddressParams{
			Line1: stripe.String(holder.Line1), City: stripe.String(holder.City),
			PostalCode: stripe.String(holder.PostalCode), Country: stripe.String(country),
		}},
	}
	if holder.Line2 != "" {
		hp.Billing.Address.Line2 = stripe.String(holder.Line2)
	}
	if holder.Email != "" {
		hp.Email = stripe.String(holder.Email)
	}
	phone := strings.TrimSpace(holder.Phone)
	if phone == "" {
		phone = testPhone
	}
	hp.PhoneNumber = stripe.String(phone)
	hp.Context = ctx
	hp.AddMetadata("workspace_id", workspaceID)
	holderObj, err := s.api.IssuingCardholders.New(hp)
	if err != nil {
		return economy.AgentCard{}, fmt.Errorf("agentcard: create cardholder: %w", err)
	}
	cp := &stripe.IssuingCardParams{
		Cardholder: stripe.String(holderObj.ID),
		Currency:   stripe.String(s.currency),
		Type:       stripe.String(string(stripe.IssuingCardTypeVirtual)),
		Status:     stripe.String(string(stripe.IssuingCardStatusActive)),
	}
	cp.Context = ctx
	cp.AddMetadata("workspace_id", workspaceID)
	cp.AddMetadata("agent_id", agentID)
	cp.AddMetadata("agent_name", agentName)
	card, err := s.api.IssuingCards.New(cp)
	if err != nil {
		return economy.AgentCard{}, fmt.Errorf("agentcard: issue card: %w", err)
	}
	return economy.AgentCard{
		ID: card.ID, AgentID: agentID, StripeCardholderID: holderObj.ID, Last4: card.Last4,
		ExpMonth: int(card.ExpMonth), ExpYear: int(card.ExpYear), Currency: string(card.Currency), Livemode: card.Livemode,
	}, nil
}

// Authorizer decides a card's authorisation request. *economy.DualTokenStore satisfies it.
type Authorizer interface {
	AuthorizeAgentCard(ctx context.Context, a economy.CardAuthorization) (economy.CardDecision, error)
}

// Pricer prices an amount in USD at the day's reference rate. *ecbrate.Book satisfies it.
type Pricer interface {
	ToUSD(ctx context.Context, amountMinor int64, currency string, at time.Time) (ecbrate.Conversion, error)
	Refresh(ctx context.Context) error
}

// Handler answers Stripe's real-time authorisation requests.
type Handler struct {
	secret string
	auth   Authorizer
	rates  Pricer
	log    *slog.Logger
}

// NewHandler verifies requests with the real-time endpoint's signing secret.
func NewHandler(secret string, auth Authorizer, rates Pricer) *Handler {
	return &Handler{secret: secret, auth: auth, rates: rates, log: slog.Default()}
}

// decisionBudget leaves Stripe's two seconds room for the network.
const decisionBudget = 1500 * time.Millisecond

// ServeHTTP answers an issuing_authorization.request with {"approved": …} and Stripe-Version, as Stripe
// requires; it acknowledges any other event. Whatever goes wrong after the signature is verified is a
// DECLINE, never an error status — Stripe would fall back to its own timeout setting, which may approve.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	event, err := webhook.ConstructEventWithOptions(raw, r.Header.Get("Stripe-Signature"), h.secret,
		webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}
	if event.Type != "issuing_authorization.request" {
		w.WriteHeader(http.StatusOK)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), decisionBudget)
	defer cancel()
	d, err := h.decide(ctx, &event)
	if err != nil {
		h.log.Error("agent card: authorisation declined on an error", "event", event.ID, "err", err.Error())
		d = economy.CardDecision{Reason: "Talyvor could not decide this request"}
	}
	w.Header().Set("Stripe-Version", stripe.APIVersion)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	meta := map[string]string{"talyvor_reason": truncate(d.Reason, 500)}
	if d.AgentID != "" {
		meta["talyvor_agent_id"] = d.AgentID
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"approved": d.Approved, "metadata": meta})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (h *Handler) decide(ctx context.Context, event *stripe.Event) (economy.CardDecision, error) {
	var auth stripe.IssuingAuthorization
	if err := json.Unmarshal(event.Data.Raw, &auth); err != nil {
		return economy.CardDecision{}, fmt.Errorf("authorisation: %w", err)
	}
	a := economy.CardAuthorization{
		EventID: event.ID, AuthorizationID: auth.ID, Livemode: event.Livemode || auth.Livemode,
		At: time.Unix(event.Created, 0).UTC(),
	}
	if auth.Card != nil {
		a.CardID = auth.Card.ID
	}
	if p := auth.PendingRequest; p != nil {
		a.AmountMinor, a.Currency = p.Amount, string(p.Currency)
		a.MerchantAmountMinor, a.MerchantCurrency = p.MerchantAmount, string(p.MerchantCurrency)
	} else {
		a.Refusal = "the request carries no pending amount"
	}
	if m := auth.MerchantData; m != nil {
		a.MerchantName, a.MerchantCategory, a.MerchantID = m.Name, m.Category, m.NetworkID
	}
	return h.Authorize(ctx, a)
}

// Authorize prices a request in USD at the day's ECB rate and lets the agent's rules and balance decide it —
// every request Stripe sends, and (B25.7) a test agent's purchase a tester makes without Stripe.
func (h *Handler) Authorize(ctx context.Context, a economy.CardAuthorization) (economy.CardDecision, error) {
	if a.Refusal == "" {
		c, err := h.rates.ToUSD(ctx, a.AmountMinor, a.Currency, a.At)
		if errors.Is(err, ecbrate.ErrNoRate) {
			// No rate stored yet for the day (a fresh deployment): fetch once, then price or decline.
			if rerr := h.rates.Refresh(ctx); rerr == nil {
				c, err = h.rates.ToUSD(ctx, a.AmountMinor, a.Currency, a.At)
			}
		}
		if err != nil {
			a.Refusal = "Talyvor could not price this purchase in US dollars: " + err.Error()
		} else {
			a.RateDate, a.ECBUSDPerEUR, a.ECBCurrencyPerEUR, a.USDMicros = c.RateDate, c.USDPerEUR, c.CurrencyPerEUR, c.USDMicros
		}
	}
	return h.auth.AuthorizeAgentCard(ctx, a)
}
