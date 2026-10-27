package economy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// money_earnings_payouts.go — B46.11: EARNINGS PAID OUT ON THE MONEY LAYER.
//
// A marketplace sale pays its seller, a Room's earnings are split between its members as the Room's terms agree, and a
// royalty pays its creator: each is a payout of earnings, paid through the money layer's Payouts (B46.3) — the adapter
// switched on for payments_out once it is cleared, the sandbox until then — from the paying workspace's account (an
// agent's, such as a Room's wallet, or the company's) to the recipient's payout details.
//
// A workspace is paid only to payout details it saved — an IBAN, or a USDC address on Base — whose holder the money
// layer's partner verified (KYC). Each payout is made once: one row per source and recipient, its money held in the
// ledger from when it is made, and the adapter asked under the row's own id, so asking again pays nothing twice. A
// payout to a recipient without verified details is held, not paid; it is paid the moment their details are verified.
// The partner's answer — or its signed webhook later — completes it (the held money leaves to the partner), fails it
// (the hold goes back to the account it came from) or returns it, once. Every row keeps its kind, its source and the
// agreement it was paid under, and its money_entries carry the row's id, so a split is traced to its ledger lines
// and to the Room's terms.

// The kinds of earnings paid out.
const (
	EarningsSale      = "sale"
	EarningsRoomSplit = "room_split"
	EarningsRoyalty   = "royalty"
)

// PayoutHeld is a payout waiting for its recipient's verified payout details; its money is held.
const PayoutHeld = "held"

// Where a workspace's payout details stand with the partner's verification.
const (
	PayoutDetailsPending  = "pending"
	PayoutDetailsVerified = "verified"
	PayoutDetailsFailed   = "failed"
)

var (
	// ErrPayoutDetails: payout details without a holder, or that are neither an IBAN nor a USDC address on Base.
	ErrPayoutDetails = errors.New("economy: invalid payout details")
	// ErrEarningsPayout: earnings to pay out without a kind, a source, a currency or anyone to pay.
	ErrEarningsPayout = errors.New("economy: invalid earnings payout")

	addressPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
)

// PayoutDetails is where a workspace is paid in one asset, and whether the partner verified its holder.
type PayoutDetails struct {
	WorkspaceID string    `json:"workspace_id"`
	Asset       string    `json:"asset"`
	Holder      string    `json:"holder"`
	IBAN        string    `json:"iban,omitempty"`
	BIC         string    `json:"bic,omitempty"`
	Network     string    `json:"network,omitempty"`
	Address     string    `json:"address,omitempty"`
	Country     string    `json:"country,omitempty"` // the holder's, asked for by the verification; not kept
	Partner     string    `json:"partner"`
	Status      string    `json:"status"`
	UpdatedAt   time.Time `json:"updated_at"`

	kycRef string
}

// EarningsShare is one recipient's part of earnings paid out: an amount, and for a split the share it is of the whole.
type EarningsShare struct {
	WorkspaceID string `json:"workspace_id"`
	ShareBPS    int    `json:"share_bps,omitempty"`
	AmountMinor int64  `json:"amount_minor"`
}

// Earnings is what to pay out: why (Kind and Source — the sale, the Room's payout, the royalty), under which
// Agreement, from AgentID's account in Currency (the company's when it is ""), and to whom.
type Earnings struct {
	Kind      string          `json:"kind"`
	Source    string          `json:"source"`
	Agreement string          `json:"agreement,omitempty"`
	AgentID   string          `json:"agent_id,omitempty"`
	Currency  string          `json:"currency"`
	Funding   string          `json:"funding,omitempty"`
	Shares    []EarningsShare `json:"shares"`
}

// EarningsPayout is one payout of earnings to one recipient, and where it stands.
type EarningsPayout struct {
	ID                   string    `json:"id"`
	WorkspaceID          string    `json:"workspace_id"`
	AgentID              string    `json:"agent_id,omitempty"`
	Kind                 string    `json:"kind"`
	Source               string    `json:"source"`
	Agreement            string    `json:"agreement,omitempty"`
	RecipientWorkspaceID string    `json:"recipient_workspace_id"`
	ShareBPS             int       `json:"share_bps,omitempty"`
	AmountMinor          int64     `json:"amount_minor"`
	Currency             string    `json:"currency"`
	Funding              string    `json:"funding"`
	Status               string    `json:"status"`
	Partner              string    `json:"partner,omitempty"`
	PartnerRef           string    `json:"partner_ref,omitempty"`
	Detail               string    `json:"detail,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`

	accountID, holdAccountID string
}

