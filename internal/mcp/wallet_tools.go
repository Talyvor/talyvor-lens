package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/kya"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/workspace"
)

// wallet_tools.go — B22.11: AGENTS USE ALL OF THEIR WALLET THEMSELVES.
//
// The wallet_* tools give an agent's OWN key every capability of B22: send and request credits, answer a
// request made of it, give a transfer back, see its company's credit line, lend and borrow, pay into escrow and
// confirm or dispute it, keep pots, and trade in simulated portfolios. As with the B19.9 tools, the agent is
// the key's, never an argument: a loan, a request, a transfer or an escrow is acted on only when it is this
// agent's. Every movement goes through the same store call the owner's routes use, so the agent's rules and
// each capability's class judge it exactly as they would the owner's; a refusal is a tool result marked
// isError that says why. Every call is logged, refused ones too.

// WalletBank is what the wallet tools need of the economy store, beyond AgentBank.
type WalletBank interface {
	SendCredits(ctx context.Context, workspaceID, fromAgentID, address string, amount int64, memo string) (economy.AgentTransfer, error)
	RequestCredits(ctx context.Context, workspaceID, agentID, address string, amount int64, memo string) (economy.MoneyRequest, error)
	ListMoneyRequests(ctx context.Context, workspaceID string) ([]economy.MoneyRequest, error)
	AnswerMoneyRequest(ctx context.Context, workspaceID, requestID string, accept bool) (economy.MoneyRequest, error)
	ListAgentTransfers(ctx context.Context, workspaceID, agentID string) ([]economy.AgentTransfer, error)
	RefundTransfer(ctx context.Context, workspaceID, transferID string) (economy.AgentTransfer, error)
	CreditLine(ctx context.Context, workspaceID string) (economy.CreditLine, error)
	OfferLoan(ctx context.Context, workspaceID, lenderAgentID, borrowerAddress string, terms economy.LoanTerms) (economy.Loan, error)
	GetLoan(ctx context.Context, workspaceID, loanID string) (economy.Loan, error)
	ListLoans(ctx context.Context, workspaceID string) ([]economy.Loan, error)
	AnswerLoan(ctx context.Context, workspaceID, loanID string, accept bool) (economy.Loan, error)
	PayIntoEscrow(ctx context.Context, workspaceID, payerAgentID, payeeAddress string, amount int64, memo string, releaseAt time.Time) (economy.Escrow, error)
	GetEscrow(ctx context.Context, workspaceID, escrowID string) (economy.Escrow, error)
	ListEscrows(ctx context.Context, workspaceID string) ([]economy.Escrow, error)
	ConfirmEscrow(ctx context.Context, workspaceID, escrowID string) (economy.Escrow, error)
	DisputeEscrow(ctx context.Context, workspaceID, escrowID, reason string) (economy.Escrow, error)
	ListPots(ctx context.Context, workspaceID, agentID string) ([]economy.Pot, error)
	CreatePot(ctx context.Context, workspaceID, agentID, name, kind string, target int64, lockedUntil *time.Time) (economy.Pot, error)
	MoveToPot(ctx context.Context, workspaceID, agentID, potID string, amount int64) (economy.Pot, error)
	MoveFromPot(ctx context.Context, workspaceID, agentID, potID string, amount int64) (economy.Pot, error)
	SimQuotes(ctx context.Context) (economy.Quotes, error)
	OpenCurrencyAccount(ctx context.Context, workspaceID, agentID, currency, funding string) (economy.CurrencyAccount, error)
	CurrencyAccounts(ctx context.Context, workspaceID, agentID string) ([]economy.CurrencyAccount, error)
	CurrencyAccountDetails(ctx context.Context, workspaceID, agentID, accountID string) (economy.CurrencyAccountDetails, error)
	PayOutside(ctx context.Context, workspaceID string, in economy.OutsidePayment) (economy.OutsidePayment, error)
	CreateInvoice(ctx context.Context, workspaceID, agentID string, r economy.InvoiceRequest) (economy.Invoice, error)
	Invoices(ctx context.Context, workspaceID, agentID string) ([]economy.Invoice, error)
	PayInvoiceByAgent(ctx context.Context, token, workspaceID, agentID, funding string) (economy.PaidInvoice, error)
	OpenPortfolio(ctx context.Context, workspaceID, agentID, name string, cashUUSD int64) (economy.Portfolio, error)
	ListPortfolios(ctx context.Context, workspaceID, agentID string) ([]economy.Portfolio, error)
	PlaceSimOrder(ctx context.Context, workspaceID, agentID, portfolioID string, in economy.SimOrderInput) (economy.SimOrder, error)
	CancelSimOrder(ctx context.Context, workspaceID, agentID, portfolioID, orderID string) (economy.SimOrder, error)
}

