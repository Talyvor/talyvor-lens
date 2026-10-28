package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	stripe "github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/billing/meterevent"
	"github.com/stripe/stripe-go/v81/checkout/session"
	"github.com/stripe/stripe-go/v81/customer"
	"github.com/stripe/stripe-go/v81/invoiceitem"
	"github.com/stripe/stripe-go/v81/paymentintent"
	"github.com/stripe/stripe-go/v81/price"
	"github.com/stripe/stripe-go/v81/subscription"
	"github.com/stripe/stripe-go/v81/subscriptionitem"
	"github.com/stripe/stripe-go/v81/tax/calculation"
)

// LiveStripe is the production stripeAPI implementation. It is NOT exercised by
// unit tests (those inject a fake); the webhook/credit money path is what the
// real-PG tests cover. The secret key is set on the process-global stripe.Key.
type LiveStripe struct {
	successURL string
	cancelURL  string
	// key is the secret key this instance calls with. The checkout, customer, card and subscription calls
	// below pass it on each call, so a test-mode instance (B25.2) and the live one never share a key.
	key string
}

// NewLiveStripe configures the Stripe SDK with the secret key and the
// success/cancel redirect URLs Checkout requires.
func NewLiveStripe(secretKey, successURL, cancelURL string) *LiveStripe {
	stripe.Key = secretKey
	return &LiveStripe{successURL: successURL, cancelURL: cancelURL, key: secretKey}
}

// NewTestModeStripe is a second instance with a Stripe TEST-MODE key, for test (synthetic) workspaces
// (B25.2). It leaves the process-global key, which the live instance set, alone.
func NewTestModeStripe(secretKey, successURL, cancelURL string) *LiveStripe {
	return &LiveStripe{successURL: successURL, cancelURL: cancelURL, key: secretKey}
}

func (l *LiveStripe) backend() stripe.Backend { return stripe.GetBackend(stripe.APIBackend) }

// LiveKey reports whether key is a live-mode Stripe key, which moves real money.
func LiveKey(key string) bool {
	return strings.HasPrefix(key, "sk_live_") || strings.HasPrefix(key, "rk_live_")
}

// CardPayments says in public whether a real workspace's card can be charged (B51.2): "live" when billing is on
// with a live main key, "test" when it is on with a test-mode one (B50.13 refuses a real workspace's payment),
// "off" when billing is off. It says nothing else about the key.
func CardPayments(billingEnabled bool, mainKey string) string {
	switch {
	case !billingEnabled:
		return "off"
	case LiveKey(mainKey):
		return "live"
	default:
		return "test"
	}
}

// Livemode reports whether this instance's key is live (B22.1: a live key pays out only live earnings). Its own
// key, never the process-wide one: a test-mode instance beside a live one is test mode (B25.6).
func (l *LiveStripe) Livemode() bool { return LiveKey(l.key) }

// CreateCustomer creates a Stripe customer tagged with the workspace id.
func (l *LiveStripe) CreateCustomer(ctx context.Context, workspaceID string) (string, error) {
	params := &stripe.CustomerParams{}
	params.Context = ctx
	params.AddMetadata("workspace_id", workspaceID)
	c, err := customer.Client{B: l.backend(), Key: l.key}.New(params)
	if err != nil {
		return "", err
	}
	return c.ID, nil
}

// CustomerCountries is the country of a customer's billing address and of the card that is its default payment
// method, in one read — the evidence of where a buyer is (B32.38). Each is "" where the customer has none.
func (l *LiveStripe) CustomerCountries(ctx context.Context, customerID string) (billingCountry, cardCountry string, err error) {
	params := &stripe.CustomerParams{}
	params.Context = ctx
	params.AddExpand("invoice_settings.default_payment_method")
	c, err := customer.Client{B: l.backend(), Key: l.key}.Get(customerID, params)
	if err != nil {
		return "", "", err
	}
	if c.Address != nil {
		billingCountry = c.Address.Country
	}
	if c.InvoiceSettings != nil && c.InvoiceSettings.DefaultPaymentMethod != nil && c.InvoiceSettings.DefaultPaymentMethod.Card != nil {
		cardCountry = c.InvoiceSettings.DefaultPaymentMethod.Card.Country
	}
	return billingCountry, cardCountry, nil
}

