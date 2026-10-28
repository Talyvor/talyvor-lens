// Package money is the money layer (B46.3): one partner-neutral interface for each thing money does — accounts,
// wallets, transfers, cards, payouts and identity checks — and one adapter per partner that answers them.
//
// Lens asks a Registry, never a partner: the Registry reads a switch per capability, country and partner before every
// call and hands out the adapter switched on there, and the sandbox adapter everywhere else. Every switch is off for
// real money today, so the sandbox — today's test-money partners (internal/partners) — answers every call. Before an
// adapter moves money the owner's limits and one-tap approval are asked (Limits): the adapter never decides them.
// Every call, and every verified partner webhook, is a link of the hash-chained audit trail (Audit).
//
// Amounts are int64 minor units of an explicit asset — pence, cents, or millionths of a USDC — never a float. Every
// call that changes anything carries the caller's idempotency key: asked again, it does nothing new and answers as it
// did the first time. Errors are typed: ErrInsufficientFunds, ErrLimit, ErrApprovalNeeded, ErrPartnerDown, ErrRefused,
// ErrInvalid, ErrNotFound and ErrKeyReused, tested with errors.Is.
package money

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The assets money is held in, and how many decimal places each one's minor unit is (economy.MoneyCurrencies).
const (
	GBP  = "GBP"
	EUR  = "EUR"
	USD  = "USD"
	USDC = "USDC"
)

var assets = map[string]int{GBP: 2, EUR: 2, USD: 2, USDC: 6}

// Amount is an amount of an asset in its minor units.
type Amount struct {
	Minor int64  `json:"amount_minor"`
	Asset string `json:"asset"`
}

func (a Amount) String() string { return fmt.Sprintf("%d %s minor units", a.Minor, a.Asset) }

// check refuses an amount that is not positive or not of an asset Talyvor holds.
func (a Amount) check() error {
	if _, ok := assets[a.Asset]; !ok {
		return fmt.Errorf("%w: an asset is GBP, EUR, USD or USDC, not %q", ErrInvalid, a.Asset)
	}
	if a.Minor <= 0 {
		return fmt.Errorf("%w: an amount is positive, not %d", ErrInvalid, a.Minor)
	}
	return nil
}

func checkKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("%w: every call carries the caller's idempotency key", ErrInvalid)
	}
	return nil
}

// The typed errors every adapter answers with.
var (
	ErrInvalid           = errors.New("money: invalid request")
	ErrNotFound          = errors.New("money: the partner has no record of this reference")
	ErrKeyReused         = errors.New("money: this idempotency key was already used for a different request")
	ErrInsufficientFunds = errors.New("money: insufficient funds")
	ErrLimit             = errors.New("money: over the owner's limit")
	ErrApprovalNeeded    = errors.New("money: the owner must approve this first")
	ErrPartnerDown       = errors.New("money: the partner did not answer")
	ErrRefused           = errors.New("money: the partner refused")
)

// Capability is one thing money does; a switch is per capability.
type Capability string

// The capabilities.
const (
	CapAccounts  Capability = "accounts"
	CapWallets   Capability = "wallets"
	CapTransfers Capability = "transfers"
	CapCards     Capability = "cards"
	CapPayouts   Capability = "payouts"
	CapKYC       Capability = "kyc"
)

// Capabilities is every capability, in order.
var Capabilities = []Capability{CapAccounts, CapWallets, CapTransfers, CapCards, CapPayouts, CapKYC}

// Status is where an operation with a partner stands.
type Status string

// The statuses.
const (
	StatusPending   Status = "pending"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusReturned  Status = "returned" // done, then undone by the partner: the money came back
)

