package economy

import (
	"context"
	"strings"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/plans"
)

// labels.go — B56.7: LIVE OR SANDBOX FOR EVERY MONEY CAPABILITY, READ FROM THE FACTS EVERY TIME.
//
// A capability is Live when its money can be real: a GREEN one while card payments are live, an AMBER or RED one
// while it has a clearance in force and every rail its money moves on is live. Anything else is Sandbox. For a
// workspace it is Sandbox too while its plan has no live money for a B30 capability, or the workspace is synthetic.
// Nothing is stored: every answer is worked out from the rails and clearances of that moment.

// The labels.
const (
	LabelLive    = "live"
	LabelSandbox = "sandbox"
)

// RailCards is money that enters by card — credits bought and Stripe bills: live while card payments are live.
const RailCards = "cards"

func partnerRail(s partners.Service) string { return "partner:" + string(s) }
func moneyRail(c money.Capability) string   { return "money:" + string(c) }

// capabilityRails names every rail each capability's money moves on. rules_approvals_statements_pots moves no money,
// so it has none and no label.
var capabilityRails = map[string][]string{
	"spend_on_talyvor":                {RailCards},
	CapabilityBuyListings:             {RailCards},
	CapabilityRentAndSubscribe:        {RailCards},
	CapabilityLineageRoyalties:        {RailCards},
	"move_between_own_agents":         {RailCards},
	"rules_approvals_statements_pots": nil,
	"company_credit_line":             {RailCards},
	CapabilityPayAnotherOwner:         {RailCards, moneyRail(money.CapTransfers)},
	"loans_between_companies":         {partnerRail(partners.ServiceCapital)},
	CapabilityEscrow:                  {RailCards},
	CapabilityCashOut:                 {RailCards, moneyRail(money.CapPayouts)},
	"credit_involving_a_person":       {partnerRail(partners.ServiceCapital)},
	"interest_and_yield":              {partnerRail(partners.ServiceAccount)},
	"invest_and_trade":                {partnerRail(partners.ServiceBroker)},
	CapabilityAgentCard:               {partnerRail(partners.ServiceAgentToken), moneyRail(money.CapCards)},
	CapabilityCurrencyAccounts:        {partnerRail(partners.ServiceAccount), moneyRail(money.CapAccounts)},
	CapabilityAccountDetails:          {partnerRail(partners.ServiceAccount), moneyRail(money.CapAccounts)},
	CapabilityPaymentsIn:              {partnerRail(partners.ServiceAccount)},
	CapabilityPaymentsOut:             {partnerRail(partners.ServiceAccount), moneyRail(money.CapTransfers)},
	CapabilityPayByBank:               {partnerRail(partners.ServiceAccount)},
	CapabilityFX:                      {partnerRail(partners.ServiceFX)},
	CapabilityStablecoins:             {partnerRail(partners.ServiceStablecoin), moneyRail(money.CapWallets)},
	CapabilityX402:                    {partnerRail(partners.ServiceStablecoin), moneyRail(money.CapWallets)},
	CapabilityMerchantAcceptance:      {partnerRail(partners.ServiceAccount)},
	CapabilityB2BCredit:               {partnerRail(partners.ServiceCapital)},
	CapabilitySellerAdvances:          {partnerRail(partners.ServiceCapital)},
	CapabilityLendingMarketplace:      {partnerRail(partners.ServiceCapital)},
	CapabilityTradeEquities:           {partnerRail(partners.ServiceBroker)},
	CapabilityTradeCrypto:             {partnerRail(partners.ServiceBroker)},
	CapabilityTradePrediction:         {partnerRail(partners.ServiceBroker)},
	CapabilityTreasurySweep:           {partnerRail(partners.ServiceAccount)},
	CapabilityPriceLock:               {RailCards},
	CapabilityCover:                   {partnerRail(partners.ServiceInsurer)},
	CapabilityPayoutsToPeople:         {partnerRail(partners.ServiceAccount), moneyRail(money.CapPayouts)},
}

