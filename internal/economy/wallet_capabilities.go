package economy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/text/language"

	"github.com/talyvor/lens/internal/plans"
)

// wallet_capabilities.go — B22.1: EVERY WALLET CAPABILITY CARRIES ITS CLASS, AND REAL MONEY OBEYS IT.
//
//	GREEN  real money now.
//	AMBER  test money; real money only once the lawyer confirms it.
//	RED    test money only, until a licence or a licensed partner exists.
//
// An AMBER or RED capability takes only test-funded money until the operator records a clearance for it
// (wallet_clearances, migration 0158: who, when, the lawyer's or partner's reference; 0194, B30.1: the licence,
// the licensed partner, the countries it covers and its expiry), and revoking the clearance stops live money
// again from the next use. A clearance past its expiry refuses live money exactly like no clearance, and so does a
// clearance used by an owner verified in a country it does not list (B30.10: the owner's country is the one their
// verification confirmed), naming that country. A GREEN capability takes any.
//
// Money reaches a capability two ways, and both are judged:
//
//   - credits: every credit lot carries its funding on its lxc_ledger row (metadata.funding: test, live,
//     grant or synthetic), and the test-funded part of a workspace's balance is lxc_balances.test_funded_ulxc.
//     An uncleared AMBER or RED spend takes it from there, or is refused. Every other spend takes the credits
//     that are not test-funded first (writeLXCBalance), so nothing but a gated spend uses test-funded credits
//     up while others remain.
//   - a Stripe bill (a payment to another company's agent is on the company's marketplace bill, B19.15): that
//     money is live when Lens's Stripe key is live (SetLiveStripe).
//
// Going live with Stripe therefore needs no code change to stay safe: a live key makes every new purchase
// live-funded and every bill live, and neither reaches an AMBER or RED capability without a clearance.

// CapabilityClass is GREEN, AMBER or RED.
type CapabilityClass string

// The classes.
const (
	ClassGreen CapabilityClass = "GREEN"
	ClassAmber CapabilityClass = "AMBER"
	ClassRed   CapabilityClass = "RED"
)

// Capability is one thing a wallet can do, and the verification level its live money needs (B30.4).
type Capability struct {
	Key   string            `json:"capability"`
	Name  string            `json:"name"`
	Class CapabilityClass   `json:"class"`
	Level VerificationLevel `json:"level_needed"`
}

// The capabilities whose class code enforces today.
const (
	CapabilityPayAnotherOwner = "pay_another_owner" // AMBER: agents of different owners pay each other
	CapabilityAgentCard       = "agent_card"        // RED
	// GREEN, both on the buyer's monthly marketplace bill (B32.19): asked where a licence's charge is recorded.
	CapabilityBuyListings      = "buy_marketplace_listings"
	CapabilityRentAndSubscribe = "rent_and_subscribe_listings"
	// GREEN (B32.26): asked where a marketplace sale's pool flows up to the originals of a remix.
	CapabilityLineageRoyalties = "lineage_royalties"
)