// CalculateTax asks Stripe Tax for the tax on a sale (B32.45), with this instance's key: partners.StripeTaxPartner's API.
func (l *LiveStripe) CalculateTax(ctx context.Context, params *stripe.TaxCalculationParams) (*stripe.TaxCalculation, error) {
	params.Context = ctx
	return calculation.Client{B: l.backend(), Key: l.key}.New(params)
}

// CreateCheckoutSession creates a one-off (mode=payment) Checkout Session for a
// single USD line item, stamping workspace_id / lxc_amount / usd_cents into the
// session metadata the webhook later re-verifies (never trusts as truth).
func (l *LiveStripe) CreateCheckoutSession(ctx context.Context, p CheckoutParams) (string, string, error) {
	params := &stripe.CheckoutSessionParams{
		Mode:       stripe.String(string(stripe.CheckoutSessionModePayment)),
		Customer:   stripe.String(p.CustomerID),
		SuccessURL: stripe.String(l.successURL),
		CancelURL:  stripe.String(l.cancelURL),
		LineItems: []*stripe.CheckoutSessionLineItemParams{{
			PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
				Currency:   stripe.String("usd"),
				UnitAmount: stripe.Int64(p.USDCents),
				ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
					Name: stripe.String("LXC usage credit top-up"),
				},
			},
			Quantity: stripe.Int64(1),
		}},
		// B22.2: the customer pays in their own currency and Stripe converts it at its rate (the customer pays
		// its conversion fee); the credits are bought in USD, and Lens converts nothing.
		AdaptivePricing: &stripe.CheckoutSessionAdaptivePricingParams{Enabled: stripe.Bool(true)},
	}
	params.Context = ctx
	params.AddMetadata("workspace_id", p.WorkspaceID)
	params.AddMetadata("lxc_amount", strconv.FormatInt(p.LXCAmount, 10)) // µLXC (SEC-2)
	params.AddMetadata("usd_cents", strconv.FormatInt(p.USDCents, 10))

	sess, err := session.Client{B: l.backend(), Key: l.key}.New(params)
	if err != nil {
		return "", "", err
	}
	return sess.URL, sess.ID, nil
}

// CardFingerprint retrieves the payment intent with its payment method expanded
// and returns the card's stable per-card fingerprint (Stripe's card.fingerprint).
// Returns "" (not an error) when there is no expandable card — the caller treats
// "" as capture-failed and swallows it. U6 PR2 owner-linkage.
func (l *LiveStripe) CardFingerprint(ctx context.Context, paymentIntentID string) (string, error) {
	params := &stripe.PaymentIntentParams{}
	params.Context = ctx
	params.AddExpand("payment_method")
	pi, err := paymentintent.Client{B: l.backend(), Key: l.key}.Get(paymentIntentID, params)
	if err != nil {
		return "", err
	}
	if pi.PaymentMethod == nil || pi.PaymentMethod.Card == nil {
		return "", nil
	}
	return pi.PaymentMethod.Card.Fingerprint, nil
}