const earningsPayoutCols = `id, workspace_id, agent_id, kind, source, agreement, recipient_workspace_id, share_bps, amount_minor, currency,
	funding, status, partner, partner_ref, detail, created_at, updated_at, account_id, hold_account_id`

func scanEarningsPayout(row pgx.Row) (EarningsPayout, error) {
	var p EarningsPayout
	err := row.Scan(&p.ID, &p.WorkspaceID, &p.AgentID, &p.Kind, &p.Source, &p.Agreement, &p.RecipientWorkspaceID, &p.ShareBPS, &p.AmountMinor,
		&p.Currency, &p.Funding, &p.Status, &p.Partner, &p.PartnerRef, &p.Detail, &p.CreatedAt, &p.UpdatedAt, &p.accountID, &p.holdAccountID)
	return p, err
}

// SplitEarnings divides totalMinor by weights so the parts sum to it exactly: each part rounded down, then the minor
// units left over given one each to the largest remainders, the earliest first on a tie. A zero weight gets nothing.
func SplitEarnings(totalMinor int64, weights []int64) ([]int64, error) {
	var sum int64
	for _, w := range weights {
		if w < 0 {
			return nil, fmt.Errorf("%w: a share is not negative", ErrEarningsPayout)
		}
		sum += w
	}
	if totalMinor <= 0 || sum <= 0 {
		return nil, fmt.Errorf("%w: a split is of a positive amount among shares that are not all zero", ErrEarningsPayout)
	}
	parts, rems := make([]int64, len(weights)), make([]int64, len(weights))
	left := totalMinor
	for i, w := range weights {
		// shortcut: totalMinor×w overflows past ~9.2e18 combined, far beyond any one payout; use math/big if it ever is not
		parts[i], rems[i] = totalMinor*w/sum, totalMinor*w%sum
		left -= parts[i]
	}
	for ; left > 0; left-- {
		best := -1
		for i := range rems {
			if weights[i] > 0 && (best < 0 || rems[i] > rems[best]) {
				best = i
			}
		}
		parts[best]++
		rems[best] = -1
	}
	return parts, nil
}

// earningsRoute is the route the money layer verifies payout details and pays earnings out on.
func earningsRoute(workspaceID, agentID string) money.Route {
	return money.Route{Workspace: workspaceID, Agent: agentID, Capability: CapabilityPaymentsOut}
}