// Capabilities is every wallet capability and its class, as Nicolai decided them on 28 Sep 2026, and the verification
// level its live money needs (B30.4): currency accounts, payments, FX and trading L2; credit and merchant acceptance
// L3; spending on Talyvor itself and moving money inside one owner's own wallets L0.
var Capabilities = []Capability{
	{"spend_on_talyvor", "Spending on Talyvor", ClassGreen, LevelSignedIn},
	{CapabilityBuyListings, "Buying marketplace listings", ClassGreen, LevelSignedIn},
	{CapabilityRentAndSubscribe, "Renting and subscribing to marketplace listings", ClassGreen, LevelSignedIn},
	{CapabilityLineageRoyalties, "Royalties to the authors of remixed listings, paid from marketplace sales", ClassGreen, LevelSignedIn},
	{"move_between_own_agents", "Moving money between one owner's own agents", ClassGreen, LevelSignedIn},
	{"rules_approvals_statements_pots", "Rules, approvals, statements and pots", ClassGreen, LevelSignedIn},
	{"company_credit_line", "Talyvor's credit line to companies, for Talyvor services", ClassGreen, LevelSignedIn},
	{CapabilityPayAnotherOwner, "Sending and requesting money between different owners", ClassAmber, LevelIdentity},
	{"loans_between_companies", "Loans between companies", ClassAmber, LevelCompany},
	{"escrow", "Escrow between agents", ClassAmber, LevelIdentity},
	{"cash_out", "Cashing credits out as money", ClassRed, LevelIdentity},
	{"credit_involving_a_person", "Any loan or credit involving a private user", ClassRed, LevelCompany},
	{"interest_and_yield", "Interest or yield", ClassRed, LevelIdentity},
	{"invest_and_trade", "Investing and trading real assets", ClassRed, LevelIdentity},
	{CapabilityAgentCard, "Cards", ClassRed, LevelIdentity},

	// B30.1 — money and markets for AI agents (Nicolai, 5 Oct 2026: build everything now; real money only once
	// licensed). invest_and_trade above stays, for simulated trading.
	{CapabilityCurrencyAccounts, "Accounts in pounds, euros, dollars and USDC", ClassRed, LevelIdentity},
	{CapabilityAccountDetails, "Account details others can pay into", ClassRed, LevelIdentity},
	{CapabilityPaymentsIn, "Receiving money from outside Talyvor", ClassRed, LevelIdentity},
	{CapabilityPaymentsOut, "Paying people and companies outside Talyvor", ClassRed, LevelIdentity},
	{CapabilityPayByBank, "Topping up by a payment from your own bank", ClassAmber, LevelIdentity},
	{CapabilityFX, "Converting between currencies", ClassRed, LevelIdentity},
	{CapabilityStablecoins, "Stablecoin balances and transfers", ClassRed, LevelIdentity},
	{CapabilityX402, "Paying and being paid over HTTP 402", ClassRed, LevelIdentity},
	{CapabilityMerchantAcceptance, "Accepting payments from agents as a business", ClassRed, LevelCompany},
	{CapabilityB2BCredit, "Credit lines and loans to companies", ClassAmber, LevelCompany},
	{CapabilitySellerAdvances, "Advances against marketplace earnings", ClassAmber, LevelCompany},
	{CapabilityLendingMarketplace, "Companies lending to companies through the marketplace", ClassAmber, LevelCompany},
	{CapabilityTradeEquities, "Trading shares through a broker partner", ClassRed, LevelIdentity},
	{CapabilityTradeCrypto, "Trading crypto through a broker partner", ClassRed, LevelIdentity},
	{CapabilityTradePrediction, "Prediction-market trading", ClassRed, LevelIdentity},
	{CapabilityTreasurySweep, "Idle money in a money-market fund", ClassRed, LevelIdentity},
	{CapabilityPriceLock, "Prepaid usage at today's prices", ClassAmber, LevelIdentity},
	{CapabilityCover, "Cover for agent mistakes", ClassRed, LevelIdentity},
	{CapabilityPayoutsToPeople, "Paying people for tasks", ClassRed, LevelIdentity},
}

// The B30 capabilities. Each is asked at the point its money moves.
const (
	CapabilityCurrencyAccounts   = "currency_accounts"
	CapabilityAccountDetails     = "account_details"
	CapabilityPaymentsIn         = "payments_in"
	CapabilityPaymentsOut        = "payments_out"
	CapabilityPayByBank          = "pay_by_bank"
	CapabilityFX                 = "fx"
	CapabilityStablecoins        = "stablecoins"
	CapabilityX402               = "x402"
	CapabilityMerchantAcceptance = "merchant_acceptance"
	CapabilityB2BCredit          = "b2b_credit"
	CapabilitySellerAdvances     = "seller_advances"
	CapabilityLendingMarketplace = "lending_marketplace"
	CapabilityTradeEquities      = "trade_equities"
	CapabilityTradeCrypto        = "trade_crypto"
	CapabilityTradePrediction    = "trade_prediction"
	CapabilityTreasurySweep      = "treasury_sweep"
	CapabilityPriceLock          = "price_lock"
	CapabilityCover              = "cover"
	CapabilityPayoutsToPeople    = "payouts_to_people"
)

// b30 is every capability B30 registers: on a plan without live money each takes test money only, whatever its
// class and even with a clearance (B32.12).
var b30 = map[string]bool{CapabilityCurrencyAccounts: true, CapabilityAccountDetails: true, CapabilityPaymentsIn: true,
	CapabilityPaymentsOut: true, CapabilityPayByBank: true, CapabilityFX: true, CapabilityStablecoins: true,
	CapabilityX402: true, CapabilityMerchantAcceptance: true, CapabilityB2BCredit: true, CapabilitySellerAdvances: true,
	CapabilityLendingMarketplace: true, CapabilityTradeEquities: true, CapabilityTradeCrypto: true,
	CapabilityTradePrediction: true, CapabilityTreasurySweep: true, CapabilityPriceLock: true, CapabilityCover: true,
	CapabilityPayoutsToPeople: true}