// CreateSubscriptionCheckoutSession creates a mode=subscription Checkout Session for
// the configured Price. W4.6.1 step 1.
//
// ⚠ THE METADATA IS STAMPED ON THE SUBSCRIPTION, NOT ONLY ON THE SESSION, and that is
// the difference between this and CreateCheckoutSession above. A one-off webhook reads
// `checkout.session.completed`, which carries the session's own metadata. The
// RECURRING events — customer.subscription.updated a month later, and every renewal
// after that — are about the SUBSCRIPTION object and never see the session again. So
// `workspace_id` goes in SubscriptionData.Metadata, which Stripe copies onto the
// subscription and replays on every future event. Without it, the second month's
// event arrives with no way to attribute it, and handleSubscription's
// "no workspace on file" branch is what a renewal would hit forever.
func (l *LiveStripe) CreateSubscriptionCheckoutSession(ctx context.Context, p SubscriptionParams) (string, string, error) {
	params := &stripe.CheckoutSessionParams{
		Mode:       stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		Customer:   stripe.String(p.CustomerID),
		SuccessURL: stripe.String(l.successURL),
		CancelURL:  stripe.String(l.cancelURL),
		LineItems: []*stripe.CheckoutSessionLineItemParams{{
			Price:    stripe.String(p.PriceID),
			Quantity: stripe.Int64(1),
		}},
		SubscriptionData: &stripe.CheckoutSessionSubscriptionDataParams{},
	}
	params.Context = ctx
	params.AddMetadata("workspace_id", p.WorkspaceID)
	params.SubscriptionData.AddMetadata("workspace_id", p.WorkspaceID)
	sess, err := session.Client{B: l.backend(), Key: l.key}.New(params)
	if err != nil {
		return "", "", err
	}
	return sess.URL, sess.ID, nil
}

// PlanLookupKeys are the Stripe lookup keys B13.1 gave the plan Prices (talyvor-lens #548), by plan — and
// BYOK's (B27.26), Team's and Business's (B32.10).
var PlanLookupKeys = map[string]string{"plus": "talyvor_plus_monthly", "pro": "talyvor_pro_monthly", "max": "talyvor_max_monthly",
	BYOKPlan: BYOKLookupKey, TeamPlan: TeamLookupKey, BusinessPlan: BusinessLookupKey}

// Team and Business, the company plans, per workspace: Nicolai's decision of 5 Oct 2026 — Team $49 and
// Business $299 a month; BYOK is Team's add-on and is included in Business; Enterprise is contracted and
// invoiced by the operator, from $2,500 a month, and is never a Stripe Price. Like BYOKUSDCents, the amounts
// are used only to create the TEST-MODE Prices when the account has none (EnsurePlanPrices); the live Prices
// are created in Stripe, with these lookup keys.
const (
	TeamPlan          = "team"
	TeamLookupKey     = "talyvor_team_monthly"
	TeamUSDCents      = 4900
	BusinessPlan      = "business"
	BusinessLookupKey = "talyvor_business_monthly"
	BusinessUSDCents  = 29900
	EnterprisePlan    = "enterprise"
	FreePlan          = "free"
)

// BYOK, the one subscription tier (B27.26): a workspace brings its own provider keys and pays the platform
// fee instead of tokens. BYOKUSDCents is Nicolai's decision of 4 Oct 2026 — $199 a month — and is used only
// to create the TEST-MODE Price when the account has none (EnsureBYOKPrice); a live Price is created in Stripe.
const (
	BYOKPlan      = "byok"
	BYOKLookupKey = "talyvor_byok_monthly"
	BYOKUSDCents  = 19900
)

// EnsureBYOKPrice creates the BYOK Price — $199 a month, lookup key talyvor_byok_monthly — in this key's
// Stripe account when it has no active one. TEST-MODE keys only: a live key is refused, never billed from code.
func (l *LiveStripe) EnsureBYOKPrice(ctx context.Context) error {
	if LiveKey(l.key) {
		return fmt.Errorf("billing: the BYOK Price is created from code only in Stripe test mode")
	}
	return l.ensureMonthlyPrice(ctx, BYOKLookupKey, BYOKUSDCents, "Talyvor BYOK")
}

// EnsurePlanPrices creates the Team Price ($49 a month, talyvor_team_monthly) and the Business Price ($299 a
// month, talyvor_business_monthly) in this key's Stripe account, each only when it has no active one (B32.10).
// TEST-MODE keys only: a live key is refused before Stripe is called.
func (l *LiveStripe) EnsurePlanPrices(ctx context.Context) error {
	if LiveKey(l.key) {
		return fmt.Errorf("billing: the Team and Business Prices are created from code only in Stripe test mode")
	}
	if err := l.ensureMonthlyPrice(ctx, TeamLookupKey, TeamUSDCents, "Talyvor Team"); err != nil {
		return err
	}
	return l.ensureMonthlyPrice(ctx, BusinessLookupKey, BusinessUSDCents, "Talyvor Business")
}

