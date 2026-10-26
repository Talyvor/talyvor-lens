package economy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/partners"
)

// account_details.go — B30.14: ACCOUNT DETAILS OTHERS CAN PAY INTO.
//
// A company's account in GBP, EUR or USD has details at the account partner that holds it: a sort code and account
// number for pounds, an IBAN and BIC for euros, a routing and account number for dollars. An agent's account has none
// of its own: a payer pays its company's details and quotes the agent account's payment reference (migration 0241),
// and ReceivePayment puts the money in the agent's account. Details the Test partner made up reach no bank, and every
// answer that carries them says TEST.
//
// Reading details is the first use of account_details, class RED: a frozen workspace, or one that has not accepted
// the capability's terms, is shown none. Money arriving is payments_in, class RED, asked by PostMoney.

// The modes of account details.
const (
	DetailsTest = "TEST" // made up by the Test partner: they reach no bank
	DetailsLive = "LIVE"
)

// ErrNoAccountDetails: the account partner gave no details for the account.
var ErrNoAccountDetails = errors.New("economy: the account partner gave no details for this account")

// testDetailsNotice is what every answer carrying the Test partner's details says.
const testDetailsNotice = "Preview — test money only. These details are made up and reach no bank: nothing paid to them moves real money."

// CurrencyAccountDetails is what someone needs to pay into a company's or an agent's account: the company account's
// details, and for an agent's account the payment reference to quote with them.
type CurrencyAccountDetails struct {
	AccountID string `json:"account_id"`
	partners.AccountDetails
	PaymentReference string `json:"payment_reference,omitempty"` // an agent's: quoted with the details, it routes the money to the agent
	Mode             string `json:"mode"`                        // TEST or LIVE
	Notice           string `json:"notice,omitempty"`
}

// CurrencyAccountDetails is what a payer needs to pay into workspaceID's account accountID — with agentID, only if
// it is that agent's.
func (s *DualTokenStore) CurrencyAccountDetails(ctx context.Context, workspaceID, agentID, accountID string) (CurrencyAccountDetails, error) {
	d := CurrencyAccountDetails{AccountID: accountID}
	var status, companyID, holder, ref, currency string
	err := s.pool.QueryRow(ctx, `SELECT a.status, COALESCE(a.payment_reference, ''), c.id, c.name, c.partner_account_ref, c.currency
		FROM money_accounts a JOIN money_accounts c ON c.id = COALESCE(a.parent_account_id, a.id)
		WHERE a.id = $1 AND a.workspace_id = $2 AND a.purpose IN ('company', 'agent') AND ($3 = '' OR a.agent_id = $3)`,
		accountID, workspaceID, agentID).Scan(&status, &d.PaymentReference, &companyID, &holder, &ref, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return CurrencyAccountDetails{}, ErrMoneyAccountNotFound
	}
	if err != nil {
		return CurrencyAccountDetails{}, fmt.Errorf("economy: account details: %w", err)
	}
	if status != MoneyOpen {
		return CurrencyAccountDetails{}, fmt.Errorf("%w: %s is %s", ErrMoneyAccountNotOpen, accountID, status)
	}
	c, _ := CapabilityByKey(CapabilityAccountDetails)
	if err := refuseFrozen(ctx, s.pool, workspaceID, c); err != nil {
		return CurrencyAccountDetails{}, err
	}
	if err := refuseUnaccepted(ctx, s.pool, workspaceID, c); err != nil {
		return CurrencyAccountDetails{}, err
	}
	if s.accountPartners == nil {
		return CurrencyAccountDetails{}, fmt.Errorf("%w: no account partner is set", ErrNoAccountDetails)
	}
	p, err := s.accountPartners.Account(ctx, CapabilityAccountDetails)
	if err != nil {
		return CurrencyAccountDetails{}, fmt.Errorf("economy: account details: %w", err)
	}
	details, err := p.AccountDetails(ctx, ref)
	if errors.Is(err, partners.ErrNotFound) && p.Name() == "test" {
		// The Test partner keeps its accounts in memory, so after Lens restarts it is asked to open the account again:
		// opening is idempotent on the id, and gives back the same reference.
		if reopened, oerr := p.OpenAccount(ctx, partners.AccountRequest{ID: companyID, Holder: holder, Currency: currency}); oerr == nil && reopened.Ref == ref {
			details, err = p.AccountDetails(ctx, ref)
		}
	}
	if err != nil {
		return CurrencyAccountDetails{}, fmt.Errorf("%w: %v", ErrNoAccountDetails, err)
	}
	d.AccountDetails = details
	d.Mode = DetailsLive
	if p.Name() == "test" {
		d.Mode, d.Notice = DetailsTest, testDetailsNotice
	}
	return d, nil
}