// SavePayoutDetails saves where workspaceID is paid in d.Asset — an IBAN for pounds, euros or dollars, a USDC address on
// Base for USDC — and hands its holder to the money layer's partner to verify. Payouts held for want of verified
// details are paid once they are.
func (s *DualTokenStore) SavePayoutDetails(ctx context.Context, workspaceID string, d PayoutDetails) (PayoutDetails, error) {
	if s.moneyLayer == nil {
		return PayoutDetails{}, ErrAgentWalletsOff
	}
	d.WorkspaceID, d.Asset, d.Holder = workspaceID, strings.ToUpper(strings.TrimSpace(d.Asset)), strings.Join(strings.Fields(d.Holder), " ")
	d.IBAN, d.BIC = strings.ToUpper(strings.ReplaceAll(d.IBAN, " ", "")), strings.ToUpper(strings.TrimSpace(d.BIC))
	d.Network, d.Address = strings.ToLower(strings.TrimSpace(d.Network)), strings.TrimSpace(d.Address)
	switch {
	case d.Holder == "" || len(d.Holder) > 140:
		return PayoutDetails{}, fmt.Errorf("%w: payout details name their holder, up to 140 characters", ErrPayoutDetails)
	case d.Asset == money.USDC && (d.Network != "base" || !addressPattern.MatchString(d.Address) || d.IBAN != ""):
		return PayoutDetails{}, fmt.Errorf("%w: USDC is paid to an address on Base: network \"base\" and a 0x address of 40 hex digits", ErrPayoutDetails)
	case d.Asset == CurrencyGBP || d.Asset == CurrencyEUR || d.Asset == CurrencyUSD:
		if !ibanPattern.MatchString(d.IBAN) || !ibanChecks(d.IBAN) || d.Address != "" {
			return PayoutDetails{}, fmt.Errorf("%w: %s is paid to an IBAN", ErrPayoutDetails, d.Asset)
		}
		d.Network = ""
	case d.Asset != money.USDC:
		return PayoutDetails{}, fmt.Errorf("%w: an asset is GBP, EUR, USD or USDC, not %q", ErrPayoutDetails, d.Asset)
	}
	rt := earningsRoute(workspaceID, "")
	partner, err := s.moneyLayer.Partner(ctx, money.CapKYC, rt)
	if err != nil {
		return PayoutDetails{}, err
	}
	kyc, err := s.moneyLayer.KYC(ctx, rt)
	if err != nil {
		return PayoutDetails{}, err
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{workspaceID, d.Asset, d.Holder, d.IBAN, d.BIC, d.Network, d.Address}, "\x00")))
	v, err := kyc.Start(ctx, money.StartKYC{Key: "payout_details:" + hex.EncodeToString(sum[:16]), Subject: money.KYCPerson, Name: d.Holder,
		Country: d.Country})
	if err != nil {
		return PayoutDetails{}, err
	}
	d.Partner, d.kycRef, d.Status = partner, v.Ref, detailsStatus(v.Status)
	if err := s.pool.QueryRow(ctx, `INSERT INTO money_payout_details (workspace_id, asset, holder, iban, bic, network, address, partner, kyc_ref, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (workspace_id, asset) DO UPDATE SET holder = $3, iban = $4, bic = $5, network = $6, address = $7, partner = $8,
			kyc_ref = $9, status = $10, updated_at = now() RETURNING updated_at`,
		workspaceID, d.Asset, d.Holder, d.IBAN, d.BIC, d.Network, d.Address, d.Partner, d.kycRef, d.Status).Scan(&d.UpdatedAt); err != nil {
		return PayoutDetails{}, fmt.Errorf("economy: payout details: %w", err)
	}
	d.Country = ""
	if d.Status == PayoutDetailsVerified {
		s.payHeldEarnings(ctx, workspaceID, d.Asset)
	}
	return d, nil
}

func detailsStatus(st money.Status) string {
	switch st {
	case money.StatusCompleted:
		return PayoutDetailsVerified
	case money.StatusFailed, money.StatusReturned:
		return PayoutDetailsFailed
	}
	return PayoutDetailsPending
}

// PayoutDetailsOf is workspaceID's payout details, each asset's; one whose verification was pending is asked again,
// and payouts held for it are paid once it passes.
func (s *DualTokenStore) PayoutDetailsOf(ctx context.Context, workspaceID string) ([]PayoutDetails, error) {
	rows, err := s.pool.Query(ctx, `SELECT workspace_id, asset, holder, iban, bic, network, address, partner, kyc_ref, status, updated_at
		FROM money_payout_details WHERE workspace_id = $1 ORDER BY asset`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: payout details: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PayoutDetails, error) {
		var d PayoutDetails
		err := r.Scan(&d.WorkspaceID, &d.Asset, &d.Holder, &d.IBAN, &d.BIC, &d.Network, &d.Address, &d.Partner, &d.kycRef, &d.Status, &d.UpdatedAt)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("economy: payout details: %w", err)
	}
	for i := range out {
		d := &out[i]
		if d.Status != PayoutDetailsPending || s.moneyLayer == nil {
			continue
		}
		kyc, err := s.moneyLayer.KYC(ctx, earningsRoute(workspaceID, ""))
		if err != nil {
			return nil, err
		}
		v, err := kyc.Status(ctx, d.kycRef)
		if err != nil {
			return nil, err
		}
		if st := detailsStatus(v.Status); st != d.Status {
			if _, err := s.pool.Exec(ctx, `UPDATE money_payout_details SET status = $3, updated_at = now() WHERE workspace_id = $1 AND asset = $2`,
				workspaceID, d.Asset, st); err != nil {
				return nil, fmt.Errorf("economy: payout details: %w", err)
			}
			d.Status = st
			if st == PayoutDetailsVerified {
				s.payHeldEarnings(ctx, workspaceID, d.Asset)
			}
		}
	}
	return out, nil
}