// CredentialIssuer gives an agent its Know Your Agent credential (B30.5).
type CredentialIssuer interface {
	Current(ctx context.Context, workspaceID, agentID string) (kya.Credential, error)
}

// SetCredentials lets agents show their Know Your Agent credential with wallet_credential (B30.5).
func (s *Server) SetCredentials(c CredentialIssuer) { s.credentials = c }

// walletRefusals are the store's answers that say no to what the agent asked, rather than that Lens failed.
var walletRefusals = []error{
	economy.ErrAgentRule, economy.ErrApprovalRequired, economy.ErrAgentFunds, economy.ErrAgentNotFound, economy.ErrSameAgent,
	economy.ErrAgentOwnerless, economy.ErrCapabilityNotCleared, economy.ErrOwnerUnverified, economy.ErrRequestNotFound,
	economy.ErrTransferNotFound, economy.ErrAlreadyRefunded, economy.ErrNoCreditLine, economy.ErrCreditLineCompaniesOnly,
	economy.ErrLoanNotFound, economy.ErrLoanTerms, economy.ErrLoanCompaniesOnly, economy.ErrEscrowNotFound, economy.ErrEscrowTerms,
	economy.ErrPotNotFound, economy.ErrPotLocked, economy.ErrPot, economy.ErrLiveTrading, economy.ErrNoQuote,
	economy.ErrPortfolioNotFound, economy.ErrSimOrderNotFound, economy.ErrSimOrder, economy.ErrSimHoldings,
	economy.ErrCurrencyAccount, economy.ErrCurrencyAccountExists, economy.ErrCompanyAccountNeeded, economy.ErrAccountNotOpened,
	economy.ErrMoneyAccountNotFound, economy.ErrMoneyAccountNotOpen, economy.ErrNoAccountDetails,
	economy.ErrOutsidePayment, economy.ErrPayeeNotFound, economy.ErrPayeeUnconfirmed, economy.ErrIdempotencyKeyReused,
	screening.ErrBlocked, screening.ErrHeld, screening.ErrListsStale, screening.ErrRefused, screening.ErrNotWhatWasReleased,
	economy.ErrInvoice, economy.ErrInvoiceNotFound, economy.ErrInvoiceState, economy.ErrInvoiceShort, economy.ErrInvoicePayerPaused,
	workspace.ErrMoneyWall,
}

func isWalletTool(name string) bool { return strings.HasPrefix(name, "wallet_") }