// InboundPayment is money a payer sent to a company account's details, as the account partner reports it.
type InboundPayment struct {
	PartnerAccountRef string `json:"partner_account_ref"` // the partner's reference for the account it arrived in
	PartnerRef        string `json:"partner_ref"`         // the partner's reference for the payment: it posts once
	Payer             string `json:"payer"`
	Reference         string `json:"reference"` // what the payer quoted: an agent's payment reference in it routes the money
	AmountMinor       int64  `json:"amount_minor"`
	Currency          string `json:"currency"`
	Funding           string `json:"funding"` // test ("" too) or live
}

// ErrTestMoney: test money asked for in an amount, or with a key, it cannot have.
var ErrTestMoney = errors.New("economy: invalid test money")

// AddTestMoney is B17.255: the owner puts amountMinor of test money into workspaceID's company account accountID, so
// a preview can pay out of it. It is a payment in from the Test partner through ReceivePayment — screened, asked of
// payments_in, posted once per key and funding test, so it never counts as real money.
func (s *DualTokenStore) AddTestMoney(ctx context.Context, workspaceID, accountID string, amountMinor int64, key string) (MoneyEntry, error) {
	if amountMinor <= 0 || strings.TrimSpace(key) == "" {
		return MoneyEntry{}, fmt.Errorf("%w: it needs a positive amount_minor and an idempotency_key", ErrTestMoney)
	}
	var ref, currency string
	err := s.pool.QueryRow(ctx, `SELECT partner_account_ref, currency FROM money_accounts
		WHERE id = $1 AND workspace_id = $2 AND purpose = 'company' AND status = 'open'`, accountID, workspaceID).Scan(&ref, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return MoneyEntry{}, fmt.Errorf("%w: no open company account %s in this workspace", ErrMoneyAccountNotFound, accountID)
	}
	if err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: add test money: %w", err)
	}
	return s.ReceivePayment(ctx, InboundPayment{PartnerAccountRef: ref, PartnerRef: "test_money:" + accountID + ":" + key,
		Payer: "Talyvor test money", Reference: "Test money", AmountMinor: amountMinor, Currency: currency, Funding: FundingTest})
}

// paymentReference is an agent account's payment reference, found in what a payer quoted once it is upper-cased and
// stripped of everything but letters and digits.
var paymentReference = regexp.MustCompile(`TLV[0-9A-F]{12}`)

// ReceivePayment posts money that arrived at a company account's details: into the open agent account under it whose
// payment reference the payer quoted, or else into the company account. Money that arrived at an agent's wallet
// (B46.4) goes into the wallet. It is one entry from the partner account
// that mirrors the company's, screened against the payer first (B30.6), and the partner's reference for the payment
// is its idempotency key, so a payment reported twice posts once.
func (s *DualTokenStore) ReceivePayment(ctx context.Context, in InboundPayment) (MoneyEntry, error) {
	switch {
	case in.PartnerAccountRef == "" || in.PartnerRef == "":
		return MoneyEntry{}, errors.New("economy: a payment in names the partner's account and its own reference")
	case in.AmountMinor <= 0:
		return MoneyEntry{}, fmt.Errorf("economy: a payment in brings money, not %d", in.AmountMinor)
	}
	if in.Funding == "" {
		in.Funding = FundingTest
	}
	var ws, companyID, partnerID string
	err := s.pool.QueryRow(ctx, `SELECT c.workspace_id, c.id, p.id FROM money_accounts c
		JOIN money_accounts p ON p.workspace_id = c.workspace_id AND p.partner_account_ref = c.partner_account_ref AND p.purpose = 'partner'
		WHERE c.partner_account_ref = $1 AND c.purpose IN ('company', 'agent') AND c.status = 'open'`, in.PartnerAccountRef).Scan(&ws, &companyID, &partnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return MoneyEntry{}, fmt.Errorf("%w: no open company account is at the partner's %s", ErrMoneyAccountNotFound, in.PartnerAccountRef)
	}
	if err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: receive payment: %w", err)
	}
	to := companyID
	quoted := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToUpper(r)
		}
		return -1
	}, in.Reference)
	if ref := paymentReference.FindString(quoted); ref != "" {
		var agentAccount string
		err := s.pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE parent_account_id = $1 AND payment_reference = $2 AND status = 'open'`,
			companyID, ref).Scan(&agentAccount)
		switch {
		case err == nil:
			to = agentAccount
		case !errors.Is(err, pgx.ErrNoRows):
			return MoneyEntry{}, fmt.Errorf("economy: receive payment: %w", err)
		}
	}
	return s.PostMoney(ctx, MoneyEntry{WorkspaceID: ws, Capability: CapabilityPaymentsIn, Kind: "payment_in",
		IdempotencyKey: "payment_in:" + in.PartnerRef, Funding: in.Funding, Memo: in.Reference, Counterparty: in.Payer, PartnerRef: in.PartnerRef,
		Postings: []MoneyPosting{
			{AccountID: partnerID, AmountMinor: -in.AmountMinor, Currency: in.Currency},
			{AccountID: to, AmountMinor: in.AmountMinor, Currency: in.Currency},
		}})
}