// ensureMonthlyPrice creates a monthly USD Price of cents under lookupKey, with a product of that name, unless
// the account already has an active Price with that lookup key.
func (l *LiveStripe) ensureMonthlyPrice(ctx context.Context, lookupKey string, cents int64, product string) error {
	list := &stripe.PriceListParams{Active: stripe.Bool(true), LookupKeys: []*string{stripe.String(lookupKey)}}
	list.Context = ctx
	it := price.Client{B: l.backend(), Key: l.key}.List(list)
	for it.Next() {
		if it.Price().LookupKey == lookupKey {
			return nil
		}
	}
	if err := it.Err(); err != nil {
		return err
	}
	params := &stripe.PriceParams{
		Currency:    stripe.String(string(stripe.CurrencyUSD)),
		UnitAmount:  stripe.Int64(cents),
		Recurring:   &stripe.PriceRecurringParams{Interval: stripe.String(string(stripe.PriceRecurringIntervalMonth))},
		LookupKey:   stripe.String(lookupKey),
		ProductData: &stripe.PriceProductDataParams{Name: stripe.String(product)},
	}
	params.Context = ctx
	_, err := price.Client{B: l.backend(), Key: l.key}.New(params)
	return err
}

// PlanPrices returns the plans this key's Stripe account holds under PlanLookupKeys: plan → the id of the active
// Price with that lookup key (B17.21). Only the ids — every amount stays in Stripe.
func (l *LiveStripe) PlanPrices(ctx context.Context) (map[string]string, error) {
	params := &stripe.PriceListParams{Active: stripe.Bool(true)}
	params.Context = ctx
	byKey := map[string]string{}
	for plan, key := range PlanLookupKeys {
		byKey[key] = plan
		params.LookupKeys = append(params.LookupKeys, stripe.String(key))
	}
	plans := map[string]string{}
	it := price.Client{B: l.backend(), Key: l.key}.List(params)
	for it.Next() {
		if plan, ok := byKey[it.Price().LookupKey]; ok {
			plans[plan] = it.Price().ID
		}
	}
	return plans, it.Err()
}

// PriceUSDCents is what one unit of a Price bills, in US cents (B28.439) — the fee a subscriber to it is
// granted their included usage for (feeOf). 0 for a Price that is not USD or has no fixed amount.
func (l *LiveStripe) PriceUSDCents(ctx context.Context, priceID string) (int64, error) {
	params := &stripe.PriceParams{}
	params.Context = ctx
	p, err := price.Client{B: l.backend(), Key: l.key}.Get(priceID, params)
	if err != nil {
		return 0, err
	}
	if p.Currency != stripe.CurrencyUSD || p.UnitAmount <= 0 {
		return 0, nil
	}
	return p.UnitAmount, nil
}

// SubscriptionJSON is the subscription as Stripe serialises it (B17.21): what customer.subscription.created
// carries, for a completed subscription checkout to record it the same way.
func (l *LiveStripe) SubscriptionJSON(ctx context.Context, subscriptionID string) (json.RawMessage, error) {
	params := &stripe.SubscriptionParams{}
	params.Context = ctx
	sub, err := subscription.Client{B: l.backend(), Key: l.key}.Get(subscriptionID, params)
	if err != nil {
		return nil, err
	}
	return sub.LastResponse.RawJSON, nil
}

// SetCancelAtPeriodEnd flips the subscription's cancel_at_period_end flag. B1.5.
// Stripe answers with the updated subscription and then sends the matching
// customer.subscription.updated to the webhook, which is what records it.
func (l *LiveStripe) SetCancelAtPeriodEnd(ctx context.Context, subscriptionID string, cancel bool) (*stripe.Subscription, error) {
	params := &stripe.SubscriptionParams{CancelAtPeriodEnd: stripe.Bool(cancel)}
	params.Context = ctx
	return subscription.Client{B: l.backend(), Key: l.key}.Update(subscriptionID, params)
}