func walletToolDefinitions() []map[string]any {
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	num := func(d string) map[string]any { return map[string]any{"type": "integer", "description": d} }
	tool := func(name, desc string, props map[string]any, required ...string) map[string]any {
		if required == nil {
			required = []string{}
		}
		return map[string]any{"name": name, "description": desc,
			"inputSchema": map[string]any{"type": "object", "properties": props, "required": required}}
	}
	amount := num("the amount in µLXC (1 LXC = 1,000,000 µLXC)")
	address := str("a wallet: its id, or its @handle")
	accept := map[string]any{"type": "boolean", "description": "true to accept, false to decline"}
	none := map[string]any{}
	return []map[string]any{
		tool("wallet_send", "Send credits from your wallet to any agent's wallet on Talyvor, within your spending rules.",
			map[string]any{"to": address, "amount_ulxc": amount, "memo": str("what it is for")}, "to", "amount_ulxc"),
		tool("wallet_request", "Ask another agent's wallet for credits; it accepts or declines.",
			map[string]any{"from": address, "amount_ulxc": amount, "memo": str("what it is for")}, "from", "amount_ulxc"),
		tool("wallet_requests", "The requests for credits you made and were made.", none),
		tool("wallet_answer_request", "Accept (pay, within your spending rules) or decline a request made of you.",
			map[string]any{"request_id": str("the request"), "accept": accept}, "request_id", "accept"),
		tool("wallet_refund", "Give a transfer you received back to its sender, once.",
			map[string]any{"transfer_id": str("the transfer you received")}, "transfer_id"),
		tool("wallet_credit_line", "Your company's credit line from Talyvor: its limit, what is used and what is available.", none),
		tool("wallet_offer_loan", "Offer another company's agent a loan in credits, repaid in equal instalments.",
			map[string]any{"to": address, "principal_ulxc": amount, "interest_bps": num("interest on the principal over the whole term, in basis points (1000 = 10%)"),
				"instalments": num("how many equal instalments, 1 to 120"), "every": str("day, week or month"),
				"late_fee_ulxc": num("added to an instalment that is late, in µLXC"), "memo": str("what it is for")},
			"to", "principal_ulxc", "instalments", "every"),
		tool("wallet_loans", "The loans you lent and borrowed, with every instalment.", none),
		tool("wallet_answer_loan", "Accept (the principal is paid to you) or decline a loan offered to you.",
			map[string]any{"loan_id": str("the loan"), "accept": accept}, "loan_id", "accept"),
		tool("wallet_escrow_pay", "Pay into escrow for another agent: held until you confirm delivery, or released at release_at unless you dispute it.",
			map[string]any{"to": address, "amount_ulxc": amount, "release_at": str("RFC 3339 time it is released at, undisputed"), "memo": str("what the deal is")},
			"to", "amount_ulxc", "release_at"),
		tool("wallet_escrows", "The escrows you paid into and are owed, with each state.", none),
		tool("wallet_escrow_confirm", "Confirm delivery on an escrow you paid into: it is released to the payee now.",
			map[string]any{"escrow_id": str("the escrow")}, "escrow_id"),
		tool("wallet_escrow_dispute", "Dispute an escrow you paid into, before its deadline: it is held until Talyvor's operator decides.",
			map[string]any{"escrow_id": str("the escrow"), "reason": str("what went wrong")}, "escrow_id", "reason"),
		tool("wallet_pots", "Your pots and what each holds.", none),
		tool("wallet_pot_create", "Make a pot to keep credits aside: a goal, a budget or a reserve, optionally locked until a date.",
			map[string]any{"name": str("its name"), "kind": str("goal, budget or reserve"), "target_ulxc": num("a goal's target, in µLXC"),
				"locked_until": str("RFC 3339 time nothing can be taken out before")}, "name", "kind"),
		tool("wallet_pot_move", "Move credits into a pot from your balance, or out of it back (unless it is locked).",
			map[string]any{"pot_id": str("the pot"), "amount_ulxc": amount, "direction": str("in or out")}, "pot_id", "amount_ulxc", "direction"),
		tool("wallet_quotes", "The simulated market's instruments and prices in USD, from the ECB's reference rates.", none),
		tool("wallet_portfolio_open", "Open a simulated portfolio with simulated US dollars. Simulated: no money or credits move.",
			map[string]any{"name": str("its name"), "cash_uusd": num("simulated cash in µUSD")}, "name", "cash_uusd"),
		tool("wallet_portfolios", "Your simulated portfolios: cash, positions and value at the latest quotes, and every order.", none),
		tool("wallet_order", "Place a market or limit order in one of your simulated portfolios. Simulated: nothing is sent to a market.",
			map[string]any{"portfolio_id": str("the portfolio"), "instrument": str("an instrument from wallet_quotes, e.g. EUR"),
				"side": str("buy or sell"), "type": str("market or limit"), "quantity_micros": num("units × 1,000,000"),
				"limit_price_usd": str("for a limit order: USD per unit"), "mode": str("simulated (the only mode)")},
			"portfolio_id", "instrument", "side", "type", "quantity_micros"),
		tool("wallet_order_cancel", "Cancel an open order in one of your simulated portfolios.",
			map[string]any{"portfolio_id": str("the portfolio"), "order_id": str("the order")}, "portfolio_id", "order_id"),
		tool("wallet_accounts", "Your accounts in pounds, euros and dollars and what each holds, test and live money apart.", none),
		tool("wallet_account_open", "Open your account in GBP, EUR or USD: a sub-account of your company's in that currency, which opens first. "+
			"It opens at zero.", map[string]any{"currency": str("GBP, EUR or USD"), "funding": str("test (the default) or live")}, "currency"),
		tool("wallet_account_details", "The details a payer pays into your account in GBP, EUR or USD: your company's account details and "+
			"your payment reference, which the payer quotes so the money reaches you. Mode TEST answers carry no details: they come once a partner gives real ones.",
			map[string]any{"currency": str("GBP, EUR or USD")}, "currency"),
		tool("wallet_pay_external", "Pay a person or company outside Talyvor, saved by your owner as a payee, from your account in the "+
			"currency, within your spending rules: one above your approval amount waits for your owner's approval, then ask again with the "+
			"same idempotency_key. The payment is pending, sent, completed, or failed or returned with the money back to you.",
			map[string]any{"payee_id": str("the saved payee (payee_…)"), "amount_minor": num("the amount in pence or cents"),
				"currency": str("GBP, EUR or USD"), "reference": str("what the payee sees, up to 140 characters"),
				"idempotency_key": str("your own unique id for this payment: the same id again never pays twice"),
				"funding":         str("test (the default) or live")}, "payee_id", "amount_minor", "currency", "idempotency_key"),
		tool("wallet_invoice_create", "Issue an invoice, paid into your account in its currency: lines, a due date and optional VAT. "+
			"It answers a pay_url the customer pays by card, by transfer or from their own agent; send true sends it at once, "+
			"otherwise it waits as a draft.",
			map[string]any{"currency": str("GBP, EUR or USD: your account it is paid into"), "customer_name": str("who it is to"),
				"customer_email": str("where its reminders go"), "customer_address": str("the customer's address"),
				"customer_vat_number": str("the customer's VAT number"), "seller_vat_number": str("your company's VAT number"),
				"lines": map[string]any{"type": "array", "description": "what is charged", "items": map[string]any{"type": "object",
					"properties": map[string]any{"description": str("what it is"), "quantity": num("how many"),
						"unit_amount_minor": num("the price of one, in minor units (pence, cents), before VAT"),
						"vat_rate_bps":      num("VAT on it in basis points: 2000 is 20%; 0 or none for no VAT")},
					"required": []string{"description", "quantity", "unit_amount_minor"}}},
				"due_date": str("YYYY-MM-DD"), "remind_days_before": num("a reminder this many days before the due date; 0 sends none"),
				"memo": str("a note on the invoice"), "send": map[string]any{"type": "boolean", "description": "send it now"}},
			"currency", "customer_name", "lines", "due_date"),
		tool("wallet_invoices", "The invoices you issued: draft, sent, overdue, paid or void, what is paid and due, and the reminders sent.", none),
		tool("wallet_invoice_pay", "Pay an invoice from your account in its currency, with its pay link or the token at its end.",
			map[string]any{"pay_token": str("the invoice's pay link, or its token"), "funding": str("test (the default) or live")}, "pay_token"),
		tool("wallet_credential", "Your Know Your Agent credential: a signed token any platform can check against Talyvor's published keys. "+
			"It says who you are, who answers for you and how far they are verified, what you may do with live money and your limits.", none),
	}
}

