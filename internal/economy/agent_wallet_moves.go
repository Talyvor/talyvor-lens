package economy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
)

// agent_wallet_moves.go — B48.8: TOPPING UP AND CASHING OUT AN AGENT'S WALLET.
//
// An owner funds an agent's wallet four ways: by paying into its receive details (the money layer's webhook posts it,
// B46.4); in one tap from their own wallet or another of their agents' (route wallet); by buying USDC with a card or a
// bank transfer through the ramp partner (card, bank_transfer); or by converting money in one of their fiat wallets
// through the ramp (convert). They cash out the other way: the ramp sells the agent's USDC into their own bank account,
// a saved payee whose bank confirmed the name exactly, once they are verified (cash_out).
//
// Each one is quoted first (QuoteWalletMove): what it sells, the partner's fee, the rate and what it buys, kept in
// wallet_moves. Confirming it (ConfirmWalletMove) asks the partners under the move's id, so a retry moves nothing twice,
// and posts one money entry under the move's id, so it posts once, at the quoted amounts, in both wallets' history.
// A move between the owner's own wallets needs no approval: the money layer's gate lets through exactly the move
// quoted (heldWalletMove), never the same key to another wallet or for another amount. A cash-out is judged by the
// agent's rules as any payment out is, so above its approval amount it waits for the owner's signed approval. Only the
// agent's owner moves money out of its wallet; a cash-out is screened against the bank account's holder.
//
// Once money has left — paid in to the ramp, a card charged, the ramp executed — no refusal fails a move: it is held
// (status held), where the owner sees it and the operators list it (HeldWalletMoves), until it is confirmed again to its
// end or, while the ramp has not executed it, the owner has the money given back (ReturnWalletMove).
//
// The USDC a ramp buys arrives in the agent's wallet as a deposit, which the money layer's webhook reports
// (ReceivePayment). A top-up posts once, under that deposit's key, whichever sees it first: the confirm, when the money
// layer knows the ramp's reference as a deposit, or the webhook (deliverWalletMove).

// The routes money comes into or leaves an agent's wallet by.
const (
	WalletMoveWallet       = "wallet"
	WalletMoveCard         = partners.RampCard
	WalletMoveBankTransfer = partners.RampBankTransfer
	WalletMoveConvert      = "convert"
	WalletMoveCashOut      = "cash_out"
)

var (
	// ErrWalletMove: a top-up or cash-out that cannot be made as asked.
	ErrWalletMove = errors.New("economy: wallet top-up or cash-out")
	// ErrWalletMoveNotFound: no such top-up or cash-out for this agent.
	ErrWalletMoveNotFound = errors.New("economy: no such top-up or cash-out")
	// ErrWalletMoveExpired: a quote confirmed after its partner stopped honouring it.
	ErrWalletMoveExpired = errors.New("economy: the quote has expired: ask for a new one")
	// ErrNoRampPartner: no ramp is set, so nothing buys or sells USDC.
	ErrNoRampPartner = errors.New("economy: buying or selling USDC needs a ramp partner, and none is configured")
)

// SetRampPartner is who buys USDC for agents' wallets and sells it for money (B48.8); nil, only route wallet works.
//
// shortcut: one ramp for every workspace and country, the Test one until a partner is chosen; move it behind the
// partners Registry's clearances when a live ramp is added.
func (s *DualTokenStore) SetRampPartner(p partners.RampPartner) { s.rampPartner = p }

// WalletMoveRequest asks for a quote. AmountMinor of Currency is what is sold: the money moved (wallet), paid by card or
// bank (card, bank_transfer), taken from the fiat wallet (convert), or the USDC cashed out (cash_out).
type WalletMoveRequest struct {
	Route       string `json:"route"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	FromAgentID string `json:"from_agent_id,omitempty"` // wallet, convert: the owner's other agent that pays; "" the owner's wallet
	PayeeID     string `json:"payee_id,omitempty"`      // cash_out: the owner's bank account
	ToCurrency  string `json:"to_currency,omitempty"`   // cash_out: GBP, EUR or USD
}

// WalletMove is one top-up or cash-out, as quoted and as it ended.
type WalletMove struct {
	ID            string     `json:"id"`
	WorkspaceID   string     `json:"workspace_id"`
	AgentID       string     `json:"agent_id"`
	Route         string     `json:"route"`
	FromAccountID string     `json:"from_account_id,omitempty"`
	ToAccountID   string     `json:"to_account_id,omitempty"`
	PayeeID       string     `json:"payee_id,omitempty"`
	SellMinor     int64      `json:"sell_minor"`
	SellCurrency  string     `json:"sell_currency"`
	FeeMinor      int64      `json:"fee_minor"` // the partner's, in SellCurrency, taken from what is sold
	BuyMinor      int64      `json:"buy_minor"`
	BuyCurrency   string     `json:"buy_currency"`
	Rate          string     `json:"rate"` // BuyCurrency per SellCurrency, in whole units
	Partner       string     `json:"partner"`
	PartnerRef    string     `json:"partner_ref,omitempty"`
	Funding       string     `json:"funding"`
	Status        string     `json:"status"` // quoted | posted | failed | held | returned
	Detail        string     `json:"detail,omitempty"`
	EntryID       string     `json:"entry_id,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	PostedAt      *time.Time `json:"posted_at,omitempty"`
	HeldAt        *time.Time `json:"held_at,omitempty"`

	fromRef, toRef, quoteRef, payInName, payInAddress, stage string
}

// How far a quoted move got (wallet_moves.stage): waiting for the owner's approval with nothing moved, or a partner step
// begun, after which the move is replayed to its end whatever its quote's expiry says.
const (
	walletMoveAwaitingApproval = "approval"
	walletMoveStarted          = "started"
)

const walletMoveCols = `id, workspace_id, agent_id, route, from_account_id, to_account_id, payee_id, sell_minor, sell_currency, fee_minor,
	buy_minor, buy_currency, rate, partner, partner_ref, funding, status, detail, entry_id, expires_at, created_at, posted_at,
	from_ref, to_ref, quote_ref, pay_in_name, pay_in_address, stage, held_at`

func scanWalletMove(row pgx.Row) (WalletMove, error) {
	var m WalletMove
	err := row.Scan(&m.ID, &m.WorkspaceID, &m.AgentID, &m.Route, &m.FromAccountID, &m.ToAccountID, &m.PayeeID, &m.SellMinor, &m.SellCurrency,
		&m.FeeMinor, &m.BuyMinor, &m.BuyCurrency, &m.Rate, &m.Partner, &m.PartnerRef, &m.Funding, &m.Status, &m.Detail, &m.EntryID,
		&m.ExpiresAt, &m.CreatedAt, &m.PostedAt, &m.fromRef, &m.toRef, &m.quoteRef, &m.payInName, &m.payInAddress, &m.stage, &m.HeldAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrWalletMoveNotFound
	}
	return m, err
}