// CapabilityByKey finds a capability.
func CapabilityByKey(key string) (Capability, bool) {
	for _, c := range Capabilities {
		if c.Key == key {
			return c, true
		}
	}
	return Capability{}, false
}

// Funding says what paid for a credit lot, on its lxc_ledger row's metadata.
const (
	FundingTest      = "test"      // bought in Stripe test mode: test money forever
	FundingLive      = "live"      // bought with real money
	FundingGrant     = "grant"     // comped by the operator
	FundingSynthetic = "synthetic" // made inside Talyvor (LENS converted to LXC)
)

// lotFunding stamps a new credit lot's funding on its ledger metadata (a copy) and says how much of it is
// test-funded. A grant is FundingGrant. A purchase's caller says: funding test or live, or, for a lot of
// both (marketplace earnings taken as credits), test_funded_ulxc; a purchase that says nothing is live, the
// safe reading — live credits are the ones an uncleared capability refuses.
func lotFunding(ledgerType string, amount int64, metadata map[string]interface{}) (map[string]interface{}, int64) {
	out := make(map[string]interface{}, len(metadata)+1)
	for k, v := range metadata {
		out[k] = v
	}
	if ledgerType == LXCTypeGrant {
		out["funding"] = FundingGrant
		return out, 0
	}
	if part, ok := out["test_funded_ulxc"].(int64); ok {
		return out, min(max(part, 0), amount)
	}
	if out["funding"] == FundingTest {
		return out, amount
	}
	if _, ok := out["funding"]; !ok {
		out["funding"] = FundingLive
	}
	return out, 0
}

// ErrCapabilityNotCleared: live money for an AMBER or RED capability that has no clearance.
var ErrCapabilityNotCleared = errors.New("economy: this capability takes no real money until Talyvor records a clearance")

// CapabilityRefusal says which capability refused live money, and its class — or, when the capability is
// cleared but the workspace's plan keeps it on test money (B32.12), the plan's refusal; or, when the workspace's
// verification keeps it out (B30.4), the level it needs, or the limit it is over; or, while an operator has the
// workspace frozen (B30.8), the freeze; or, until the workspace accepts the capability's latest terms (B30.9), their
// version; or, when the capability is cleared but not for the owner's country (B30.10), that country.
type CapabilityRefusal struct {
	Capability Capability
	Plan       *plans.Refusal
	Level      *LevelRefusal
	Freeze     *Freeze
	Terms      *TermsNeeded
	Country    *CountryRefusal
}

// CountryRefusal is a clearance that does not reach the owner's country: the country the owner is verified in ("" when
// no check confirmed one) and the countries the clearance lists.
type CountryRefusal struct {
	Country   string   `json:"country"`
	Countries []string `json:"countries"`
}

func (r *CountryRefusal) message(c Capability) string {
	where := fmt.Sprintf("%s is class %s and takes real money only for owners verified in %s", c.Name, c.Class,
		strings.Join(r.Countries, ", "))
	if r.Country == "" {
		return where + "; this workspace has no verified country yet: its identity or company check confirms one"
	}
	return fmt.Sprintf("%s; this workspace is verified in %s", where, r.Country)
}

func (e *CapabilityRefusal) Error() string {
	switch {
	case e.Freeze != nil:
		// The workspace is told it is frozen and on which case, never the operator's reason.
		return fmt.Sprintf("%s is refused: an operator has frozen this workspace's money capabilities on compliance case %s. "+
			"Talyvor's own services still work", e.Capability.Name, e.Freeze.CaseID)
	case e.Terms != nil:
		return fmt.Sprintf("%s needs its terms accepted before it is used: this workspace has not accepted version %d of them. "+
			"Read them and accept them, then try again", e.Capability.Name, e.Terms.Version)
	case e.Plan != nil:
		return e.Plan.Error()
	case e.Level != nil:
		return e.Level.message(e.Capability)
	case e.Country != nil:
		return e.Country.message(e.Capability)
	}
	return fmt.Sprintf("%s is class %s: it takes no real money until Talyvor records a clearance for it, and this would use real money",
		e.Capability.Name, e.Capability.Class)
}