// PayEarnings pays out e from workspaceID: one payout per share, each made once — the same source and recipient again
// answers the payout first made, and pays a held or unanswered one now if it can be. Its money is held from the paying
// account when it is made; a recipient without verified payout details is held, not paid.
func (s *DualTokenStore) PayEarnings(ctx context.Context, workspaceID string, e Earnings) ([]EarningsPayout, error) {
	e.Kind, e.Source, e.Currency = strings.TrimSpace(e.Kind), strings.TrimSpace(e.Source), strings.ToUpper(strings.TrimSpace(e.Currency))
	if e.Funding == "" {
		e.Funding = FundingTest
	}
	switch {
	case s.moneyLayer == nil:
		return nil, ErrAgentWalletsOff
	case e.Kind != EarningsSale && e.Kind != EarningsRoomSplit && e.Kind != EarningsRoyalty:
		return nil, fmt.Errorf("%w: earnings are a sale, a room_split or a royalty, not %q", ErrEarningsPayout, e.Kind)
	case e.Source == "" || len(e.Source) > 200:
		return nil, fmt.Errorf("%w: earnings name their source, up to 200 characters", ErrEarningsPayout)
	case MoneyCurrencies[e.Currency] == 0:
		return nil, fmt.Errorf("%w: a payout is in GBP, EUR, USD or USDC, not %q", ErrEarningsPayout, e.Currency)
	case e.Funding != FundingTest && e.Funding != FundingLive:
		return nil, fmt.Errorf("%w: funding is test or live, not %q", ErrEarningsPayout, e.Funding)
	case len(e.Shares) == 0:
		return nil, fmt.Errorf("%w: earnings are paid to someone", ErrEarningsPayout)
	}
	seen := map[string]bool{}
	for _, sh := range e.Shares {
		if strings.TrimSpace(sh.WorkspaceID) == "" || sh.AmountMinor < 0 || seen[sh.WorkspaceID] {
			return nil, fmt.Errorf("%w: each share names one recipient once and an amount that is not negative", ErrEarningsPayout)
		}
		seen[sh.WorkspaceID] = true
	}
	from, err := s.payingAccount(ctx, workspaceID, OutsidePayment{AgentID: e.AgentID, Currency: e.Currency})
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("economy: earnings payout: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM money_accounts WHERE id = ANY($1) ORDER BY id FOR UPDATE`, []string{from.companyID, from.id}); err != nil {
		return nil, fmt.Errorf("economy: earnings payout: %w", err)
	}
	var balance int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1 AND funding = $2`,
		from.id, e.Funding).Scan(&balance); err != nil {
		return nil, fmt.Errorf("economy: earnings payout: %w", err)
	}
	hold, err := holdAccount(ctx, tx, workspaceID, e.Currency)
	if err != nil {
		return nil, err
	}
	var out []EarningsPayout
	for _, sh := range e.Shares {
		if sh.AmountMinor == 0 {
			continue
		}
		p, err := scanEarningsPayout(tx.QueryRow(ctx, `SELECT `+earningsPayoutCols+` FROM money_earnings_payouts
			WHERE workspace_id = $1 AND kind = $2 AND source = $3 AND recipient_workspace_id = $4`, workspaceID, e.Kind, e.Source, sh.WorkspaceID))
		if err == nil {
			if p.AmountMinor != sh.AmountMinor || p.Currency != e.Currency {
				return nil, fmt.Errorf("%w: %s's %s %s was already paid out as %s", ErrIdempotencyKeyReused, e.Kind, e.Source, sh.WorkspaceID,
					minorString(p.AmountMinor, p.Currency))
			}
			out = append(out, p)
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("economy: earnings payout: %w", err)
		}
		if balance < sh.AmountMinor {
			return nil, fmt.Errorf("%w: the account holds %s of %s money, and these earnings are more", ErrAgentFunds,
				minorString(balance, e.Currency), e.Funding)
		}
		balance -= sh.AmountMinor
		if p, err = scanEarningsPayout(tx.QueryRow(ctx, `INSERT INTO money_earnings_payouts (id, workspace_id, agent_id, kind, source, agreement,
			recipient_workspace_id, share_bps, amount_minor, currency, funding, account_id, hold_account_id, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING `+earningsPayoutCols,
			"mpo_"+uuid.NewString(), workspaceID, e.AgentID, e.Kind, e.Source, e.Agreement, sh.WorkspaceID, sh.ShareBPS, sh.AmountMinor, e.Currency,
			e.Funding, from.id, hold, PayoutHeld)); err != nil {
			return nil, fmt.Errorf("economy: earnings payout: %w", err)
		}
		if _, err := postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: workspaceID, Capability: CapabilityPaymentsOut, Kind: "earnings_payout_hold",
			IdempotencyKey: "earnings_payout:" + p.ID + ":hold", Funding: p.Funding, Memo: p.memo(), Postings: []MoneyPosting{
				{AccountID: from.id, AmountMinor: -p.AmountMinor}, {AccountID: hold, AmountMinor: p.AmountMinor}}}, s.screener); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("economy: earnings payout: %w", err)
	}
	ctx = context.WithoutCancel(ctx) // a caller that goes away mid-split leaves nothing half-paid: every row is held or sent
	for i := range out {
		if out[i], err = s.sendEarningsPayout(ctx, out[i]); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (p EarningsPayout) memo() string {
	m := p.Kind + " " + p.Source + " to " + p.RecipientWorkspaceID
	if p.Agreement != "" {
		m += " under " + p.Agreement
	}
	return m
}

// payHeldEarnings pays every payout held for recipient in asset, now that its payout details are verified. One that
// cannot be paid stays as it was, and is paid when its earnings are paid out again.
func (s *DualTokenStore) payHeldEarnings(ctx context.Context, recipient, asset string) {
	rows, err := s.pool.Query(ctx, `SELECT `+earningsPayoutCols+` FROM money_earnings_payouts
		WHERE recipient_workspace_id = $1 AND currency = $2 AND status = $3 ORDER BY created_at`, recipient, asset, PayoutHeld)
	if err != nil {
		return
	}
	held, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (EarningsPayout, error) { return scanEarningsPayout(r) })
	if err != nil {
		return
	}
	for _, p := range held {
		_, _ = s.sendEarningsPayout(context.WithoutCancel(ctx), p)
	}
}

// sendEarningsPayout pays p through the money layer if its recipient's payout details are verified, and records the
// partner's answer; a payout without them stays held, and one the partner already answered is left to its webhook.
func (s *DualTokenStore) sendEarningsPayout(ctx context.Context, p EarningsPayout) (EarningsPayout, error) {
	if p.Status != PayoutHeld && (p.Status != PaymentPending || p.PartnerRef != "") {
		return p, nil
	}
	var d PayoutDetails
	err := s.pool.QueryRow(ctx, `SELECT holder, iban, bic, network, address FROM money_payout_details
		WHERE workspace_id = $1 AND asset = $2 AND status = $3`, p.RecipientWorkspaceID, p.Currency, PayoutDetailsVerified).
		Scan(&d.Holder, &d.IBAN, &d.BIC, &d.Network, &d.Address)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.setEarningsPayout(ctx, p, PayoutHeld, "", "", "held until "+p.RecipientWorkspaceID+" has verified "+p.Currency+" payout details")
	}
	if err != nil {
		return p, fmt.Errorf("economy: earnings payout: %w", err)
	}
	if s.screener == nil {
		return p, fmt.Errorf("%w: no screener is set, so no money moves out", partners.ErrScreeningUnavailable)
	}
	var refusal *screening.Refusal
	err = s.screener.ScreenPayment(ctx, screening.Payment{WorkspaceID: p.WorkspaceID, ID: "earnings_payout:" + p.ID, Capability: CapabilityPaymentsOut,
		Direction: "out", Counterparty: d.Holder, AmountMinor: p.AmountMinor, Currency: p.Currency, Funding: p.Funding})
	switch {
	case errors.As(err, &refusal):
		return s.applyEarningsResult(ctx, p, money.Result{Status: money.StatusFailed, Detail: err.Error()})
	case err != nil:
		return p, err
	}
	rt := earningsRoute(p.WorkspaceID, p.AgentID)
	partner, err := s.moneyLayer.Partner(ctx, money.CapPayouts, rt)
	if err != nil {
		return p, err
	}
	if p, err = s.setEarningsPayout(ctx, p, PaymentPending, partner, "", ""); err != nil {
		return p, err
	}
	payouts, err := s.moneyLayer.Payouts(ctx, rt)
	if err != nil {
		return p, err
	}
	var from string
	if err := s.pool.QueryRow(ctx, `SELECT c.partner_account_ref FROM money_accounts a JOIN money_accounts c ON c.id = COALESCE(a.parent_account_id, a.id)
		WHERE a.id = $1`, p.accountID).Scan(&from); err != nil {
		return p, fmt.Errorf("economy: earnings payout: %w", err)
	}
	recipient := money.PayoutSeller
	if p.Kind == EarningsRoomSplit {
		recipient = money.PayoutRoomMember
	}
	res, err := payouts.Pay(partners.WithWorkspace(ctx, p.WorkspaceID), money.Payout{Key: p.ID, From: from, Recipient: recipient,
		Amount: money.Amount{Minor: p.AmountMinor, Asset: p.Currency}, To: money.Payee{Name: d.Holder, IBAN: d.IBAN, BIC: d.BIC,
			Network: d.Network, Address: d.Address}})
	for _, refused := range []error{money.ErrRefused, money.ErrInvalid, money.ErrNotFound, money.ErrKeyReused, money.ErrInsufficientFunds,
		money.ErrLimit, money.ErrApprovalNeeded} {
		if errors.Is(err, refused) {
			return s.applyEarningsResult(ctx, p, money.Result{Status: money.StatusFailed, Detail: err.Error()})
		}
	}
	if err != nil { // the partner did not answer: the money stays held, and it is asked again under the same id
		return p, nil
	}
	return s.applyEarningsResult(ctx, p, res)
}

func (s *DualTokenStore) setEarningsPayout(ctx context.Context, p EarningsPayout, status, partner, ref, detail string) (EarningsPayout, error) {
	q, err := scanEarningsPayout(s.pool.QueryRow(ctx, `UPDATE money_earnings_payouts SET status = $2, partner = COALESCE(NULLIF($3, ''), partner),
		partner_ref = COALESCE(NULLIF($4, ''), partner_ref), detail = $5, updated_at = now() WHERE id = $1 AND status IN ('held', 'pending')
		RETURNING `+earningsPayoutCols, p.ID, status, partner, ref, detail))
	if errors.Is(err, pgx.ErrNoRows) { // answered meanwhile: as it stands
		return scanEarningsPayout(s.pool.QueryRow(ctx, `SELECT `+earningsPayoutCols+` FROM money_earnings_payouts WHERE id = $1`, p.ID))
	}
	if err != nil {
		return p, fmt.Errorf("economy: earnings payout: %w", err)
	}
	return q, nil
}

// applyEarningsResult moves p to where res says it stands, once: completed, its held money leaves to the partner;
// failed, it goes back to the account it came from; returned after completing, it comes back there.
func (s *DualTokenStore) applyEarningsResult(ctx context.Context, p EarningsPayout, res money.Result) (EarningsPayout, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return p, fmt.Errorf("economy: earnings payout: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if p, err = scanEarningsPayout(tx.QueryRow(ctx, `SELECT `+earningsPayoutCols+` FROM money_earnings_payouts WHERE id = $1 FOR UPDATE`, p.ID)); err != nil {
		return p, fmt.Errorf("economy: earnings payout: %w", err)
	}
	if res.Ref != "" {
		p.PartnerRef = res.Ref
	}
	var partnerAccount string
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT p.id FROM money_accounts p WHERE p.workspace_id = c.workspace_id AND p.purpose = 'partner'
		AND p.partner_account_ref = c.partner_account_ref AND c.partner_account_ref <> '' LIMIT 1), '')
		FROM money_accounts a JOIN money_accounts c ON c.id = COALESCE(a.parent_account_id, a.id) WHERE a.id = $1`, p.accountID).Scan(&partnerAccount); err != nil {
		return p, fmt.Errorf("economy: earnings payout: %w", err)
	}
	post := func(kind string, outside bool, postings []MoneyPosting) error {
		e := MoneyEntry{WorkspaceID: p.WorkspaceID, Capability: CapabilityPaymentsOut, Kind: kind, IdempotencyKey: "earnings_payout:" + p.ID + ":" + kind,
			Funding: p.Funding, Memo: p.memo(), Postings: postings}
		if outside {
			e.Counterparty, e.PartnerRef = p.RecipientWorkspaceID, p.PartnerRef
		}
		_, err := postMoneyTx(ctx, tx, e, s.screener)
		return err
	}
	open := p.Status == PayoutHeld || p.Status == PaymentPending
	status := p.Status
	switch res.Status {
	case money.StatusPending:
	case money.StatusFailed:
		if open {
			if err := post("earnings_payout_failed", false, []MoneyPosting{{AccountID: p.holdAccountID, AmountMinor: -p.AmountMinor},
				{AccountID: p.accountID, AmountMinor: p.AmountMinor}}); err != nil {
				return p, err
			}
			status = PaymentFailed
		}
	case money.StatusCompleted, money.StatusReturned:
		if open {
			if err := post("earnings_payout", true, []MoneyPosting{{AccountID: p.holdAccountID, AmountMinor: -p.AmountMinor},
				{AccountID: partnerAccount, AmountMinor: p.AmountMinor}}); err != nil {
				return p, err
			}
			status = PaymentCompleted
		}
		if res.Status == money.StatusReturned && status == PaymentCompleted {
			if err := post("earnings_payout_returned", true, []MoneyPosting{{AccountID: partnerAccount, AmountMinor: -p.AmountMinor},
				{AccountID: p.accountID, AmountMinor: p.AmountMinor}}); err != nil {
				return p, err
			}
			status = PaymentReturned
		}
	default:
		return p, fmt.Errorf("economy: the partner answered %q for payout %s", res.Status, p.ID)
	}
	if res.Detail != "" {
		p.Detail = res.Detail
	} else if status != p.Status {
		p.Detail = ""
	}
	if p, err = scanEarningsPayout(tx.QueryRow(ctx, `UPDATE money_earnings_payouts SET status = $2, partner_ref = $3, detail = $4, updated_at = now()
		WHERE id = $1 RETURNING `+earningsPayoutCols, p.ID, status, p.PartnerRef, p.Detail)); err != nil {
		return p, fmt.Errorf("economy: earnings payout: %w", err)
	}
	return p, tx.Commit(ctx)
}