// Result is a partner's answer about one operation.
type Result struct {
	Ref    string `json:"partner_ref"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Details is how money reaches an account or a wallet: bank details for pounds, euros and dollars, a chain address for
// USDC.
type Details struct {
	Asset         string `json:"asset"`
	Holder        string `json:"holder,omitempty"`
	IBAN          string `json:"iban,omitempty"`
	BIC           string `json:"bic,omitempty"`
	SortCode      string `json:"sort_code,omitempty"`
	AccountNumber string `json:"account_number,omitempty"`
	RoutingNumber string `json:"routing_number,omitempty"`
	Network       string `json:"network,omitempty"`
	Address       string `json:"address,omitempty"`
}

// Payee is who money out goes to: bank details, or a chain address for USDC.
type Payee struct {
	Name          string `json:"name"`
	IBAN          string `json:"iban,omitempty"`
	BIC           string `json:"bic,omitempty"`
	SortCode      string `json:"sort_code,omitempty"`
	AccountNumber string `json:"account_number,omitempty"`
	RoutingNumber string `json:"routing_number,omitempty"`
	Network       string `json:"network,omitempty"`
	Address       string `json:"address,omitempty"`
}

// Accounts opens and closes accounts held at a partner in one asset.
type Accounts interface {
	Open(ctx context.Context, req OpenAccount) (Account, error)
	Close(ctx context.Context, key, ref string) (Result, error)
	Details(ctx context.Context, ref string) (Details, error)
}

// OpenAccount asks for an account in one asset for a holder.
type OpenAccount struct {
	Key     string `json:"key"`
	Holder  string `json:"holder"`
	Asset   string `json:"asset"`
	Country string `json:"country"` // the holder's, ISO 3166-1 alpha-2
}

// Account is an account at a partner: open once its status is completed.
type Account struct {
	Result
	Asset string `json:"asset"`
}

// Wallets holds an agent's money.
type Wallets interface {
	Create(ctx context.Context, req CreateWallet) (Wallet, error)
	Balance(ctx context.Context, ref string) (Amount, error)
	Receive(ctx context.Context, ref string) (Details, error)
}

// CreateWallet asks for an agent's wallet in one asset.
type CreateWallet struct {
	Key     string `json:"key"`
	AgentID string `json:"agent_id"`
	Asset   string `json:"asset"`
}

// Wallet is an agent's wallet at a partner.
type Wallet struct {
	Ref   string `json:"partner_ref"`
	Asset string `json:"asset"`
}

// Transfers moves money between wallets, out to a payee, and credits money that came in.
type Transfers interface {
	Internal(ctx context.Context, req Internal) (Result, error)
	Outbound(ctx context.Context, req Outbound) (Result, error)
	Credit(ctx context.Context, req Credit) (Result, error)
}

// Internal moves money from one wallet to another at the same partner.
type Internal struct {
	Key    string `json:"key"`
	From   string `json:"from"`
	To     string `json:"to"`
	Amount Amount `json:"amount"`
}

// Outbound pays money out of a wallet to a payee.
type Outbound struct {
	Key       string `json:"key"`
	From      string `json:"from"`
	Payee     Payee  `json:"payee"`
	Amount    Amount `json:"amount"`
	Reference string `json:"reference"` // what the payee sees
}

// Credit is money that came into a wallet, as the partner reported it: PartnerRef is its reference, and it credits
// once.
type Credit struct {
	Key        string `json:"key"`
	To         string `json:"to"`
	Amount     Amount `json:"amount"`
	PartnerRef string `json:"partner_ref"`
}

// Cards issues an agent's cards on a wallet.
type Cards interface {
	Issue(ctx context.Context, req IssueCard) (Card, error)
	Lock(ctx context.Context, req LockCard) (Card, error)
	Freeze(ctx context.Context, key, ref string) (Card, error)
	Close(ctx context.Context, key, ref string) (Card, error)
}

// IssueCard asks for a card on a wallet, locked to a merchant and capped.
type IssueCard struct {
	Key      string `json:"key"`
	Wallet   string `json:"wallet"`
	Merchant string `json:"merchant,omitempty"` // "" is any merchant
	Cap      Amount `json:"cap"`
}

// LockCard locks a card to a merchant and a cap.
type LockCard struct {
	Key      string `json:"key"`
	Ref      string `json:"ref"`
	Merchant string `json:"merchant,omitempty"`
	Cap      Amount `json:"cap"`
}

// The states of a card.
const (
	CardActive = "active"
	CardFrozen = "frozen"
	CardClosed = "closed"
)

// Card is a card at a partner.
type Card struct {
	Ref      string `json:"partner_ref"`
	Wallet   string `json:"wallet"`
	Last4    string `json:"last4"`
	Merchant string `json:"merchant,omitempty"`
	Cap      Amount `json:"cap"`
	State    string `json:"state"`
}

// Authorization is a card issuer asking, as a charge is made on a card, whether it may go. Lens answers; the issuer
// approves or declines the charge as told. Asked again under the same ID, the answer is the same.
type Authorization struct {
	ID       string `json:"id"`          // the issuer's
	Card     string `json:"partner_ref"` // the card's
	Merchant string `json:"merchant"`
	Amount   Amount `json:"amount"`
}

// Payouts pays a seller or a Room member out of a wallet.
type Payouts interface {
	Pay(ctx context.Context, req Payout) (Result, error)
}

// The recipients of a payout.
const (
	PayoutSeller     = "seller"
	PayoutRoomMember = "room_member"
)

// Payout pays a seller or a Room member.
type Payout struct {
	Key       string `json:"key"`
	From      string `json:"from"`
	Recipient string `json:"recipient"` // PayoutSeller or PayoutRoomMember
	To        Payee  `json:"to"`
	Amount    Amount `json:"amount"`
}

// KYC hands a person or a company to the partner's hosted verification, and reads where it stands.
type KYC interface {
	Start(ctx context.Context, req StartKYC) (Verification, error)
	Status(ctx context.Context, ref string) (Verification, error)
}

// The subjects of a verification.
const (
	KYCPerson  = "person"
	KYCCompany = "company"
)

// StartKYC starts a verification.
type StartKYC struct {
	Key     string `json:"key"`
	Subject string `json:"subject"` // KYCPerson or KYCCompany
	Name    string `json:"name"`
	Country string `json:"country"`
}

// Verification is where a verification stands, and where the person completes it.
type Verification struct {
	Result
	Link string `json:"link,omitempty"` // the partner's hosted page; Lens never stores it
}
