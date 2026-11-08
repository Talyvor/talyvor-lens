package economy

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/fees"
)

// merchant_checkouts.go — B30.38: TALYVOR PAY, A BUSINESS ACCEPTS PAYMENTS FROM ANY TALYVOR AGENT.
//
// A company becomes a merchant by opening a checkout (migration 0363): an amount in a currency it has a company account
// in, its own order id, and where the payer returns to. Any Talyvor agent pays it from its own account in that currency:
// its rules judge it as a payment to the merchant, exactly as an invoice's (B30.131), and one money entry moves it from
// the agent's account into the merchant's, less Talyvor's fee — LENS_MERCHANT_A2A_FEE_BPS of it (internal/fees, B32.8) —
// which goes to revenue. The entry is the merchant's, under merchant_acceptance (RED): test money only until the
// merchant is cleared, asked by PostMoney. The merchant may refund a paid checkout, which reverses the entry in full.
//
// shortcut: an agent pays from an account only; paying a checkout by card (LENS_MERCHANT_FEE_BPS plus the processor's
// cost) waits for a card checkout, upgrade when one is built.

var (
	// ErrCheckout: a checkout asked for with an amount, a currency, an order id or a return URL it cannot have.
	ErrCheckout = errors.New("economy: invalid checkout")
	// ErrCheckoutNotFound: no such checkout, or not one this merchant opened.
	ErrCheckoutNotFound = errors.New("economy: no such checkout")
	// ErrCheckoutState: the checkout is not in a state that allows it — paying a paid one, refunding an unpaid one.
	ErrCheckoutState = errors.New("economy: the checkout's state does not allow this")
	// ErrCheckoutShort: the account does not hold what paying or refunding the checkout takes.
	ErrCheckoutShort = errors.New("economy: the account does not hold enough for this checkout")
)

// The states of a checkout.
const (
	CheckoutOpen     = "open"
	CheckoutPaid     = "paid"
	CheckoutRefunded = "refunded"
)

// CheckoutRequest is a checkout a merchant asks for.
type CheckoutRequest struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	OrderID     string `json:"order_id"`
	ReturnURL   string `json:"return_url"`
}