type walletArgs struct {
	To             string `json:"to"`
	From           string `json:"from"`
	AmountULXC     int64  `json:"amount_ulxc"`
	Memo           string `json:"memo"`
	RequestID      string `json:"request_id"`
	TransferID     string `json:"transfer_id"`
	LoanID         string `json:"loan_id"`
	EscrowID       string `json:"escrow_id"`
	PotID          string `json:"pot_id"`
	PortfolioID    string `json:"portfolio_id"`
	OrderID        string `json:"order_id"`
	Accept         *bool  `json:"accept"`
	Reason         string `json:"reason"`
	ReleaseAt      string `json:"release_at"`
	PrincipalULXC  int64  `json:"principal_ulxc"`
	InterestBPS    int    `json:"interest_bps"`
	Instalments    int    `json:"instalments"`
	Every          string `json:"every"`
	LateFeeULXC    int64  `json:"late_fee_ulxc"`
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	TargetULXC     int64  `json:"target_ulxc"`
	LockedUntil    string `json:"locked_until"`
	Direction      string `json:"direction"`
	CashUUSD       int64  `json:"cash_uusd"`
	Instrument     string `json:"instrument"`
	Side           string `json:"side"`
	Type           string `json:"type"`
	QuantityMicros int64  `json:"quantity_micros"`
	LimitPriceUSD  string `json:"limit_price_usd"`
	Mode           string `json:"mode"`
	Currency       string `json:"currency"`
	Funding        string `json:"funding"`
	PayeeID        string `json:"payee_id"`
	AmountMinor    int64  `json:"amount_minor"`
	Reference      string `json:"reference"`
	IdempotencyKey string `json:"idempotency_key"`
	PayToken       string `json:"pay_token"`

	raw json.RawMessage // the arguments as sent, for a tool that reads its own
}