// moveWallet is a wallet a move pays from or into: the owner's (agentID "") or an agent's, which must be of a kind
// that holds asset; with the partner's reference for it.
func (s *DualTokenStore) moveWallet(ctx context.Context, workspaceID, agentID, asset string) (AgentWallet, string, error) {
	var w AgentWallet
	var err error
	if agentID == "" {
		w, err = s.ownerWallet(ctx, workspaceID, asset)
	} else {
		kind, fiat, kerr := s.agentWalletKind(ctx, workspaceID, agentID)
		if kerr != nil {
			return AgentWallet{}, "", kerr
		}
		if !slices.Contains(walletKindAssets(kind, fiat), asset) {
			return AgentWallet{}, "", fmt.Errorf("%w: agent %s has no %s wallet", ErrWalletMove, agentID, asset)
		}
		w, err = s.agentWalletIn(ctx, workspaceID, agentID, asset)
	}
	if err != nil {
		return AgentWallet{}, "", err
	}
	var ref string
	if err := s.pool.QueryRow(ctx, `SELECT partner_account_ref FROM money_accounts WHERE id = $1`, w.AccountID).Scan(&ref); err != nil {
		return AgentWallet{}, "", fmt.Errorf("economy: wallet move: %w", err)
	}
	if ref == "" {
		return AgentWallet{}, "", fmt.Errorf("%w: the %s wallet is held at no partner", ErrWalletMove, asset)
	}
	return w, ref, nil
}

// agentOwner is agentID's owner, refusing an archived agent or one nobody has claimed.
func (s *DualTokenStore) agentOwner(ctx context.Context, workspaceID, agentID string) (string, error) {
	var owner string
	err := s.pool.QueryRow(ctx, `SELECT owner_user_id FROM agent_accounts WHERE id = $1 AND workspace_id = $2 AND archived_at IS NULL`,
		agentID, workspaceID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrAgentNotFound
	}
	if err != nil {
		return "", fmt.Errorf("economy: wallet move: %w", err)
	}
	if owner == "" {
		return "", fmt.Errorf("%w: agent %s has no owner yet: claim it first", ErrWalletMove, agentID)
	}
	return owner, nil
}

// fromWallet is the wallet a wallet or convert move pays from: the owner's, or one of their own agents' — never an
// agent another person owns.
func (s *DualTokenStore) fromWallet(ctx context.Context, workspaceID, owner, fromAgentID, asset string) (AgentWallet, string, error) {
	if fromAgentID != "" {
		other, err := s.agentOwner(ctx, workspaceID, fromAgentID)
		if err != nil {
			return AgentWallet{}, "", err
		}
		if other != owner {
			return AgentWallet{}, "", fmt.Errorf("%w: money moves in one tap only between one owner's own wallets", ErrWalletMove)
		}
	}
	return s.moveWallet(ctx, workspaceID, fromAgentID, asset)
}

func isFiat(c string) bool { return c == CurrencyGBP || c == CurrencyEUR || c == CurrencyUSD }

func testPartner(name string) bool { return name == money.SandboxName || name == "test" }

// QuoteWalletMove prices a top-up or cash-out of agentID's wallet, and keeps the quote to be confirmed. by is who asks;
// workspaceOwner whether they are the workspace's owner, the only one who moves money out of the workspace's own wallets.
func (s *DualTokenStore) QuoteWalletMove(ctx context.Context, workspaceID, agentID string, r WalletMoveRequest, by string,
	workspaceOwner bool) (WalletMove, error) {
	if s.moneyLayer == nil {
		return WalletMove{}, ErrAgentWalletsOff
	}
	r.Currency, r.ToCurrency = strings.ToUpper(strings.TrimSpace(r.Currency)), strings.ToUpper(strings.TrimSpace(r.ToCurrency))
	if r.Route == WalletMoveCashOut && r.Currency == "" {
		r.Currency = money.USDC
	}
	if r.AmountMinor <= 0 {
		return WalletMove{}, fmt.Errorf("%w: amount_minor is positive", ErrWalletMove)
	}
	owner, err := s.agentOwner(ctx, workspaceID, agentID)
	if err != nil {
		return WalletMove{}, err
	}
	if err := mayMove(r.Route, r.FromAgentID, by, owner, workspaceOwner); err != nil {
		return WalletMove{}, err
	}
	m := WalletMove{ID: "wmove_" + uuid.NewString(), WorkspaceID: workspaceID, AgentID: agentID, Route: r.Route, SellMinor: r.AmountMinor,
		SellCurrency: r.Currency, BuyMinor: r.AmountMinor, BuyCurrency: r.Currency, Rate: "1", Status: "quoted"}
	var ramp partners.RampQuoteRequest
	var partnerNames []string
	switch r.Route {
	case WalletMoveWallet:
		if !isFiat(r.Currency) && r.Currency != money.USDC {
			return WalletMove{}, fmt.Errorf("%w: a wallet holds GBP, EUR, USD or USDC, not %q", ErrWalletMove, r.Currency)
		}
		to, toRef, err := s.moveWallet(ctx, workspaceID, agentID, r.Currency)
		if err != nil {
			return WalletMove{}, err
		}
		from, fromRef, err := s.fromWallet(ctx, workspaceID, owner, r.FromAgentID, r.Currency)
		if err != nil {
			return WalletMove{}, err
		}
		if from.AccountID == to.AccountID {
			return WalletMove{}, fmt.Errorf("%w: money moves from one wallet to another", ErrWalletMove)
		}
		if from.Partner != to.Partner {
			return WalletMove{}, fmt.Errorf("%w: the two wallets are at different partners", ErrWalletMove)
		}
		m.FromAccountID, m.fromRef, m.ToAccountID, m.toRef, m.Partner = from.AccountID, fromRef, to.AccountID, toRef, to.Partner
		partnerNames = []string{to.Partner}
	case WalletMoveCard, WalletMoveBankTransfer, WalletMoveConvert:
		if !isFiat(r.Currency) {
			return WalletMove{}, fmt.Errorf("%w: USDC is bought with GBP, EUR or USD, not %q", ErrWalletMove, r.Currency)
		}
		to, toRef, err := s.moveWallet(ctx, workspaceID, agentID, money.USDC)
		if err != nil {
			return WalletMove{}, err
		}
		m.ToAccountID, m.toRef, partnerNames = to.AccountID, toRef, []string{to.Partner}
		ramp = partners.RampQuoteRequest{Sell: partners.Money{Minor: r.AmountMinor, Currency: r.Currency}, Buy: money.USDC, Method: r.Route}
		if r.Route == WalletMoveConvert {
			from, fromRef, err := s.fromWallet(ctx, workspaceID, owner, r.FromAgentID, r.Currency)
			if err != nil {
				return WalletMove{}, err
			}
			m.FromAccountID, m.fromRef, ramp.Method = from.AccountID, fromRef, partners.RampWallet
			partnerNames = append(partnerNames, from.Partner)
		}
	case WalletMoveCashOut:
		if r.Currency != money.USDC || !isFiat(r.ToCurrency) {
			return WalletMove{}, fmt.Errorf("%w: a cash-out sells USDC for GBP, EUR or USD (to_currency)", ErrWalletMove)
		}
		from, fromRef, err := s.moveWallet(ctx, workspaceID, agentID, money.USDC)
		if err != nil {
			return WalletMove{}, err
		}
		m.FromAccountID, m.fromRef, m.PayeeID, partnerNames = from.AccountID, fromRef, r.PayeeID, []string{from.Partner}
		ramp = partners.RampQuoteRequest{Sell: partners.Money{Minor: r.AmountMinor, Currency: money.USDC}, Buy: r.ToCurrency, Method: partners.RampWallet}
	default:
		return WalletMove{}, fmt.Errorf("%w: route is wallet, card, bank_transfer, convert or cash_out, not %q", ErrWalletMove, r.Route)
	}
	if ramp.Method != "" {
		if s.rampPartner == nil {
			return WalletMove{}, ErrNoRampPartner
		}
		ramp.ID = m.ID
		q, err := s.rampPartner.Quote(ctx, ramp)
		if errors.Is(err, partners.ErrInvalid) {
			return WalletMove{}, fmt.Errorf("%w: %v", ErrWalletMove, err)
		}
		if err != nil {
			return WalletMove{}, fmt.Errorf("%w: %v", money.ErrPartnerDown, err)
		}
		m.FeeMinor, m.BuyMinor, m.BuyCurrency, m.Rate, m.Partner = q.Fee.Minor, q.Buy.Minor, q.Buy.Currency, q.Rate, s.rampPartner.Name()
		m.quoteRef, m.payInName, m.payInAddress, m.ExpiresAt = q.Ref, q.PayInName, q.PayInAddress, &q.ExpiresAt
		partnerNames = append(partnerNames, m.Partner)
	}
	// Money moves live only when every partner it passes through is live; the ledger then asks the capability's clearance.
	m.Funding = FundingLive
	for _, p := range partnerNames {
		if testPartner(p) {
			m.Funding = FundingTest
		}
	}
	if m.Route == WalletMoveCashOut {
		if err := s.checkCashOutPayee(ctx, workspaceID, r.PayeeID, m.Funding == FundingLive, time.Now()); err != nil {
			return WalletMove{}, err
		}
	}
	err = s.pool.QueryRow(ctx, `INSERT INTO wallet_moves (id, workspace_id, agent_id, route, from_account_id, to_account_id, from_ref, to_ref,
		payee_id, sell_minor, sell_currency, fee_minor, buy_minor, buy_currency, rate, partner, quote_ref, pay_in_name, pay_in_address, funding,
		expires_at, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
		RETURNING created_at`, m.ID, workspaceID, agentID, m.Route, m.FromAccountID, m.ToAccountID, m.fromRef, m.toRef, m.PayeeID, m.SellMinor,
		m.SellCurrency, m.FeeMinor, m.BuyMinor, m.BuyCurrency, m.Rate, m.Partner, m.quoteRef, m.payInName, m.payInAddress, m.Funding,
		m.ExpiresAt, by).Scan(&m.CreatedAt)
	if err != nil {
		return WalletMove{}, fmt.Errorf("economy: wallet move: %w", err)
	}
	return m, nil
}