// ChangeSubscriptionPrice moves a subscription's one item to priceID with proration (B18.14): Stripe credits
// the unused time on the old price and charges the rest of the period on the new one, on the next invoice.
// Stripe answers with the updated subscription and sends customer.subscription.updated, which records it.
func (l *LiveStripe) ChangeSubscriptionPrice(ctx context.Context, subscriptionID, priceID string) (*stripe.Subscription, error) {
	get := &stripe.SubscriptionParams{}
	get.Context = ctx
	cur, err := subscription.Client{B: l.backend(), Key: l.key}.Get(subscriptionID, get)
	if err != nil {
		return nil, err
	}
	// B32.10: the plan's item, never Team's BYOK add-on — and a move to Business, which includes BYOK, drops
	// the add-on in the same update, so it is never billed twice.
	var plan *stripe.SubscriptionItem
	var addons []*stripe.SubscriptionItem
	if cur.Items != nil {
		for _, it := range cur.Items.Data {
			switch {
			case it == nil:
			case it.Price != nil && it.Price.LookupKey == BYOKLookupKey:
				addons = append(addons, it)
			case plan == nil:
				plan = it
			}
		}
	}
	if plan == nil {
		return nil, fmt.Errorf("subscription %s has no item to change", subscriptionID)
	}
	params := &stripe.SubscriptionParams{
		Items:             []*stripe.SubscriptionItemsParams{{ID: stripe.String(plan.ID), Price: stripe.String(priceID)}},
		ProrationBehavior: stripe.String("create_prorations"),
	}
	if len(addons) > 0 {
		get := &stripe.PriceParams{}
		get.Context = ctx
		to, err := price.Client{B: l.backend(), Key: l.key}.Get(priceID, get)
		if err != nil {
			return nil, err
		}
		if to.LookupKey == BusinessLookupKey {
			for _, it := range addons {
				params.Items = append(params.Items, &stripe.SubscriptionItemsParams{ID: stripe.String(it.ID), Deleted: stripe.Bool(true)})
			}
		}
	}
	params.Context = ctx
	return subscription.Client{B: l.backend(), Key: l.key}.Update(subscriptionID, params)
}

// AddSubscriptionItem adds priceID to the subscription as a further item, with proration (B32.10: Team's BYOK
// add-on), idempotent on idempotencyKey. Stripe sends customer.subscription.updated, which records it.
func (l *LiveStripe) AddSubscriptionItem(ctx context.Context, subscriptionID, priceID, idempotencyKey string) (*stripe.Subscription, error) {
	params := &stripe.SubscriptionParams{
		Items:             []*stripe.SubscriptionItemsParams{{Price: stripe.String(priceID)}},
		ProrationBehavior: stripe.String("create_prorations"),
	}
	params.Context = ctx
	params.SetIdempotencyKey(idempotencyKey)
	return subscription.Client{B: l.backend(), Key: l.key}.Update(subscriptionID, params)
}

// RemoveSubscriptionItem removes the subscription's items billed at priceID, with proration (B32.10).
func (l *LiveStripe) RemoveSubscriptionItem(ctx context.Context, subscriptionID, priceID string) (*stripe.Subscription, error) {
	get := &stripe.SubscriptionParams{}
	get.Context = ctx
	cur, err := subscription.Client{B: l.backend(), Key: l.key}.Get(subscriptionID, get)
	if err != nil {
		return nil, err
	}
	params := &stripe.SubscriptionParams{ProrationBehavior: stripe.String("create_prorations")}
	if cur.Items != nil {
		for _, it := range cur.Items.Data {
			if it != nil && it.Price != nil && it.Price.ID == priceID {
				params.Items = append(params.Items, &stripe.SubscriptionItemsParams{ID: stripe.String(it.ID), Deleted: stripe.Bool(true)})
			}
		}
	}
	if len(params.Items) == 0 {
		return cur, nil
	}
	params.Context = ctx
	return subscription.Client{B: l.backend(), Key: l.key}.Update(subscriptionID, params)
}