// Is makes a refusal an ErrCapabilityNotCleared, a verification level's an ErrVerificationNeeded too, a freeze's
// an ErrWorkspaceFrozen, and unaccepted terms' an ErrTermsNotAccepted.
func (e *CapabilityRefusal) Is(target error) bool {
	return target == ErrCapabilityNotCleared || (target == ErrVerificationNeeded && e.Level != nil) ||
		(target == ErrWorkspaceFrozen && e.Freeze != nil) || (target == ErrTermsNotAccepted && e.Terms != nil)
}

// Unwrap is the plan's refusal, when the plan refused.
func (e *CapabilityRefusal) Unwrap() error {
	if e.Plan == nil {
		return nil
	}
	return e.Plan
}

// ClearanceTerms is what a clearance rests on and where it reaches: the lawyer's or partner's reference, the
// licence, the licensed partner, the countries live money may be used from, and when it ends.
type ClearanceTerms struct {
	Reference string    `json:"reference"`
	Licence   string    `json:"licence_reference"`
	Partner   string    `json:"partner"`
	Countries []string  `json:"countries"` // ISO 3166-1 alpha-2
	ExpiresAt time.Time `json:"expires_at"`
}

// Clearance is the operator's record that a capability may take real money.
type Clearance struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
	ClearanceTerms
}

// CapabilityStatus is a capability, its class, and whether it takes real money now.
type CapabilityStatus struct {
	Capability
	RealMoney bool       `json:"real_money"`          // GREEN, or cleared — then only in the countries the clearance lists
	Clearance *Clearance `json:"clearance,omitempty"` // the clearance in force, for an AMBER or RED one
}

type useCountryKey struct{}

// WithUseCountry says which country a use of a capability comes from (ISO 3166-1 alpha-2). A clearance lets live
// money through only for a country it lists; a use from no known country is listed by none.
func WithUseCountry(ctx context.Context, country string) context.Context {
	return context.WithValue(ctx, useCountryKey{}, strings.ToUpper(strings.TrimSpace(country)))
}

func useCountry(ctx context.Context) string {
	c, _ := ctx.Value(useCountryKey{}).(string)
	return c
}