// mayMove refuses by a move of route paying from fromAgentID's wallet ("" the workspace's own), of an agent owner owns:
// only the agent's owner moves money out of an agent's wallet, and only the workspace's owner out of the workspace's.
func mayMove(route, fromAgentID, by, owner string, workspaceOwner bool) error {
	if (fromAgentID != "" || route == WalletMoveCashOut) && by != owner {
		return fmt.Errorf("%w: only the agent's owner moves money out of its wallet", ErrWalletMove)
	}
	if (route == WalletMoveWallet || route == WalletMoveConvert) && fromAgentID == "" && !workspaceOwner {
		return fmt.Errorf("%w: only the workspace's owner moves money out of the workspace's own wallets", ErrWalletMove)
	}
	return nil
}

// mayConfirm is mayMove for move m, quoted already: confirming it or having its money back is the same person's as
// quoting it.
func (s *DualTokenStore) mayConfirm(ctx context.Context, m WalletMove, by string, workspaceOwner bool) error {
	owner, err := s.agentOwner(ctx, m.WorkspaceID, m.AgentID)
	if err != nil {
		return err
	}
	from, err := s.walletMoveFromRoute(ctx, m)
	if err != nil {
		return err
	}
	fromAgent := from.Agent
	if m.Route == WalletMoveCashOut || m.FromAccountID == "" {
		fromAgent = ""
	}
	return mayMove(m.Route, fromAgent, by, owner, workspaceOwner)
}

// checkCashOutPayee refuses a cash-out unless payeeID is the owner's own bank account: a saved payee whose bank
// confirmed its name exactly, and that name is the person whose identity check (L2) passed — for live money, a check a
// real provider passed — and whose details have not changed since the move was quoted (quotedAt) nor in the last 24 hours.
func (s *DualTokenStore) checkCashOutPayee(ctx context.Context, workspaceID, payeeID string, live bool, quotedAt time.Time) error {
	var name, check, address string
	var changed *time.Time
	err := s.pool.QueryRow(ctx, `SELECT name, check_result, address, details_changed_at FROM money_payees WHERE id = $1 AND workspace_id = $2`,
		payeeID, workspaceID).Scan(&name, &check, &address, &changed)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: payee_id names none of this workspace's saved bank accounts", ErrWalletMove)
	}
	if err != nil {
		return fmt.Errorf("economy: wallet move: %w", err)
	}
	if check != "exact_match" || address != "" {
		return fmt.Errorf("%w: cash out goes only to a bank account whose bank confirmed the name exactly", ErrWalletMove)
	}
	if changed != nil && (!changed.Before(quotedAt) || time.Since(*changed) < 24*time.Hour) {
		return fmt.Errorf("%w: the bank account's details changed: cash out to it 24 hours after the change, quoted again", ErrWalletMove)
	}
	checks, err := readVerificationChecks(ctx, s.pool, workspaceID)
	if err != nil {
		return err
	}
	if live {
		checks = slices.DeleteFunc(checks, func(c VerificationCheck) bool { return c.Test })
	}
	if person, named := identityNamed(checks, []string{name}); person == "" {
		return fmt.Errorf("%w: the owner must be verified to cash out", ErrWalletMove)
	} else if !named {
		return fmt.Errorf("%w: cash out goes only to the owner's own bank account, in %s's name", ErrWalletMove, person)
	}
	return nil
}