// CreditInvoice adds a NEGATIVE line of amountCents to a draft invoice (B13.2), idempotent on
// idempotencyKey so a retried webhook cannot add a second one.
func (l *LiveStripe) CreditInvoice(ctx context.Context, customerID, invoiceID string, amountCents int64, description, idempotencyKey string) error {
	params := &stripe.InvoiceItemParams{
		Customer:    stripe.String(customerID),
		Invoice:     stripe.String(invoiceID),
		Amount:      stripe.Int64(-amountCents),
		Currency:    stripe.String(string(stripe.CurrencyUSD)),
		Description: stripe.String(description),
	}
	params.Context = ctx
	params.SetIdempotencyKey(idempotencyKey)
	_, err := invoiceitem.New(params)
	return err
}

// CreateMarketSubscription subscribes customerID to the metered marketplace price (B20.2). The workspace is
// named as market_workspace_id — never workspace_id, which would make it the workspace's plan — and the
// idempotency key makes two first uses at once one subscription.
func (l *LiveStripe) CreateMarketSubscription(ctx context.Context, customerID, priceID, workspaceID string) (string, error) {
	params := &stripe.SubscriptionParams{
		Customer: stripe.String(customerID),
		Items:    []*stripe.SubscriptionItemsParams{{Price: stripe.String(priceID)}},
	}
	params.Context = ctx
	params.AddMetadata("market_workspace_id", workspaceID)
	params.SetIdempotencyKey("market-bill-" + workspaceID)
	sub, err := subscription.Client{B: l.backend(), Key: l.key}.New(params)
	if err != nil {
		return "", err
	}
	return sub.ID, nil
}

// AddMarketSubscriptionItem adds the metered tax price to a marketplace subscription (B32.39), so the tax meter's
// events are invoiced as their own line. Idempotent on the subscription and the price.
func (l *LiveStripe) AddMarketSubscriptionItem(ctx context.Context, subscriptionID, priceID string) error {
	params := &stripe.SubscriptionItemParams{
		Subscription:      stripe.String(subscriptionID),
		Price:             stripe.String(priceID),
		ProrationBehavior: stripe.String("none"),
	}
	params.Context = ctx
	params.SetIdempotencyKey("market-bill-tax-" + subscriptionID + "-" + priceID)
	_, err := subscriptionitem.Client{B: l.backend(), Key: l.key}.New(params)
	return err
}

// CreditMarketUse puts a NEGATIVE line of cents (a decimal: a use's price may be a fraction of a cent) on
// the customer's next invoice of the marketplace subscription (B20.4), idempotent on idempotencyKey.
func (l *LiveStripe) CreditMarketUse(ctx context.Context, customerID, subscriptionID string, cents float64, description, idempotencyKey string) (string, error) {
	params := &stripe.InvoiceItemParams{
		Customer:          stripe.String(customerID),
		Subscription:      stripe.String(subscriptionID),
		Currency:          stripe.String(string(stripe.CurrencyUSD)),
		UnitAmountDecimal: stripe.Float64(-cents),
		Quantity:          stripe.Int64(1),
		Description:       stripe.String(description),
	}
	params.Context = ctx
	params.SetIdempotencyKey(idempotencyKey)
	item, err := invoiceitem.Client{B: l.backend(), Key: l.key}.New(params)
	if err != nil {
		return "", err
	}
	return item.ID, nil
}

// SendMeterEvent records one marketplace use of value µLXC for customerID (B20.2). Stripe keeps one event
// per identifier, so a retried use is billed once.
func (l *LiveStripe) SendMeterEvent(ctx context.Context, eventName, customerID, identifier string, value int64, at time.Time) error {
	params := &stripe.BillingMeterEventParams{
		EventName:  stripe.String(eventName),
		Identifier: stripe.String(identifier),
		Payload:    map[string]string{"stripe_customer_id": customerID, "value": strconv.FormatInt(value, 10)},
		Timestamp:  stripe.Int64(at.Unix()),
	}
	params.Context = ctx
	_, err := meterevent.Client{B: l.backend(), Key: l.key}.New(params)
	return err
}