// countryCodes checks a clearance's countries are ISO 3166-1 alpha-2 country codes, and returns them upper-case,
// each once.
func countryCodes(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, c := range in {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		r, err := language.ParseRegion(c)
		if len(c) != 2 || err != nil || !r.IsCountry() || r.String() != c {
			return nil, fmt.Errorf("economy: %q is not an ISO 3166-1 alpha-2 country code", c)
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("economy: a clearance names the countries it covers (ISO 3166-1 alpha-2, e.g. GB)")
	}
	return out, nil
}

// SetLiveStripe tells the store whether Lens's Stripe key is live, which makes a Stripe bill real money.
func (s *DualTokenStore) SetLiveStripe(live bool) { s.liveStripe = live }

// WalletCapabilities lists every capability, its class and whether it takes real money now.
func (s *DualTokenStore) WalletCapabilities(ctx context.Context) ([]CapabilityStatus, error) {
	cleared, err := clearancesInForce(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	out := make([]CapabilityStatus, 0, len(Capabilities))
	for _, c := range Capabilities {
		st := CapabilityStatus{Capability: c, RealMoney: c.Class == ClassGreen}
		if cl, ok := cleared[c.Key]; ok && c.Class != ClassGreen {
			st.RealMoney, st.Clearance = true, &cl
		}
		out = append(out, st)
	}
	return out, nil
}

// ClearCapability records that the AMBER or RED capability may take real money, on the lawyer's or partner's
// reference, under the licence and through the partner the terms name, from the countries they list, until they
// expire.
func (s *DualTokenStore) ClearCapability(ctx context.Context, key, operator string, terms ClearanceTerms) (Clearance, error) {
	countries, err := countryCodes(terms.Countries)
	if err != nil {
		return Clearance{}, err
	}
	terms.Countries = countries
	terms.Licence, terms.Partner = strings.TrimSpace(terms.Licence), strings.TrimSpace(terms.Partner)
	switch {
	case terms.Licence == "" || terms.Partner == "":
		return Clearance{}, errors.New("economy: a clearance names the licence it rests on and the licensed partner the money moves through")
	case !terms.ExpiresAt.After(time.Now()):
		return Clearance{}, errors.New("economy: a clearance needs an expiry in the future")
	}
	return s.recordClearance(ctx, key, "clear", operator, terms)
}

// RevokeClearance stops the capability taking real money, from its next use; why is recorded.
func (s *DualTokenStore) RevokeClearance(ctx context.Context, key, operator, why string) error {
	_, err := s.recordClearance(ctx, key, "revoke", operator, ClearanceTerms{Reference: why})
	return err
}

func (s *DualTokenStore) recordClearance(ctx context.Context, key, action, operator string, terms ClearanceTerms) (Clearance, error) {
	c, ok := CapabilityByKey(key)
	switch {
	case !ok:
		return Clearance{}, fmt.Errorf("economy: no wallet capability is called %q", key)
	case c.Class == ClassGreen:
		return Clearance{}, fmt.Errorf("economy: %s is GREEN and takes real money already: there is nothing to clear", c.Name)
	case strings.TrimSpace(operator) == "" || strings.TrimSpace(terms.Reference) == "":
		return Clearance{}, errors.New("economy: a clearance needs who records it and a reference (why, for a revoke)")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Clearance{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One capability's clears and revokes are ordered: the latest row is the one in force.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('wallet_clearance:' || $1, 0))`, key); err != nil {
		return Clearance{}, err
	}
	var latest string
	err = tx.QueryRow(ctx, `SELECT action FROM wallet_clearances WHERE capability = $1 ORDER BY id DESC LIMIT 1`, key).Scan(&latest)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Clearance{}, fmt.Errorf("economy: clearance: %w", err)
	}
	if action == "revoke" && latest != "clear" {
		return Clearance{}, fmt.Errorf("economy: %s has no clearance to revoke", c.Name)
	}
	var expires *time.Time
	if action == "clear" {
		expires = &terms.ExpiresAt
	}
	if terms.Countries == nil {
		terms.Countries = []string{}
	}
	cl := Clearance{By: operator, ClearanceTerms: terms}
	if err := tx.QueryRow(ctx, `INSERT INTO wallet_clearances (capability, action, operator, reference, countries, partner, licence_reference,
		expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING created_at`, key, action, operator, terms.Reference, terms.Countries,
		terms.Partner, terms.Licence, expires).Scan(&cl.At); err != nil {
		return Clearance{}, fmt.Errorf("economy: record clearance: %w", err)
	}
	return cl, tx.Commit(ctx)
}

// ClearanceRecord is one row of the clearances' audit log.
type ClearanceRecord struct {
	Capability string    `json:"capability"`
	Action     string    `json:"action"`
	Operator   string    `json:"operator"`
	Reference  string    `json:"reference"`
	At         time.Time `json:"at"`
}

// ClearanceLog is every clear and revoke, newest first.
func (s *DualTokenStore) ClearanceLog(ctx context.Context, limit int) ([]ClearanceRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT capability, action, operator, reference, created_at FROM wallet_clearances
		ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("economy: clearances: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ClearanceRecord, error) {
		var r ClearanceRecord
		return r, row.Scan(&r.Capability, &r.Action, &r.Operator, &r.Reference, &r.At)
	})
}

// clearancesInForce is each capability's clearance whose latest row is a clear that names its countries and has
// not expired.
func clearancesInForce(ctx context.Context, q pgxDB) (map[string]Clearance, error) {
	rows, err := q.Query(ctx, `SELECT capability, operator, reference, created_at, countries, partner, licence_reference, expires_at
		FROM (SELECT DISTINCT ON (capability) * FROM wallet_clearances ORDER BY capability, id DESC) latest
		WHERE action = 'clear' AND expires_at > now() AND cardinality(countries) > 0`)
	if err != nil {
		return nil, fmt.Errorf("economy: clearances: %w", err)
	}
	defer rows.Close()
	out := map[string]Clearance{}
	for rows.Next() {
		var key string
		var c Clearance
		if err := rows.Scan(&key, &c.By, &c.Reference, &c.At, &c.Countries, &c.Partner, &c.Licence, &c.ExpiresAt); err != nil {
			return nil, err
		}
		out[key] = c
	}
	return out, rows.Err()
}

// clearanceCountries is the countries key's clearance in force lists: nil unless its latest clearance row is a clear
// that has not expired and names its countries.
func clearanceCountries(ctx context.Context, q pgxDB, key string) ([]string, error) {
	var countries []string
	err := q.QueryRow(ctx, `SELECT CASE WHEN action = 'clear' AND COALESCE(expires_at > now(), false) THEN countries END
		FROM wallet_clearances WHERE capability = $1 ORDER BY id DESC LIMIT 1`, key).Scan(&countries)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("economy: clearance: %w", err)
	}
	if len(countries) == 0 {
		return nil, nil
	}
	return countries, nil
}