// mirrorAccount is the partner account that mirrors accountID at its partner.
func (s *DualTokenStore) mirrorAccount(ctx context.Context, accountID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT p.id FROM money_accounts a JOIN money_accounts p ON p.workspace_id = a.workspace_id
		AND p.partner_account_ref = a.partner_account_ref AND p.purpose = 'partner' WHERE a.id = $1`, accountID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("economy: wallet move: the partner account mirroring %s: %w", accountID, err)
	}
	return id, nil
}

// walletMoveEntry is the one money entry m posts: money between the two wallets, or between a wallet and the partner
// account mirroring it, where the ramp takes it or delivers it.
func (s *DualTokenStore) walletMoveEntry(ctx context.Context, m WalletMove) (MoneyEntry, error) {
	e := MoneyEntry{WorkspaceID: m.WorkspaceID, Capability: CapabilityFX, Kind: "wallet_" + m.Route, IdempotencyKey: "wallet_move:" + m.ID,
		Funding: m.Funding, Counterparty: m.Partner, Memo: "Top-up"}
	if m.Route == WalletMoveCashOut { // screened against whose bank account it pays, before the ramp pays it
		e.Memo = "Cash out"
		if err := s.pool.QueryRow(ctx, `SELECT name FROM money_payees WHERE id = $1 AND workspace_id = $2`, m.PayeeID, m.WorkspaceID).
			Scan(&e.Counterparty); err != nil {
			return MoneyEntry{}, fmt.Errorf("economy: wallet move: the payee: %w", err)
		}
	}
	if m.FromAccountID != "" {
		e.Postings = append(e.Postings, MoneyPosting{AccountID: m.FromAccountID, AmountMinor: -m.SellMinor, Currency: m.SellCurrency})
		if m.Route == WalletMoveWallet {
			e.Capability, e.Counterparty, e.Memo = agentWalletRoute("", m.AgentID, m.SellCurrency).Capability, "", "Moved between your wallets"
		} else {
			mirror, err := s.mirrorAccount(ctx, m.FromAccountID)
			if err != nil {
				return MoneyEntry{}, err
			}
			e.Postings = append(e.Postings, MoneyPosting{AccountID: mirror, AmountMinor: m.SellMinor, Currency: m.SellCurrency})
		}
	}
	if m.ToAccountID != "" {
		if m.Route != WalletMoveWallet {
			mirror, err := s.mirrorAccount(ctx, m.ToAccountID)
			if err != nil {
				return MoneyEntry{}, err
			}
			e.Postings = append(e.Postings, MoneyPosting{AccountID: mirror, AmountMinor: -m.BuyMinor, Currency: m.BuyCurrency})
		}
		e.Postings = append(e.Postings, MoneyPosting{AccountID: m.ToAccountID, AmountMinor: m.BuyMinor, Currency: m.BuyCurrency})
	}
	return e, nil
}

// errWalletMovePending: a partner has not finished; confirming again asks it again under the same id.
// errWalletMoveApproval: the agent's rules wait for the owner's approval, and nothing has moved.
// errWalletMoveDelivering: the ramp bought the USDC, and the money layer has not yet seen it arrive in the wallet.
// errWalletMovePaidIn: the ramp said no to a move whose wallet already paid it in, so the money is at the ramp.
var (
	errWalletMovePending    = errors.New("pending at the partner")
	errWalletMoveApproval   = fmt.Errorf("%w: waiting for your approval", errWalletMovePending)
	errWalletMoveDelivering = fmt.Errorf("%w: the ramp has bought it, and it posts once it arrives in the wallet", errWalletMovePending)
	errWalletMovePaidIn     = errors.New("the ramp refused a move already paid in to it")
)

// runWalletMove asks the partners to move m's money, each under m's id, and answers the last partner's reference — for
// a top-up, the deposit's that delivered it. An error that is money.ErrRefused is the partners' no;
// errWalletMovePaidIn the ramp's no once the wallet paid it in; errWalletMovePending a step not finished; any other is
// no answer.
//
// shortcut: money a wallet paid in to a ramp that then refused stays with the ramp while the move is priced again;
// handle the ramp's refund when a live ramp is added.
func (s *DualTokenStore) runWalletMove(ctx context.Context, m WalletMove) (string, error) {
	ctx = partners.WithWorkspace(ctx, m.WorkspaceID)
	done := walletMoveDone
	amount := money.Amount{Minor: m.SellMinor, Asset: m.SellCurrency}
	fromRoute, err := s.walletMoveFromRoute(ctx, m)
	if err != nil {
		return "", err
	}
	if m.Route == WalletMoveWallet {
		t, err := s.moneyLayer.Transfers(ctx, fromRoute)
		if err != nil {
			return "", err
		}
		return done(t.Internal(ctx, money.Internal{Key: m.ID, From: m.fromRef, To: m.toRef, Amount: amount}))
	}
	if m.FromAccountID != "" { // convert, cash_out: the wallet pays what it sells in to the ramp
		if err := s.payInWalletMove(ctx, m, fromRoute); err != nil {
			return "", err
		}
	}
	// Under the quote too: a move priced again asks the ramp afresh, never with an id it already said no to.
	exec := partners.RampExecution{ID: m.ID + "." + m.quoteRef, QuoteRef: m.quoteRef}
	toRoute := agentWalletRoute(m.WorkspaceID, m.AgentID, money.USDC)
	if m.Route == WalletMoveCashOut {
		var p partners.Payee
		if err := s.pool.QueryRow(ctx, `SELECT name, sort_code, account_number, iban, bic, routing_number FROM money_payees
			WHERE id = $1 AND workspace_id = $2`, m.PayeeID, m.WorkspaceID).Scan(&p.Name, &p.SortCode, &p.AccountNumber, &p.IBAN, &p.BIC,
			&p.RoutingNumber); err != nil {
			return "", fmt.Errorf("economy: wallet move: the payee: %w", err)
		}
		exec.ToPayee = p
	} else {
		w, err := s.moneyLayer.Wallets(ctx, toRoute)
		if err != nil {
			return "", err
		}
		d, err := w.Receive(ctx, m.toRef)
		if err != nil {
			return "", err
		}
		exec.ToAddress = d.Address
	}
	r, err := s.rampPartner.Execute(ctx, exec)
	if err == nil { // the ramp has taken it: its money can no longer be given back
		if _, err := s.pool.Exec(ctx, `UPDATE wallet_moves SET executed_at = COALESCE(executed_at, now()) WHERE id = $1`, m.ID); err != nil {
			return "", fmt.Errorf("economy: wallet move: %w", err)
		}
	}
	var ref string
	switch {
	case errors.Is(err, partners.ErrInvalid) || errors.Is(err, partners.ErrNotFound) || errors.Is(err, partners.ErrIDReused):
		err = fmt.Errorf("%w: %v", money.ErrRefused, err)
	case err != nil:
		return "", fmt.Errorf("%w: %v", money.ErrPartnerDown, err)
	default:
		ref, err = done(money.Result{Ref: r.Ref, Status: money.Status(r.Status), Detail: r.Detail}, nil)
	}
	if m.FromAccountID != "" && errors.Is(err, money.ErrRefused) {
		return "", fmt.Errorf("%w: %v", errWalletMovePaidIn, err)
	}
	if err != nil || m.Route == WalletMoveCashOut {
		return ref, err
	}
	// The USDC the ramp bought arrives at the agent's wallet: asked whether the ramp's reference is a deposit there, a
	// money layer that knows only its own (Circle's transaction id) says no, and the deposit webhook posts it.
	t, err := s.moneyLayer.Transfers(ctx, toRoute)
	if err != nil {
		return "", err
	}
	deposit, err := done(t.Credit(ctx, money.Credit{Key: m.ID, To: m.toRef, Amount: money.Amount{Minor: m.BuyMinor, Asset: m.BuyCurrency},
		PartnerRef: ref}))
	if errors.Is(err, money.ErrRefused) || errors.Is(err, errWalletMovePending) {
		return "", errWalletMoveDelivering
	}
	return deposit, err
}

// walletMoveDone is a money-layer step's answer as runWalletMove's: its reference, or why not.
func walletMoveDone(r money.Result, err error) (string, error) {
	if errors.Is(err, money.ErrApprovalNeeded) {
		return "", errWalletMoveApproval
	}
	for _, refused := range []error{money.ErrRefused, money.ErrInvalid, money.ErrNotFound, money.ErrKeyReused,
		money.ErrInsufficientFunds, money.ErrLimit} {
		if errors.Is(err, refused) {
			return "", fmt.Errorf("%w: %v", money.ErrRefused, err)
		}
	}
	switch {
	case err != nil:
		return "", err
	case r.Status == money.StatusFailed:
		return "", fmt.Errorf("%w: %s", money.ErrRefused, r.Detail)
	case r.Status == money.StatusPending:
		return "", errWalletMovePending
	}
	return r.Ref, nil
}

// payInWalletMove pays what m sells from its wallet in to the ramp, under m's id — asked again, the money layer answers
// the pay-in as it stands — and notes when the money layer answers that it arrived (paid_in_at); it fails as
// runWalletMove's steps do. USDC goes to the ramp's address through B48.7's Travel Rule step, as every payment to an address does: until its
// data is sent, the pay-in is not asked.
func (s *DualTokenStore) payInWalletMove(ctx context.Context, m WalletMove, fromRoute money.Route) error {
	payIn := money.Payee{Name: m.payInName, Address: m.payInAddress}
	if m.SellCurrency == money.USDC {
		w, err := s.moneyLayer.Wallets(ctx, fromRoute)
		if err != nil {
			return err
		}
		d, err := w.Receive(ctx, m.fromRef)
		if err != nil {
			return err
		}
		payIn.Network = d.Network
		if s.travelRule != nil && payIn.Address != "" {
			err := s.sendTravelRule(ctx, m.ID, m.WorkspaceID, m.SellCurrency, m.SellMinor, m.fromRef, m.payInName, payIn.Network, payIn.Address, "")
			if err != nil {
				return fmt.Errorf("%w: the Travel Rule data was not sent: %v", money.ErrPartnerDown, err)
			}
		}
	}
	t, err := s.moneyLayer.Transfers(ctx, fromRoute)
	if err != nil {
		return err
	}
	_, err = walletMoveDone(t.Outbound(ctx, money.Outbound{Key: m.ID, From: m.fromRef, Payee: payIn,
		Amount: money.Amount{Minor: m.SellMinor, Asset: m.SellCurrency}, Reference: m.ID}))
	if err == nil {
		if _, err := s.pool.Exec(ctx, `UPDATE wallet_moves SET paid_in_at = COALESCE(paid_in_at, now()) WHERE id = $1`, m.ID); err != nil {
			return fmt.Errorf("economy: wallet move: %w", err)
		}
	}
	return err
}

// screenPayIn screens the address a cash-out pays its USDC in to the ramp at, as B48.7 screens every address paid.
func (s *DualTokenStore) screenPayIn(ctx context.Context, m WalletMove) error {
	if m.SellCurrency != money.USDC || m.payInAddress == "" {
		return nil
	}
	w, err := s.moneyLayer.Wallets(ctx, agentWalletRoute(m.WorkspaceID, m.AgentID, money.USDC))
	if err != nil {
		return err
	}
	d, err := w.Receive(ctx, m.fromRef)
	if err != nil {
		return err
	}
	_, err = s.screenAddress(ctx, OutsidePayment{WorkspaceID: m.WorkspaceID, IdempotencyKey: "wallet_move:" + m.ID, AmountMinor: m.SellMinor,
		Currency: m.SellCurrency, Funding: m.Funding}, OutsidePayee{Name: m.payInName, Network: d.Network, Address: m.payInAddress})
	return err
}

// isTopUp is whether a move of route buys USDC that a ramp delivers to the agent's wallet.
func isTopUp(route string) bool {
	return route == WalletMoveCard || route == WalletMoveBankTransfer || route == WalletMoveConvert
}

// walletMoveFromRoute is the money layer's route for the wallet m pays from: the agent's, or for a wallet or convert
// move the owner's other agent's or the owner's own.
func (s *DualTokenStore) walletMoveFromRoute(ctx context.Context, m WalletMove) (money.Route, error) {
	rt := agentWalletRoute(m.WorkspaceID, m.AgentID, m.SellCurrency)
	if m.Route != WalletMoveCashOut {
		var fromAgent string
		if err := s.pool.QueryRow(ctx, `SELECT COALESCE(agent_id, '') FROM money_accounts WHERE id = $1`, m.FromAccountID).Scan(&fromAgent); err != nil &&
			!errors.Is(err, pgx.ErrNoRows) {
			return money.Route{}, fmt.Errorf("economy: wallet move: %w", err)
		}
		rt.Agent = fromAgent
	}
	return rt, nil
}

// ConfirmWalletMove makes the move quoted as id, once: asked again, it answers the move as it ended and moves nothing.
// by confirms it as they would quote it (mayMove). A quote past its expiry is refused (ErrWalletMoveExpired) only while
// nothing of it has started: one the owner approved late is priced again, and one a partner began is replayed to its
// end. The partners' no to its first step fails the move (money.ErrRefused); the ramp's, once the wallet paid it in,
// prices it again to wait for the owner's confirm; any other no once money may have left holds it (status held). A held
// move confirmed again is replayed to its end.
func (s *DualTokenStore) ConfirmWalletMove(ctx context.Context, workspaceID, agentID, id, by string, workspaceOwner bool) (WalletMove, error) {
	if s.moneyLayer == nil {
		return WalletMove{}, ErrAgentWalletsOff
	}
	// An owner who hangs up mid-move does not leave a partner's step unrecorded.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	m, err := scanWalletMove(s.pool.QueryRow(ctx, `SELECT `+walletMoveCols+` FROM wallet_moves WHERE id = $1 AND workspace_id = $2 AND agent_id = $3`,
		id, workspaceID, agentID))
	if err != nil || (m.Status != "quoted" && m.Status != "held") {
		return m, err
	}
	if err := s.mayConfirm(ctx, m, by, workspaceOwner); err != nil {
		return WalletMove{}, err
	}
	screened, err := s.walletMoveEntry(ctx, m)
	if err != nil {
		return WalletMove{}, err
	}
	// B50.15: screened before the locks, as PostMoney screens; B48.7: and the address a cash-out pays the ramp at, before
	// it is paid. A refusal is judged under the lock: once money may have left, it holds the move.
	screenErr := s.screenBeforeLocks(ctx, screened)
	if screenErr == nil && m.stage != walletMoveStarted {
		screenErr = s.screenPayIn(ctx, m)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WalletMove{}, fmt.Errorf("economy: wallet move: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One confirm of a move at a time. An advisory lock, not the row's: the row's stage is written, and committed, on its
	// own before each partner step, while this transaction holds the wallet it pays from.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('wallet_moves:' || $1, 0))`, id); err != nil {
		return WalletMove{}, fmt.Errorf("economy: wallet move: %w", err)
	}
	if m, err = scanWalletMove(tx.QueryRow(ctx, `SELECT `+walletMoveCols+` FROM wallet_moves WHERE id = $1`, id)); err != nil ||
		(m.Status != "quoted" && m.Status != "held") {
		return m, err
	}
	// A move a partner step began may have money out: from here no refusal fails it.
	replay := m.stage == walletMoveStarted
	// stage keeps how far the move got, committed at once.
	stage := func(stage string) error {
		if _, err := s.pool.Exec(ctx, `UPDATE wallet_moves SET stage = $2 WHERE id = $1`, id, stage); err != nil {
			return fmt.Errorf("economy: wallet move: %w", err)
		}
		m.stage = stage
		return nil
	}
	fail := func(cause error) (WalletMove, error) {
		if _, err := tx.Exec(ctx, `UPDATE wallet_moves SET status = 'failed', detail = $2 WHERE id = $1`, id, cause.Error()); err != nil {
			return WalletMove{}, fmt.Errorf("economy: wallet move: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return WalletMove{}, fmt.Errorf("economy: wallet move: %w", err)
		}
		m.Status, m.Detail = "failed", cause.Error()
		return m, cause
	}
	// hold keeps a move whose money may have left, for the owner to confirm again or have the money back, and the
	// operators to see: on q, this confirm's transaction, or the pool once it could not commit.
	hold := func(q pgxDB, cause error) (WalletMove, error) {
		m.Detail = "held: " + cause.Error()
		if err := q.QueryRow(ctx, `UPDATE wallet_moves SET status = 'held', detail = $2, held_at = COALESCE(held_at, now()) WHERE id = $1
			RETURNING held_at`, id, m.Detail).Scan(&m.HeldAt); err != nil {
			return WalletMove{}, fmt.Errorf("economy: wallet move: %w", err)
		}
		if q == tx {
			if err := tx.Commit(ctx); err != nil {
				return WalletMove{}, fmt.Errorf("economy: wallet move: %w", err)
			}
		}
		slog.Error("economy: a wallet move is held with its money out: an operator case", "workspace_id", workspaceID, "move_id", id,
			"route", m.Route, "cause", cause)
		m.Status = "held"
		return m, nil
	}
	if screenErr != nil {
		if replay {
			return hold(tx, screenErr)
		}
		return m, screenErr
	}
	if m.ExpiresAt != nil && time.Now().After(*m.ExpiresAt) {
		switch m.stage {
		case "":
			return fail(ErrWalletMoveExpired)
		case walletMoveAwaitingApproval: // the owner approved after the ramp's price ran out; nothing has moved
			if err := s.requoteWalletMove(ctx, &m); errors.Is(err, ErrWalletMove) {
				return fail(err)
			} else if err != nil {
				return WalletMove{}, err
			}
		}
	}
	// The entry is the move as it is now, under the lock: another confirm may have priced it again since it was screened.
	e, err := s.walletMoveEntry(ctx, m)
	if err != nil {
		return WalletMove{}, err
	}
	if e.Counterparty != screened.Counterparty {
		return m, fmt.Errorf("%w: the bank account's holder changed while it was confirmed: confirm again", ErrWalletMove)
	}
	if m.FromAccountID != "" {
		// The wallet it pays from is locked while it moves, so two moves cannot both spend the same money.
		var held int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT balance_minor FROM money_account_balances WHERE account_id = a.id AND funding = $2), 0)
			FROM money_accounts a WHERE a.id = $1 FOR UPDATE`, m.FromAccountID, m.Funding).Scan(&held); err != nil {
			return WalletMove{}, fmt.Errorf("economy: wallet move: %w", err)
		}
		if held < m.SellMinor && m.stage != walletMoveStarted {
			return fail(fmt.Errorf("%w: the wallet holds %s", money.ErrInsufficientFunds, minorString(held, m.SellCurrency)))
		}
	}
	if m.Route == WalletMoveCashOut { // the bank account as it is now, not as it was quoted, and kept so until it is paid
		if _, err := tx.Exec(ctx, `SELECT 1 FROM money_payees WHERE id = $1 AND workspace_id = $2 FOR SHARE`, m.PayeeID, workspaceID); err != nil {
			return WalletMove{}, fmt.Errorf("economy: wallet move: %w", err)
		}
		// Held, the owner's confirming again is their confirming the bank account as it is now.
		confirmedAt := m.CreatedAt
		if m.Status == "held" {
			confirmedAt = time.Now()
		}
		if err := s.checkCashOutPayee(ctx, workspaceID, m.PayeeID, m.Funding == FundingLive, confirmedAt); err != nil {
			if replay {
				return hold(tx, err)
			}
			return fail(err)
		}
	}
	if m.stage != walletMoveStarted {
		if err := stage(walletMoveStarted); err != nil {
			return WalletMove{}, err
		}
	}
	ref, err := s.runWalletMove(ctx, m)
	switch {
	case errors.Is(err, errWalletMoveApproval) && !replay: // the first step was held, so nothing has moved
		if err := stage(walletMoveAwaitingApproval); err != nil {
			return WalletMove{}, err
		}
		m.Detail = err.Error() + ": confirm again to see it posted"
		return m, nil
	case errors.Is(err, errWalletMoveApproval): // asked on a replay: the step stays started, its pay-in as it was
		return hold(tx, err)
	case errors.Is(err, errWalletMovePending):
		m.Detail = err.Error() + ": confirm again to see it posted"
		return m, nil
	case errors.Is(err, errWalletMovePaidIn):
		if err := s.requoteWalletMove(ctx, &m); errors.Is(err, ErrWalletMove) {
			return hold(tx, err)
		} else if err != nil {
			return m, err
		}
		m.Detail = "the ramp's price ran out after your money reached it, so it is priced again: confirm to buy at the new price"
		return m, nil
	case replay && (errors.Is(err, money.ErrRefused) || errors.As(err, new(*CapabilityRefusal))):
		return hold(tx, err)
	case errors.Is(err, money.ErrRefused):
		return fail(err)
	case err != nil: // no answer: a partner may have begun it, and the move is started
		return WalletMove{}, err
	}
	if isTopUp(m.Route) { // keyed to the deposit that delivered it, as its webhook posts it
		e.IdempotencyKey = "payment_in:" + ref
	}
	e.PartnerRef = ref
	// From here the money has moved: a refusal to post it — a frozen workspace, a closed wallet — holds the move.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return WalletMove{}, fmt.Errorf("economy: wallet move: %w", err)
	}
	posted, err := postMoneyTx(ctx, sp, e, screenedBeforeLocks{})
	if err == nil {
		err = sp.Commit(ctx)
	}
	if err != nil {
		_ = sp.Rollback(ctx)
		return hold(tx, err)
	}
	if err := tx.QueryRow(ctx, `UPDATE wallet_moves SET status = 'posted', partner_ref = $2, entry_id = $3, posted_at = now() WHERE id = $1
		RETURNING posted_at`, id, ref, posted.ID).Scan(&m.PostedAt); err != nil {
		return WalletMove{}, fmt.Errorf("economy: wallet move: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" {
			err = fmt.Errorf("%w: %s", ErrMoneyUnbalanced, pgErr.Message)
		}
		return hold(s.pool, err)
	}
	s.monitorMoved(ctx, posted)
	m.Status, m.PartnerRef, m.EntryID = "posted", ref, posted.ID
	return m, nil
}

// deliverWalletMove posts payment in, arrived in the agent's wallet to, as the delivery of the top-up into it the ramp
// executed for exactly that much — once, under the deposit's key — and answers whether it was one's.
//
// shortcut: a delivery is told from another deposit by its wallet and its exact amount, the oldest such top-up first;
// match on the ramp's transaction hash when a live ramp reports one.
func (s *DualTokenStore) deliverWalletMove(ctx context.Context, workspaceID, to string, in InboundPayment) (MoneyEntry, bool, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM wallet_moves WHERE workspace_id = $1 AND to_account_id = $2 AND route IN ('card', 'bank_transfer', 'convert')
		AND status IN ('quoted', 'held') AND stage = 'started' AND buy_minor = $3 AND buy_currency = $4 AND funding = $5 ORDER BY created_at LIMIT 1`,
		workspaceID, to, in.AmountMinor, in.Currency, in.Funding).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return MoneyEntry{}, false, nil
	}
	if err != nil {
		return MoneyEntry{}, false, fmt.Errorf("economy: wallet move delivery: %w", err)
	}
	m, err := scanWalletMove(s.pool.QueryRow(ctx, `SELECT `+walletMoveCols+` FROM wallet_moves WHERE id = $1`, id))
	if err != nil {
		return MoneyEntry{}, false, err
	}
	e, err := s.walletMoveEntry(ctx, m)
	if err != nil {
		return MoneyEntry{}, false, err
	}
	// Screened as any payment in is, against who it is from (B48.7).
	e.IdempotencyKey, e.PartnerRef, e.quarantine = "payment_in:"+in.PartnerRef, in.PartnerRef, true
	if in.Payer != "" {
		e.Counterparty = in.Payer
	}
	if err := s.screenBeforeLocks(ctx, e); err != nil {
		return MoneyEntry{WorkspaceID: workspaceID}, true, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MoneyEntry{}, false, fmt.Errorf("economy: wallet move delivery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// After any confirm of it: one that posted it leaves nothing to deliver, and one still at the ramp has said so.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('wallet_moves:' || $1, 0))`, id); err != nil {
		return MoneyEntry{}, false, fmt.Errorf("economy: wallet move delivery: %w", err)
	}
	var open bool
	if err := tx.QueryRow(ctx, `SELECT status IN ('quoted', 'held') AND executed_at IS NOT NULL AND buy_minor = $2 FROM wallet_moves WHERE id = $1`,
		id, in.AmountMinor).Scan(&open); err != nil || !open {
		return MoneyEntry{}, false, err
	}
	posted, err := postMoneyTx(ctx, tx, e, screenedBeforeLocks{})
	if err != nil {
		return MoneyEntry{}, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE wallet_moves SET status = 'posted', partner_ref = $2, entry_id = $3, posted_at = now() WHERE id = $1`,
		id, in.PartnerRef, posted.ID); err != nil {
		return MoneyEntry{}, false, fmt.Errorf("economy: wallet move delivery: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return MoneyEntry{}, false, fmt.Errorf("economy: wallet move delivery: %w", err)
	}
	s.monitorMoved(ctx, posted)
	return posted, true, nil
}

// requoteWalletMove prices m at the ramp again, under a new quote: a move the owner approved after its quote ran out, or
// one the ramp refused after the wallet paid it in. What leaves the wallet stays as quoted, and so does where a started
// move paid it in; what it buys, the fee and the rate are the ramp's price now.
func (s *DualTokenStore) requoteWalletMove(ctx context.Context, m *WalletMove) error {
	if s.rampPartner == nil {
		return ErrNoRampPartner
	}
	q, err := s.rampPartner.Quote(ctx, partners.RampQuoteRequest{ID: fmt.Sprintf("%s.%d", m.ID, time.Now().UnixNano()),
		Sell: partners.Money{Minor: m.SellMinor, Currency: m.SellCurrency}, Buy: m.BuyCurrency, Method: partners.RampWallet})
	if errors.Is(err, partners.ErrInvalid) {
		return fmt.Errorf("%w: %v", ErrWalletMove, err)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", money.ErrPartnerDown, err)
	}
	m.FeeMinor, m.BuyMinor, m.Rate, m.quoteRef, m.ExpiresAt = q.Fee.Minor, q.Buy.Minor, q.Rate, q.Ref, &q.ExpiresAt
	if m.stage != walletMoveStarted {
		m.payInName, m.payInAddress = q.PayInName, q.PayInAddress
	}
	if _, err := s.pool.Exec(ctx, `UPDATE wallet_moves SET fee_minor = $2, buy_minor = $3, rate = $4, quote_ref = $5, pay_in_name = $6,
		pay_in_address = $7, expires_at = $8 WHERE id = $1`, m.ID, m.FeeMinor, m.BuyMinor, m.Rate, m.quoteRef, m.payInName, m.payInAddress,
		m.ExpiresAt); err != nil {
		return fmt.Errorf("economy: wallet move: %w", err)
	}
	return nil
}

// heldWalletMove is whether op is the money layer making a move between the owner's own wallets quoted under op's key,
// exactly as quoted: from that wallet, that amount, to that wallet or that ramp pay-in. A cash-out's first pay-in is not
// one: money leaving Talyvor is judged by the agent's rules, its approval amount and its freeze, as every payment out
// is; once they allowed it (gated_at), its replay under the same key is the same payment, not judged again.
func (s *DualTokenStore) heldWalletMove(ctx context.Context, op money.Op) (bool, error) {
	if !strings.HasPrefix(op.Key, "wmove_") || op.Capability != money.CapTransfers || (op.Method != "internal" && op.Method != "outbound") {
		return false, nil
	}
	var held bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM wallet_moves WHERE id = $1 AND workspace_id = $2 AND status IN ('quoted', 'held')
		AND from_ref = $3 AND from_ref <> '' AND sell_minor = $4 AND sell_currency = $5
		AND ((route = 'wallet' AND $6 = 'internal' AND to_ref = $7)
		  OR ((route = 'convert' OR (route = 'cash_out' AND gated_at IS NOT NULL)) AND $6 = 'outbound' AND pay_in_name = $8
		      AND pay_in_address = $9)))`,
		op.Key, op.Workspace, op.Ref, op.Amount.Minor, op.Amount.Asset, op.Method, op.Payee, op.To.Name, op.To.Address).Scan(&held)
	return held, err
}