// LabelFacts is what a label is worked out from: the capabilities with a clearance in force, and the rails that are
// live.
type LabelFacts struct {
	Cleared map[string]bool
	Live    map[string]bool
}

// Label is c's label under f: "live", "sandbox", or "" for a capability that moves no money.
func Label(c Capability, f LabelFacts) string {
	rails := capabilityRails[c.Key]
	if len(rails) == 0 {
		return ""
	}
	if c.Class != ClassGreen && !f.Cleared[c.Key] {
		return LabelSandbox
	}
	for _, r := range rails {
		if !f.Live[r] {
			return LabelSandbox
		}
	}
	return LabelLive
}

// workspaceLabel is label for a workspace whose plan has liveMoney or not, and which is synthetic or not.
func workspaceLabel(label string, c Capability, liveMoney, synthetic bool) string {
	if label == LabelLive && (synthetic || (b30[c.Key] && !liveMoney)) {
		return LabelSandbox
	}
	return label
}

// railReader is the partners registry: each partner rail and its mode.
type railReader interface{ Rails() []partners.Rail }

// SetLabelRails gives the store what labels read besides clearances: whether card payments are live
// (billing.CardPayments) and the partner rails.
func (s *DualTokenStore) SetLabelRails(cardPayments string, rails railReader) {
	s.cardPayments, s.partnerRails = cardPayments, rails
}

// labelFacts reads the clearances in force and each rail's mode now.
func (s *DualTokenStore) labelFacts(ctx context.Context) (LabelFacts, error) {
	cleared, err := clearancesInForce(ctx, s.pool)
	if err != nil {
		return LabelFacts{}, err
	}
	f := LabelFacts{Cleared: map[string]bool{}, Live: map[string]bool{RailCards: s.cardPayments == "live"}}
	for key := range cleared {
		f.Cleared[key] = true
	}
	if s.partnerRails != nil {
		for _, r := range s.partnerRails.Rails() {
			f.Live[partnerRail(r.Service)] = r.Mode == "live"
		}
	}
	for _, rails := range capabilityRails {
		for _, r := range rails {
			if c, ok := strings.CutPrefix(r, "money:"); ok && s.moneyLayer != nil {
				f.Live[r] = s.moneyLayer.Live(money.Capability(c))
			}
		}
	}
	return f, nil
}

// Labels is every money capability's label (B56.7), by key.
func (s *DualTokenStore) Labels(ctx context.Context) (map[string]string, error) {
	f, err := s.labelFacts(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, c := range Capabilities {
		if l := Label(c, f); l != "" {
			out[c.Key] = l
		}
	}
	return out, nil
}

// WorkspaceLabels is Labels for workspaceID: Sandbox too while its plan has no live money for a B30 capability, or
// it is synthetic.
func (s *DualTokenStore) WorkspaceLabels(ctx context.Context, workspaceID string) (map[string]string, error) {
	out, err := s.Labels(ctx)
	if err != nil {
		return nil, err
	}
	plan, err := plans.Of(ctx, s.pool, workspaceID)
	if err != nil {
		return nil, err
	}
	synthetic, err := testWorkspace(ctx, s.pool, workspaceID)
	if err != nil {
		return nil, err
	}
	for key, l := range out {
		c, _ := CapabilityByKey(key)
		out[key] = workspaceLabel(l, c, plan.LiveMoney, synthetic)
	}
	return out, nil
}

// objectLabel is key's label for workspaceID on an object in mode (TEST or LIVE): Sandbox while it is not LIVE.
func (s *DualTokenStore) objectLabel(ctx context.Context, workspaceID, key, mode string) string {
	l := s.workspaceLabelOf(ctx, workspaceID, key)
	if l == LabelLive && mode != DetailsLive {
		return LabelSandbox
	}
	return l
}

// workspaceLabelOf is one capability's label for workspaceID; "" when it cannot be read, which shows no label.
func (s *DualTokenStore) workspaceLabelOf(ctx context.Context, workspaceID, key string) string {
	labels, err := s.WorkspaceLabels(ctx, workspaceID)
	if err != nil {
		return ""
	}
	return labels[key]
}