// capabilityCleared reports whether key's latest clearance row is a clear that has not expired and lists the
// country the use comes from (WithUseCountry). Anything else — a revoke, an expired clear, another country, no
// known country — is no clearance.
func capabilityCleared(ctx context.Context, q pgxDB, key string) (bool, error) {
	countries, err := clearanceCountries(ctx, q, key)
	return slices.Contains(countries, useCountry(ctx)), err
}

// CapabilityCleared reports whether capability key has a clearance in force for the country the use comes from
// (WithUseCountry): the partners registry asks it before it hands out a real partner (B30.3).
func (s *DualTokenStore) CapabilityCleared(ctx context.Context, key string) (bool, error) {
	return capabilityCleared(ctx, s.pool, key)
}

// capabilityLive says why capability c may not take live money for workspaceID — nil when it may: an AMBER or RED
// one once it is cleared (clearanceCountries), a GREEN one always — and, B32.12, neither an AMBER or RED one nor any
// B30 registers while the workspace's plan keeps money capabilities on test money, a clearance or not; and, B30.4,
// not while the workspace's live verification level is below the one c needs, or no limit is set for it; and, B30.10,
// not an AMBER or RED one for an owner verified in a country its clearance does not list.
func capabilityLive(ctx context.Context, q pgxDB, workspaceID string, c Capability) (*CapabilityRefusal, error) {
	var countries []string
	if c.Class != ClassGreen {
		var err error
		if countries, err = clearanceCountries(ctx, q, c.Key); err != nil || countries == nil {
			return &CapabilityRefusal{Capability: c}, err
		}
	} else if !b30[c.Key] {
		return nil, nil
	}
	plan, err := plans.Of(ctx, q, workspaceID)
	if err != nil {
		return nil, err
	}
	if !plan.LiveMoney {
		return &CapabilityRefusal{Capability: c, Plan: plan.RefuseLiveMoney(plans.Current(), c.Name)}, nil
	}
	level, err := levelRefusal(ctx, q, workspaceID, c)
	if err != nil {
		return nil, err
	}
	if level != nil {
		return &CapabilityRefusal{Capability: c, Level: level}, nil
	}
	if c.Class == ClassGreen {
		return nil, nil
	}
	country, err := ownerCountry(ctx, q, workspaceID)
	if err != nil || slices.Contains(countries, country) {
		return nil, err
	}
	return &CapabilityRefusal{Capability: c, Country: &CountryRefusal{Country: country, Countries: countries}}, nil
}

// spendForCapability judges a spend of amount µLXC of workspaceID's credits on capability key, in tx. A
// GREEN capability, or a cleared one, takes any credits and 0 is returned. An uncleared AMBER or RED one
// takes test-funded credits only: they are taken here and their amount returned, or the spend is refused with
// a *CapabilityRefusal when the test-funded credits do not cover it — unless the workspace is a test one, whose
// every credit is test money: it takes what test-funded credits there are, and the rest of the spend with them.
func spendForCapability(ctx context.Context, tx pgx.Tx, workspaceID, key string, amount int64) (testFunded int64, err error) {
	c, ok := CapabilityByKey(key)
	if !ok {
		return 0, fmt.Errorf("economy: no wallet capability is called %q", key)
	}
	if amount <= 0 {
		return 0, nil
	}
	// B30.8: a frozen workspace's AMBER and RED capabilities take no money, test-funded credits included.
	if err := refuseFrozen(ctx, tx, workspaceID, c); err != nil {
		return 0, err
	}
	// B30.9: nor does a capability whose latest terms the workspace has not accepted.
	if err := refuseUnaccepted(ctx, tx, workspaceID, c); err != nil {
		return 0, err
	}
	refusal, err := capabilityLive(ctx, tx, workspaceID, c)
	if err != nil || refusal == nil {
		return 0, err
	}
	have, err := testFundedULXC(ctx, tx, workspaceID)
	if err != nil {
		return 0, err
	}
	take := amount
	if have < amount {
		// B25.3: a test workspace's credits are test money whatever funded them — its starting grant included —
		// and the wall (workspace.CheckMoneyWall) keeps them among test workspaces, so all of the spend is.
		test, err := testWorkspace(ctx, tx, workspaceID)
		if err != nil {
			return 0, err
		}
		if !test {
			return 0, refusal
		}
		take = have
	}
	if _, err := tx.Exec(ctx, `UPDATE lxc_balances SET test_funded_ulxc = $2 WHERE workspace_id = $1`, workspaceID, have-take); err != nil {
		return 0, fmt.Errorf("economy: take test-funded credits: %w", err)
	}
	return take, nil
}