// gateWalletMove notes, on tx, that the agent's rules allowed the payment out made under key, when it is a wallet move's.
func gateWalletMove(ctx context.Context, tx pgx.Tx, workspaceID, key string) error {
	if !strings.HasPrefix(key, "wmove_") {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE wallet_moves SET gated_at = COALESCE(gated_at, now()) WHERE id = $1 AND workspace_id = $2`, key, workspaceID)
	return err
}

// ReturnWalletMove has the ramp give back the money of a held move it has not executed — to the card or bank it was
// charged to, or the wallet that paid it in once the money layer answered that its pay-in arrived (paid_in_at) — once:
// the move ends returned, nothing posted, as nothing stayed moved. by asks it as they would confirm it (mayConfirm).
func (s *DualTokenStore) ReturnWalletMove(ctx context.Context, workspaceID, agentID, id, by string, workspaceOwner bool) (WalletMove, error) {
	if s.moneyLayer == nil {
		return WalletMove{}, ErrAgentWalletsOff
	}
	if s.rampPartner == nil {
		return WalletMove{}, ErrNoRampPartner
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WalletMove{}, fmt.Errorf("economy: wallet move: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('wallet_moves:' || $1, 0))`, id); err != nil {
		return WalletMove{}, fmt.Errorf("economy: wallet move: %w", err)
	}
	var executed, paidIn bool
	m, err := scanWalletMove(tx.QueryRow(ctx, `SELECT `+walletMoveCols+` FROM wallet_moves WHERE id = $1 AND workspace_id = $2 AND agent_id = $3`,
		id, workspaceID, agentID))
	if err == nil {
		err = tx.QueryRow(ctx, `SELECT executed_at IS NOT NULL, paid_in_at IS NOT NULL FROM wallet_moves WHERE id = $1`, id).Scan(&executed, &paidIn)
	}
	if err == nil {
		err = s.mayConfirm(ctx, m, by, workspaceOwner)
	}
	switch {
	case err != nil:
		return WalletMove{}, err
	case m.Status == "returned":
		return m, nil
	case m.Status != "held" || m.Route == WalletMoveWallet:
		return m, fmt.Errorf("%w: only a held move whose money is at the ramp is given back", money.ErrRefused)
	case executed:
		return m, fmt.Errorf("%w: the ramp has bought or sold it, so it is completed, not given back: confirm it again", money.ErrRefused)
	case m.FromAccountID != "" && !paidIn:
		return m, fmt.Errorf("%w: your money has not been seen arriving at the ramp, so there is nothing to give back yet", money.ErrRefused)
	}
	ctx = partners.WithWorkspace(ctx, workspaceID)
	r, err := s.rampPartner.Return(ctx, partners.RampReturn{ID: m.ID + ".return", QuoteRef: m.quoteRef,
		Amount: partners.Money{Minor: m.SellMinor, Currency: m.SellCurrency}})
	switch {
	case errors.Is(err, partners.ErrInvalid) || errors.Is(err, partners.ErrNotFound) || errors.Is(err, partners.ErrIDReused):
		return m, fmt.Errorf("%w: %v", money.ErrRefused, err)
	case err != nil:
		return m, fmt.Errorf("%w: %v", money.ErrPartnerDown, err)
	case r.Status == partners.StatusPending:
		m.Detail = "the ramp is giving it back: ask again to see it returned"
		return m, nil
	case r.Status == partners.StatusFailed:
		return m, fmt.Errorf("%w: %s", money.ErrRefused, r.Detail)
	}
	if m.FromAccountID != "" { // it arrives back in the wallet that paid it in
		rt, err := s.walletMoveFromRoute(ctx, m)
		if err != nil {
			return m, err
		}
		t, err := s.moneyLayer.Transfers(ctx, rt)
		if err != nil {
			return m, err
		}
		res, err := t.Credit(ctx, money.Credit{Key: m.ID + ".return", To: m.fromRef, Amount: money.Amount{Minor: m.SellMinor, Asset: m.SellCurrency},
			PartnerRef: r.Ref})
		if err != nil {
			return m, err
		}
		if res.Status == money.StatusPending {
			m.Detail = "the ramp is giving it back: ask again to see it returned"
			return m, nil
		}
	}
	m.Status, m.Detail = "returned", "given back to where it came from"
	if _, err := tx.Exec(ctx, `UPDATE wallet_moves SET status = 'returned', detail = $2, partner_ref = $3 WHERE id = $1`, id, m.Detail, r.Ref); err != nil {
		return WalletMove{}, fmt.Errorf("economy: wallet move: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return WalletMove{}, fmt.Errorf("economy: wallet move: %w", err)
	}
	m.PartnerRef = r.Ref
	return m, nil
}

// HeldWalletMoves is every workspace's held moves, oldest held first: the operators' cases.
func (s *DualTokenStore) HeldWalletMoves(ctx context.Context) ([]WalletMove, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+walletMoveCols+` FROM wallet_moves WHERE status = 'held' ORDER BY held_at LIMIT 500`)
	if err != nil {
		return nil, fmt.Errorf("economy: held wallet moves: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (WalletMove, error) { return scanWalletMove(r) })
}

// WalletMoves is agentID's top-ups and cash-outs, and the moves its wallets paid to the owner's other agents — or, with
// agentID "", what left the owner's own wallets — newest first; quotes nothing of which has started are not listed.
func (s *DualTokenStore) WalletMoves(ctx context.Context, workspaceID, agentID string) ([]WalletMove, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+walletMoveCols+` FROM wallet_moves WHERE workspace_id = $1 AND (status <> 'quoted' OR stage = 'started') AND (
		($2 <> '' AND (agent_id = $2 OR from_account_id IN (SELECT id FROM money_accounts WHERE workspace_id = $1 AND agent_id = $2)))
		OR ($2 = '' AND from_account_id IN (SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'company')))
		ORDER BY created_at DESC LIMIT 200`, workspaceID, agentID)
	if err != nil {
		return nil, fmt.Errorf("economy: wallet moves: %w", err)
	}
	defer rows.Close()
	out := []WalletMove{}
	for rows.Next() {
		m, err := scanWalletMove(rows)
		if err != nil {
			return nil, fmt.Errorf("economy: wallet moves: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