// Checkout is a merchant's checkout, and who paid it.
type Checkout struct {
	ID               string     `json:"id"`
	WorkspaceID      string     `json:"workspace_id,omitempty"`
	Merchant         string     `json:"merchant"`
	AccountID        string     `json:"account_id,omitempty"`
	Currency         string     `json:"currency"`
	AmountMinor      int64      `json:"amount_minor"`
	OrderID          string     `json:"order_id"`
	ReturnURL        string     `json:"return_url,omitempty"`
	Status           string     `json:"status"`
	FeeMinor         int64      `json:"fee_minor"`     // Talyvor's, once paid
	SettledMinor     int64      `json:"settled_minor"` // what reached the merchant's account, once paid
	Funding          string     `json:"funding,omitempty"`
	PayerWorkspaceID string     `json:"payer_workspace_id,omitempty"`
	PayerAgentID     string     `json:"payer_agent_id,omitempty"`
	PaymentEntryID   string     `json:"payment_entry_id,omitempty"`
	RefundEntryID    string     `json:"refund_entry_id,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	PaidAt           *time.Time `json:"paid_at,omitempty"`
	RefundedAt       *time.Time `json:"refunded_at,omitempty"`
}

// PaidCheckout is a checkout and the money entry that paid or refunded it.
type PaidCheckout struct {
	Checkout Checkout   `json:"checkout"`
	Entry    MoneyEntry `json:"entry"`
}

const checkoutColumns = `c.id, c.workspace_id, COALESCE((SELECT w.name FROM workspaces w WHERE w.id = c.workspace_id), ''), c.account_id,
	c.currency, c.amount_minor, c.order_id, c.return_url, c.status, c.fee_minor, c.funding, c.payer_workspace_id, c.payer_agent_id,
	COALESCE(c.payment_entry_id, ''), COALESCE(c.refund_entry_id, ''), c.created_at, c.paid_at, c.refunded_at`

// readCheckout is checkout id — workspaceID's only, unless workspaceID is "".
func readCheckout(ctx context.Context, q pgxDB, id, workspaceID string) (Checkout, error) {
	c, err := scanCheckout(q.QueryRow(ctx, `SELECT `+checkoutColumns+` FROM merchant_checkouts c WHERE c.id = $1 AND ($2 = '' OR c.workspace_id = $2)`,
		id, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Checkout{}, ErrCheckoutNotFound
	}
	return c, err
}

func scanCheckout(row pgx.Row) (Checkout, error) {
	var c Checkout
	if err := row.Scan(&c.ID, &c.WorkspaceID, &c.Merchant, &c.AccountID, &c.Currency, &c.AmountMinor, &c.OrderID, &c.ReturnURL, &c.Status,
		&c.FeeMinor, &c.Funding, &c.PayerWorkspaceID, &c.PayerAgentID, &c.PaymentEntryID, &c.RefundEntryID, &c.CreatedAt, &c.PaidAt,
		&c.RefundedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Checkout{}, err
		}
		return Checkout{}, fmt.Errorf("economy: checkout: %w", err)
	}
	c.Merchant = cmp.Or(c.Merchant, c.WorkspaceID)
	if c.Status != CheckoutOpen {
		c.SettledMinor = c.AmountMinor - c.FeeMinor
	}
	return c, nil
}

// CreateCheckout opens a checkout for workspaceID, settling into its open company account in r.Currency.
func (s *DualTokenStore) CreateCheckout(ctx context.Context, workspaceID string, r CheckoutRequest) (Checkout, error) {
	r.Currency = strings.ToUpper(strings.TrimSpace(r.Currency))
	r.OrderID = strings.TrimSpace(r.OrderID)
	switch {
	case r.AmountMinor <= 0:
		return Checkout{}, fmt.Errorf("%w: amount_minor is more than 0", ErrCheckout)
	case r.OrderID == "" || len(r.OrderID) > 200:
		return Checkout{}, fmt.Errorf("%w: order_id is 1 to 200 characters", ErrCheckout)
	}
	if r.ReturnURL != "" {
		if u, err := url.Parse(r.ReturnURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(r.ReturnURL) > 2000 {
			return Checkout{}, fmt.Errorf("%w: return_url is an http or https URL", ErrCheckout)
		}
	}
	c, _ := CapabilityByKey(CapabilityMerchantAcceptance)
	if err := refuseFrozen(ctx, s.pool, workspaceID, c); err != nil {
		return Checkout{}, err
	}
	if err := refuseUnaccepted(ctx, s.pool, workspaceID, c); err != nil {
		return Checkout{}, err
	}
	id := "chk_" + uuid.NewString()
	_, err := s.pool.Exec(ctx, `INSERT INTO merchant_checkouts (id, workspace_id, account_id, currency, amount_minor, order_id, return_url)
		SELECT $1, $2, a.id, a.currency, $4, $5, $6 FROM money_accounts a
		WHERE a.workspace_id = $2 AND a.currency = $3 AND a.purpose = 'company' AND a.status = 'open'
		ORDER BY a.created_at LIMIT 1`, id, workspaceID, r.Currency, r.AmountMinor, r.OrderID, r.ReturnURL)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Checkout{}, fmt.Errorf("%w: order %s already has a checkout", ErrCheckout, r.OrderID)
	}
	if err != nil {
		return Checkout{}, fmt.Errorf("economy: create checkout: %w", err)
	}
	out, err := readCheckout(ctx, s.pool, id, "")
	if errors.Is(err, ErrCheckoutNotFound) {
		return Checkout{}, fmt.Errorf("%w: the company has no open %s account; open it first", ErrMoneyAccountNotFound, r.Currency)
	}
	return out, err
}

// Checkouts is workspaceID's checkouts, newest first.
func (s *DualTokenStore) Checkouts(ctx context.Context, workspaceID string) ([]Checkout, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+checkoutColumns+` FROM merchant_checkouts c WHERE c.workspace_id = $1
		ORDER BY c.created_at DESC, c.id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: checkouts: %w", err)
	}
	defer rows.Close()
	out := []Checkout{}
	for rows.Next() {
		c, err := scanCheckout(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("economy: checkouts: %w", err)
	}
	return out, nil
}