// testWorkspace reports whether workspaceID is a test (synthetic) workspace. One with no row is real.
func testWorkspace(ctx context.Context, q pgxDB, workspaceID string) (bool, error) {
	var test bool
	if err := q.QueryRow(ctx, `SELECT COALESCE((SELECT synthetic FROM workspaces WHERE id = $1), false)`, workspaceID).Scan(&test); err != nil {
		return false, fmt.Errorf("economy: test workspace: %w", err)
	}
	return test, nil
}

// requireBilledCapability judges money on workspaceID's Stripe bill for capability key: real money when the key
// is live, which an uncleared AMBER or RED capability refuses, and so does a cleared one on a plan that keeps it
// on test money (B32.12), or below the verification level it needs (B30.4).
func (s *DualTokenStore) requireBilledCapability(ctx context.Context, q pgxDB, workspaceID, key string) error {
	c, ok := CapabilityByKey(key)
	if !ok {
		return fmt.Errorf("economy: no wallet capability is called %q", key)
	}
	// B30.8: a frozen workspace's AMBER and RED capabilities take no money, test or live. A caller that moves the
	// money in a transaction asks again inside it (payCompanyAgent).
	if err := refuseFrozen(ctx, q, workspaceID, c); err != nil {
		return err
	}
	// B30.9: and a capability with terms takes no money until the workspace accepts their latest version.
	if err := refuseUnaccepted(ctx, q, workspaceID, c); err != nil {
		return err
	}
	if !s.liveStripe {
		return nil
	}
	refusal, err := capabilityLive(ctx, q, workspaceID, c)
	if err != nil {
		return err
	}
	if refusal != nil {
		return refusal
	}
	return nil
}

// RequireBilledCapability asks capability key whether money on workspaceID's Stripe bill may be taken for it: the
// marketplace asks it where it records a licence's charge (B32.19).
func (s *DualTokenStore) RequireBilledCapability(ctx context.Context, workspaceID, key string) error {
	return s.requireBilledCapability(ctx, s.pool, workspaceID, key)
}

// testFundedULXC locks workspaceID's balance row and reads its test-funded credits: never more than the
// balance and the workspace's open holds. A test workspace's are all of those (B25.3), its starting grant
// included, so what it spends, sends or cashes out reads as test money to the last µLXC (B17.105).
func testFundedULXC(ctx context.Context, tx pgx.Tx, workspaceID string) (int64, error) {
	var funded, upTo int64
	var test bool
	err := tx.QueryRow(ctx, `SELECT test_funded_ulxc, GREATEST(balance + (SELECT COALESCE(sum(held_ulxc), 0) FROM lxc_reservations
		WHERE workspace_id = $1 AND status = 'held'), 0)::bigint, COALESCE((SELECT synthetic FROM workspaces WHERE id = $1), false)
		FROM lxc_balances WHERE workspace_id = $1 FOR UPDATE`, workspaceID).Scan(&funded, &upTo, &test)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("economy: test-funded credits: %w", err)
	}
	if test {
		return upTo, nil
	}
	return min(funded, upTo), nil
}

// addTestFunded returns amount µLXC of test-funded credits to workspaceID (a test purchase, or a gated spend
// given back). The caller has already credited the balance.
func addTestFunded(ctx context.Context, tx pgx.Tx, workspaceID string, amount int64) error {
	if amount <= 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE lxc_balances SET test_funded_ulxc = test_funded_ulxc + $2 WHERE workspace_id = $1`,
		workspaceID, amount); err != nil {
		return fmt.Errorf("economy: add test-funded credits: %w", err)
	}
	return nil
}

// TestFundedLXC reads the test-funded part of a workspace's balance.
func (s *DualTokenStore) TestFundedLXC(ctx context.Context, workspaceID string) (int64, error) {
	var v int64
	err := s.pool.QueryRow(ctx, `SELECT test_funded_ulxc FROM lxc_balances WHERE workspace_id = $1`, workspaceID).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return v, err
}