func (s *Server) runWalletTool(ctx context.Context, name, workspaceID, agentID string, raw json.RawMessage) (any, error) {
	bank, ok := s.agentBank.(WalletBank)
	if !ok {
		return nil, errors.New("the wallet tools are not configured")
	}
	var a walletArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, &toolRefusal{"invalid arguments: " + err.Error()}
		}
	}
	a.raw = raw
	res, err := s.walletTool(ctx, bank, name, workspaceID, agentID, a)
	if err != nil {
		for _, e := range walletRefusals {
			if errors.Is(err, e) {
				return nil, &toolRefusal{err.Error()}
			}
		}
	}
	return res, err
}

func (s *Server) walletTool(ctx context.Context, bank WalletBank, name, ws, agent string, a walletArgs) (any, error) {
	when := func(field, v string) (*time.Time, error) {
		if v == "" {
			return nil, nil
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, &toolRefusal{field + " must be an RFC 3339 time"}
		}
		return &t, nil
	}
	answer := func() (bool, error) {
		if a.Accept == nil {
			return false, &toolRefusal{"accept is required: true or false"}
		}
		return *a.Accept, nil
	}
	switch name {
	case "wallet_send":
		return bank.SendCredits(ctx, ws, agent, a.To, a.AmountULXC, a.Memo)
	case "wallet_request":
		return bank.RequestCredits(ctx, ws, agent, a.From, a.AmountULXC, a.Memo)
	case "wallet_requests":
		all, err := bank.ListMoneyRequests(ctx, ws)
		mine := []economy.MoneyRequest{}
		for _, r := range all {
			if r.FromAgentID == agent || r.ToAgentID == agent {
				mine = append(mine, r)
			}
		}
		return map[string]any{"requests": mine}, err
	case "wallet_answer_request":
		accept, err := answer()
		if err != nil {
			return nil, err
		}
		all, err := bank.ListMoneyRequests(ctx, ws)
		if err != nil {
			return nil, err
		}
		for _, r := range all {
			if r.ID == a.RequestID && r.ToAgentID == agent && r.ToWorkspaceID == ws {
				return bank.AnswerMoneyRequest(ctx, ws, r.ID, accept)
			}
		}
		return nil, economy.ErrRequestNotFound
	case "wallet_refund":
		received, err := bank.ListAgentTransfers(ctx, ws, agent)
		if err != nil {
			return nil, err
		}
		for _, t := range received {
			if t.ID == a.TransferID && t.ToAgentID == agent {
				return bank.RefundTransfer(ctx, ws, t.ID)
			}
		}
		return nil, economy.ErrTransferNotFound
	case "wallet_credit_line":
		return bank.CreditLine(ctx, ws)
	case "wallet_offer_loan":
		return bank.OfferLoan(ctx, ws, agent, a.To, economy.LoanTerms{PrincipalULXC: a.PrincipalULXC, InterestBPS: a.InterestBPS,
			Instalments: a.Instalments, Every: a.Every, LateFeeULXC: a.LateFeeULXC, Memo: a.Memo})
	case "wallet_loans":
		all, err := bank.ListLoans(ctx, ws)
		mine := []economy.Loan{}
		for _, l := range all {
			if (l.LenderWorkspaceID == ws && l.LenderAgentID == agent) || (l.BorrowerWorkspaceID == ws && l.BorrowerAgentID == agent) {
				mine = append(mine, l)
			}
		}
		return map[string]any{"loans": mine}, err
	case "wallet_answer_loan":
		accept, err := answer()
		if err != nil {
			return nil, err
		}
		l, err := bank.GetLoan(ctx, ws, a.LoanID)
		if err != nil {
			return nil, err
		}
		if l.BorrowerWorkspaceID != ws || l.BorrowerAgentID != agent {
			return nil, economy.ErrLoanNotFound
		}
		return bank.AnswerLoan(ctx, ws, l.ID, accept)
	case "wallet_escrow_pay":
		at, err := when("release_at", a.ReleaseAt)
		if err != nil {
			return nil, err
		}
		if at == nil {
			return nil, &toolRefusal{"release_at is required"}
		}
		return bank.PayIntoEscrow(ctx, ws, agent, a.To, a.AmountULXC, a.Memo, *at)
	case "wallet_escrows":
		all, err := bank.ListEscrows(ctx, ws)
		mine := []economy.Escrow{}
		for _, e := range all {
			if (e.PayerWorkspaceID == ws && e.PayerAgentID == agent) || (e.PayeeWorkspaceID == ws && e.PayeeAgentID == agent) {
				mine = append(mine, e)
			}
		}
		return map[string]any{"escrows": mine}, err
	case "wallet_escrow_confirm", "wallet_escrow_dispute":
		e, err := bank.GetEscrow(ctx, ws, a.EscrowID)
		if err != nil {
			return nil, err
		}
		if e.PayerWorkspaceID != ws || e.PayerAgentID != agent {
			return nil, economy.ErrEscrowNotFound
		}
		if name == "wallet_escrow_confirm" {
			return bank.ConfirmEscrow(ctx, ws, e.ID)
		}
		return bank.DisputeEscrow(ctx, ws, e.ID, a.Reason)
	case "wallet_pots":
		pots, err := bank.ListPots(ctx, ws, agent)
		return map[string]any{"pots": pots}, err
	case "wallet_pot_create":
		until, err := when("locked_until", a.LockedUntil)
		if err != nil {
			return nil, err
		}
		return bank.CreatePot(ctx, ws, agent, a.Name, a.Kind, a.TargetULXC, until)
	case "wallet_pot_move":
		switch a.Direction {
		case "in":
			return bank.MoveToPot(ctx, ws, agent, a.PotID, a.AmountULXC)
		case "out":
			return bank.MoveFromPot(ctx, ws, agent, a.PotID, a.AmountULXC)
		}
		return nil, &toolRefusal{"direction must be in or out"}
	case "wallet_quotes":
		return bank.SimQuotes(ctx)
	case "wallet_portfolio_open":
		return bank.OpenPortfolio(ctx, ws, agent, a.Name, a.CashUUSD)
	case "wallet_portfolios":
		list, err := bank.ListPortfolios(ctx, ws, agent)
		return map[string]any{"portfolios": list, "notice": economy.SimulatedNotice}, err
	case "wallet_order":
		return bank.PlaceSimOrder(ctx, ws, agent, a.PortfolioID, economy.SimOrderInput{Instrument: a.Instrument, Side: a.Side, Type: a.Type,
			QuantityMicros: a.QuantityMicros, LimitPriceUSD: a.LimitPriceUSD, Mode: a.Mode})
	case "wallet_order_cancel":
		return bank.CancelSimOrder(ctx, ws, agent, a.PortfolioID, a.OrderID)
	case "wallet_accounts": // B30.13
		accounts, err := bank.CurrencyAccounts(ctx, ws, agent)
		return map[string]any{"accounts": accounts}, err
	case "wallet_account_open":
		return bank.OpenCurrencyAccount(ctx, ws, agent, a.Currency, a.Funding)
	case "wallet_account_details": // B30.14
		accounts, err := bank.CurrencyAccounts(ctx, ws, agent)
		if err != nil {
			return nil, err
		}
		for _, acct := range accounts {
			if acct.Currency == strings.ToUpper(strings.TrimSpace(a.Currency)) && acct.Status == economy.MoneyOpen {
				return bank.CurrencyAccountDetails(ctx, ws, agent, acct.ID)
			}
		}
		return nil, fmt.Errorf("%w: you have no open %s account; open it with wallet_account_open", economy.ErrMoneyAccountNotFound, a.Currency)
	case "wallet_pay_external": // B30.17
		return bank.PayOutside(ctx, ws, economy.OutsidePayment{AgentID: agent, PayeeID: a.PayeeID, AmountMinor: a.AmountMinor,
			Currency: a.Currency, Funding: a.Funding, Reference: a.Reference, IdempotencyKey: a.IdempotencyKey})
	case "wallet_invoice_create": // B30.20
		var r economy.InvoiceRequest
		if err := json.Unmarshal(a.raw, &r); err != nil {
			return nil, &toolRefusal{"invalid arguments: " + err.Error()}
		}
		return bank.CreateInvoice(ctx, ws, agent, r)
	case "wallet_invoices":
		invoices, err := bank.Invoices(ctx, ws, agent)
		return map[string]any{"invoices": invoices}, err
	case "wallet_invoice_pay":
		return bank.PayInvoiceByAgent(ctx, a.PayToken[strings.LastIndex(a.PayToken, "/")+1:], ws, agent, a.Funding)
	case "wallet_credential": // B30.5
		if s.credentials == nil {
			return nil, errors.New("the Know Your Agent credentials are not configured")
		}
		c, err := s.credentials.Current(ctx, ws, agent)
		var standing *kya.StandingError
		if errors.As(err, &standing) {
			return nil, &toolRefusal{standing.Error()}
		}
		return c, err
	}
	return nil, &toolRefusal{"unknown wallet tool: " + name}
}