// PayCheckout pays checkout id from agentID's open account in its currency, in workspaceID, as funding (test, or ""
// for test; or live). A retry by the same agent answers the payment made, and moves no money twice.
func (s *DualTokenStore) PayCheckout(ctx context.Context, id, workspaceID, agentID, funding string) (PaidCheckout, error) {
	if funding == "" {
		funding = FundingTest
	}
	if funding != FundingTest && funding != FundingLive {
		return PaidCheckout{}, fmt.Errorf("%w: funding is test or live, not %q", ErrCheckout, funding)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PaidCheckout{}, fmt.Errorf("economy: pay checkout: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The checkout's row is held until the payment commits, so two payers at once cannot both pay it.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM merchant_checkouts WHERE id = $1 FOR UPDATE`, id); err != nil {
		return PaidCheckout{}, fmt.Errorf("economy: pay checkout: %w", err)
	}
	co, err := readCheckout(ctx, tx, id, "")
	if err != nil {
		return PaidCheckout{}, err
	}
	var from string
	var paused bool
	err = tx.QueryRow(ctx, `SELECT m.id, a.paused_at IS NOT NULL OR a.archived_at IS NOT NULL FROM money_accounts m JOIN agent_accounts a ON a.id = m.agent_id
		WHERE m.workspace_id = $1 AND m.agent_id = $2 AND m.purpose = 'agent' AND m.currency = $3 AND m.status = 'open'`,
		workspaceID, agentID, co.Currency).Scan(&from, &paused)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaidCheckout{}, fmt.Errorf("%w: the agent has no open %s account to pay from", ErrMoneyAccountNotFound, co.Currency)
	}
	if err != nil {
		return PaidCheckout{}, fmt.Errorf("economy: pay checkout: %w", err)
	}
	key := "merchant_checkout:" + co.ID
	if co.Status != CheckoutOpen {
		if co.PayerWorkspaceID == workspaceID && co.PayerAgentID == agentID {
			if paid, err := readMoneyEntry(ctx, tx, `e.id = $1`, co.PaymentEntryID); err == nil {
				return PaidCheckout{Checkout: co, Entry: paid}, nil
			}
		}
		return PaidCheckout{}, fmt.Errorf("%w: it is %s", ErrCheckoutState, co.Status)
	}
	if paused {
		return PaidCheckout{}, ErrInvoicePayerPaused
	}
	if s.accountPartners == nil {
		return PaidCheckout{}, fmt.Errorf("%w: no account partner is set", ErrAccountNotOpened)
	}
	// The payer's rules judge it as a payment to the merchant, at what it is worth in LXC, exactly as an invoice's
	// (B30.131). The agent is locked first, then the account, so no other movement spends the same money or limit.
	value, err := s.paymentValue(ctx, OutsidePayment{WorkspaceID: workspaceID, IdempotencyKey: key, AmountMinor: co.AmountMinor, Currency: co.Currency})
	if err != nil {
		return PaidCheckout{}, err
	}
	fp := sha256.Sum256([]byte(key + "\x00" + strconv.FormatInt(co.AmountMinor, 10) + "\x00" + co.Currency + "\x00" + funding))
	ctx = WithAgentRequest(ctx, AgentRequest{Payment: true, Fingerprint: hex.EncodeToString(fp[:]),
		Payee: Payee{Kind: "company", ID: co.WorkspaceID, Name: co.Merchant},
		Memo:  Money(co.AmountMinor, co.Currency) + " to " + co.Merchant + " for order " + co.OrderID + " (" + funding + " money)"})
	if err := lockAgent(ctx, tx, workspaceID, agentID); err != nil {
		return PaidCheckout{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM money_accounts WHERE id = $1 FOR UPDATE`, from); err != nil {
		return PaidCheckout{}, fmt.Errorf("economy: pay checkout: %w", err)
	}
	if held, err := heldMinor(ctx, tx, from, funding); err != nil {
		return PaidCheckout{}, err
	} else if held < co.AmountMinor {
		return PaidCheckout{}, fmt.Errorf("%w: it holds %s of %s money, and the checkout is %s", ErrCheckoutShort, FormatMinor(held, co.Currency),
			funding, FormatMinor(co.AmountMinor, co.Currency))
	}
	if err := enforceAgentRules(ctx, tx, workspaceID, agentID, value, key); err != nil {
		return PaidCheckout{}, s.refusedMovement(ctx, tx, err)
	}
	fee := PlatformFee(co.AmountMinor, fees.Current().MerchantA2AFeeBPS)
	revenue, err := routeAccount(ctx, tx, co.WorkspaceID, co.Currency, MoneyRevenue, "", "Talyvor fees")
	if err != nil {
		return PaidCheckout{}, err
	}
	e := MoneyEntry{WorkspaceID: co.WorkspaceID, Capability: CapabilityMerchantAcceptance, Kind: "merchant_payment", IdempotencyKey: key,
		Funding: funding, Memo: "Order " + co.OrderID + " at " + co.Merchant, payeeWorkspaceID: workspaceID,
		Postings: []MoneyPosting{{AccountID: from, AmountMinor: -co.AmountMinor}, {AccountID: co.AccountID, AmountMinor: co.AmountMinor - fee},
			{AccountID: revenue, AmountMinor: fee}}}
	out := PaidCheckout{}
	if out.Entry, err = postMoneyTx(ctx, tx, e, s.screener); err != nil {
		return PaidCheckout{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE merchant_checkouts SET status = 'paid', fee_minor = $2, funding = $3, payer_workspace_id = $4,
		payer_agent_id = $5, payment_entry_id = $6, paid_at = now() WHERE id = $1`, co.ID, fee, funding, workspaceID, agentID, out.Entry.ID); err != nil {
		return PaidCheckout{}, fmt.Errorf("economy: pay checkout: %w", err)
	}
	if err := commitMoney(ctx, tx); err != nil {
		return PaidCheckout{}, err
	}
	out.Checkout, err = readCheckout(ctx, s.pool, co.ID, "")
	return out, err
}

// RefundCheckout reverses workspaceID's paid checkout id in full: the agent gets back what it paid, from the merchant's
// account and from Talyvor's fee. It is once: a refunded checkout answers its refund.
func (s *DualTokenStore) RefundCheckout(ctx context.Context, workspaceID, id string) (PaidCheckout, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PaidCheckout{}, fmt.Errorf("economy: refund checkout: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM merchant_checkouts WHERE id = $1 AND workspace_id = $2 FOR UPDATE`, id, workspaceID); err != nil {
		return PaidCheckout{}, fmt.Errorf("economy: refund checkout: %w", err)
	}
	co, err := readCheckout(ctx, tx, id, workspaceID)
	if err != nil {
		return PaidCheckout{}, err
	}
	switch co.Status {
	case CheckoutRefunded:
		refund, err := readMoneyEntry(ctx, tx, `e.id = $1`, co.RefundEntryID)
		return PaidCheckout{Checkout: co, Entry: refund}, err
	case CheckoutOpen:
		return PaidCheckout{}, fmt.Errorf("%w: it is not paid", ErrCheckoutState)
	}
	paid, err := readMoneyEntry(ctx, tx, `e.id = $1`, co.PaymentEntryID)
	if err != nil {
		return PaidCheckout{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM money_accounts WHERE id = $1 FOR UPDATE`, co.AccountID); err != nil {
		return PaidCheckout{}, fmt.Errorf("economy: refund checkout: %w", err)
	}
	if held, err := heldMinor(ctx, tx, co.AccountID, co.Funding); err != nil {
		return PaidCheckout{}, err
	} else if held < co.SettledMinor {
		return PaidCheckout{}, fmt.Errorf("%w: it holds %s of %s money, and the refund takes %s back", ErrCheckoutShort, FormatMinor(held, co.Currency),
			co.Funding, FormatMinor(co.SettledMinor, co.Currency))
	}
	back := make([]MoneyPosting, 0, len(paid.Postings))
	for _, p := range paid.Postings {
		if p.AmountMinor != 0 {
			back = append(back, MoneyPosting{AccountID: p.AccountID, AmountMinor: -p.AmountMinor})
		}
	}
	e := MoneyEntry{WorkspaceID: co.WorkspaceID, Capability: CapabilityMerchantAcceptance, Kind: "merchant_refund",
		IdempotencyKey: "merchant_refund:" + co.ID, Funding: co.Funding, Memo: "Refund of order " + co.OrderID + " at " + co.Merchant,
		payeeWorkspaceID: co.PayerWorkspaceID, Postings: back}
	out := PaidCheckout{}
	if out.Entry, err = postMoneyTx(ctx, tx, e, s.screener); err != nil {
		return PaidCheckout{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE merchant_checkouts SET status = 'refunded', refund_entry_id = $2, refunded_at = now() WHERE id = $1`,
		co.ID, out.Entry.ID); err != nil {
		return PaidCheckout{}, fmt.Errorf("economy: refund checkout: %w", err)
	}
	if err := commitMoney(ctx, tx); err != nil {
		return PaidCheckout{}, err
	}
	out.Checkout, err = readCheckout(ctx, s.pool, co.ID, "")
	return out, err
}

// heldMinor is what account holds of funding's money.
func heldMinor(ctx context.Context, tx pgx.Tx, account, funding string) (int64, error) {
	var held int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT balance_minor FROM money_account_balances WHERE account_id = $1 AND funding = $2), 0)`,
		account, funding).Scan(&held); err != nil {
		return 0, fmt.Errorf("economy: checkout: %w", err)
	}
	return held, nil
}

// commitMoney commits a money movement: an entry that does not sum to zero is ErrMoneyUnbalanced.
func commitMoney(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" {
			return fmt.Errorf("%w: %s", ErrMoneyUnbalanced, pgErr.Message)
		}
		return fmt.Errorf("economy: checkout: %w", err)
	}
	return nil
}