// heldEarningsPayout is whether op is the money layer paying out earnings PayEarnings holds the money for: the payout
// pending under op's key, of that amount, from that workspace and agent.
func (s *DualTokenStore) heldEarningsPayout(ctx context.Context, op money.Op) (bool, error) {
	if op.Capability != money.CapPayouts || op.Method != "pay" || !strings.HasPrefix(op.Key, "mpo_") {
		return false, nil
	}
	var held bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM money_earnings_payouts WHERE id = $1 AND workspace_id = $2 AND agent_id = $3
		AND status = $4 AND amount_minor = $5 AND currency = $6)`, op.Key, op.Workspace, op.Agent, PaymentPending, op.Amount.Minor, op.Amount.Asset).Scan(&held)
	return held, err
}

// SettleEarningsPayout moves the payout partner knows as e.PartnerRef to where its webhook says it stands, once.
// money.ErrNotFound when no payout is that partner's.
func (s *DualTokenStore) SettleEarningsPayout(ctx context.Context, partner string, e money.Event) (EarningsPayout, error) {
	p, err := scanEarningsPayout(s.pool.QueryRow(ctx, `SELECT `+earningsPayoutCols+` FROM money_earnings_payouts WHERE partner = $1 AND partner_ref = $2`,
		partner, e.PartnerRef))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, fmt.Errorf("%w: no payout is %s's %s", money.ErrNotFound, partner, e.PartnerRef)
	}
	if err != nil {
		return p, fmt.Errorf("economy: settle payout: %w", err)
	}
	return s.applyEarningsResult(ctx, p, money.Result{Ref: e.PartnerRef, Status: e.Status, Detail: e.Detail})
}

// EarningsPayouts is the payouts workspaceID made or was paid, newest first, at most 200.
func (s *DualTokenStore) EarningsPayouts(ctx context.Context, workspaceID string) ([]EarningsPayout, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+earningsPayoutCols+` FROM money_earnings_payouts WHERE workspace_id = $1 OR recipient_workspace_id = $1
		ORDER BY created_at DESC, id LIMIT 200`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: earnings payouts: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (EarningsPayout, error) { return scanEarningsPayout(r) })
	if err != nil {
		return nil, fmt.Errorf("economy: earnings payouts: %w", err)
	}
	return out, nil
}
