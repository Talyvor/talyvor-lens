package economy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/workspace"
)

// agent_schedules.go — B19.8: SCHEDULED PAYMENTS AND AUTOMATIC TOP-UPS.
//
// RunAgentSchedules is called on a tick (cmd/lens, every minute). It runs every schedule tick that is
// due, each exactly once (migration 0144) — ticks missed while Lens was down are each run once when it
// is back — and then tops up every agent whose balance is below its threshold. A scheduled payment is an
// ordinary agent payment (B19.3), judged by the payer's rules; a tick they refuse is recorded as refused
// and not retried. A top-up fires once per crossing: under the agent's row lock it brings the balance
// back up to its target, so the next run finds it above the threshold.
//
// B30.18: a STANDING ORDER is a schedule that pays a saved outside payee (B30.16): each tick is one payment out
// (B30.17) of amount_minor in its currency, judged by the agent's rules when it runs, under the tick's own idempotency
// key so a tick run twice pays once. It may end at end_at, and the owner may skip its next tick.

// ErrScheduleNotFound: no such payment schedule in this workspace.
var ErrScheduleNotFound = errors.New("economy: no such payment schedule in this workspace")

// maxTicksPerRun bounds one run's catching up; the rest waits for the next run.
const maxTicksPerRun = 1000

// AgentSchedule is a recurring payment from one of a workspace's agents to another, or to a marketplace
// listing (B19.17) — exactly one of ToAgentID and ToListingID is set.
type AgentSchedule struct {
	ID          string     `json:"id"`
	FromAgentID string     `json:"from_agent_id"`
	ToAgentID   string     `json:"to_agent_id"`
	ToListingID string     `json:"to_listing_id,omitempty"`
	ToPayeeID   string     `json:"to_payee_id,omitempty"` // B30.18: an outside payee, paid AmountMinor of Currency a tick
	AmountULXC  int64      `json:"amount_ulxc"`
	AmountMinor int64      `json:"amount_minor,omitempty"`
	Currency    string     `json:"currency,omitempty"`
	Memo        string     `json:"memo,omitempty"`
	Every       string     `json:"every"` // hour | day | week | month
	NextRunAt   time.Time  `json:"next_run_at"`
	EndAt       *time.Time `json:"end_at,omitempty"` // no tick after it
	Active      bool       `json:"active"`
	CreatedAt   time.Time  `json:"created_at"`
}

// AgentScheduleRun is one tick of a schedule: paid (EntryID is the payment's entry, UseID the listing's billed use,
// or PaymentID the payment out, whose status Detail then is), refused (Detail says why) or skipped by the owner.
type AgentScheduleRun struct {
	TickAt    time.Time `json:"tick_at"`
	Outcome   string    `json:"outcome"` // paid | refused | skipped
	EntryID   string    `json:"entry_id,omitempty"`
	UseID     string    `json:"use_id,omitempty"`
	PaymentID string    `json:"payment_id,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// AgentTopUp is an agent's automatic top-up: below BelowULXC, back up to ToULXC.
type AgentTopUp struct {
	AgentID   string `json:"agent_id"`
	BelowULXC int64  `json:"below_ulxc"`
	ToULXC    int64  `json:"to_ulxc"`
}

// ScheduleRunResult counts what one RunAgentSchedules did.
type ScheduleRunResult struct {
	Paid, Refused, ToppedUp int
	Renewed, Unpaid         int // marketplace licences renewed, and ended unpaid (B32.20)
	IdleAlerted             int // renewing marketplace licences whose buyers were told they had gone unused (B41.176)
}

func nextTick(t time.Time, every string) time.Time {
	switch every {
	case "hour":
		return t.Add(time.Hour)
	case "day":
		return t.AddDate(0, 0, 1)
	case "week":
		return t.AddDate(0, 0, 7)
	default: // month
		return t.AddDate(0, 1, 0)
	}
}

const agentScheduleColumns = `id, from_agent_id, COALESCE(to_agent_id, ''), COALESCE(to_listing_id, ''), COALESCE(to_payee_id, ''), amount_ulxc,
	amount_minor, currency, memo, every, next_run_at, end_at, active, created_at`

func scanAgentSchedule(row pgx.Row) (AgentSchedule, error) {
	var sc AgentSchedule
	err := row.Scan(&sc.ID, &sc.FromAgentID, &sc.ToAgentID, &sc.ToListingID, &sc.ToPayeeID, &sc.AmountULXC, &sc.AmountMinor, &sc.Currency, &sc.Memo,
		&sc.Every, &sc.NextRunAt, &sc.EndAt, &sc.Active, &sc.CreatedAt)
	return sc, err
}

// CreateStandingOrder schedules fromAgentID to pay workspaceID's saved outside payee payeeID amountMinor of currency
// every hour, day, week or month, the first time at firstRunAt and the last on or before endAt (nil: until cancelled).
// Each tick is a payment out (B30.17), judged by the agent's rules when it runs.
func (s *DualTokenStore) CreateStandingOrder(ctx context.Context, workspaceID, fromAgentID, payeeID string, amountMinor int64, currency,
	memo, every string, firstRunAt time.Time, endAt *time.Time) (AgentSchedule, error) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	switch {
	case amountMinor <= 0:
		return AgentSchedule{}, fmt.Errorf("%w: amount_minor is a positive number of pence or cents", ErrAgentRule)
	case currency != CurrencyGBP && currency != CurrencyEUR && currency != CurrencyUSD:
		return AgentSchedule{}, fmt.Errorf("%w: a standing order is in GBP, EUR or USD, not %q", ErrAgentRule, currency)
	case every != "hour" && every != "day" && every != "week" && every != "month":
		return AgentSchedule{}, fmt.Errorf("%w: every must be hour, day, week or month", ErrAgentRule)
	case endAt != nil && endAt.Before(firstRunAt):
		return AgentSchedule{}, fmt.Errorf("%w: end_at is before the first run", ErrAgentRule)
	}
	if _, err := s.PayableOutsidePayee(ctx, workspaceID, payeeID); err != nil {
		return AgentSchedule{}, err
	}
	sc, err := scanAgentSchedule(s.pool.QueryRow(ctx, `
		INSERT INTO agent_payment_schedules (id, workspace_id, from_agent_id, to_payee_id, amount_ulxc, amount_minor, currency, memo, every, next_run_at, end_at)
		SELECT $1, $2, f.id, $4, 0, $5, $6, $7, $8, $9, $10 FROM agent_accounts f WHERE f.id = $3 AND f.workspace_id = $2
		RETURNING `+agentScheduleColumns,
		"sch_"+uuid.NewString(), workspaceID, fromAgentID, payeeID, amountMinor, currency, memo, every, firstRunAt.UTC(), endAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return sc, ErrAgentNotFound
	}
	if err != nil {
		return sc, fmt.Errorf("economy: create standing order: %w", err)
	}
	return sc, nil
}

// SkipAgentScheduleTick skips a schedule's next tick: it is recorded as a skipped run and the schedule moves on to
// the tick after it.
func (s *DualTokenStore) SkipAgentScheduleTick(ctx context.Context, workspaceID, scheduleID string) (AgentSchedule, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentSchedule{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	sc, err := scanAgentSchedule(tx.QueryRow(ctx, `SELECT `+agentScheduleColumns+` FROM agent_payment_schedules
		WHERE id = $1 AND workspace_id = $2 AND active FOR UPDATE`, scheduleID, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return sc, ErrScheduleNotFound
	}
	if err != nil {
		return sc, fmt.Errorf("economy: skip schedule tick: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_schedule_runs (schedule_id, tick_at, outcome, detail) VALUES ($1, $2, 'skipped', 'skipped by the owner')`,
		sc.ID, sc.NextRunAt); err != nil {
		return sc, fmt.Errorf("economy: skip schedule tick: %w", err)
	}
	if sc, err = advanceSchedule(ctx, tx, sc); err != nil {
		return sc, err
	}
	return sc, tx.Commit(ctx)
}

// advanceSchedule moves sc on to its next tick; one past end_at ends it.
func advanceSchedule(ctx context.Context, tx pgx.Tx, sc AgentSchedule) (AgentSchedule, error) {
	sc, err := scanAgentSchedule(tx.QueryRow(ctx, `UPDATE agent_payment_schedules SET next_run_at = $2, active = active AND (end_at IS NULL OR $2 <= end_at)
		WHERE id = $1 RETURNING `+agentScheduleColumns, sc.ID, nextTick(sc.NextRunAt.UTC(), sc.Every)))
	if err != nil {
		return sc, fmt.Errorf("economy: advance schedule: %w", err)
	}
	return sc, nil
}

// ListingCharger is what a schedule paying a marketplace listing needs of the marketplace (B19.17).
// *market.Store satisfies it.
type ListingCharger interface {
	// ListingPrice is what one use of listingID costs buyerWorkspaceID, in µLXC, or a refusal saying why
	// the workspace cannot pay it (its own listing, a free one, one it cannot see).
	ListingPrice(ctx context.Context, buyerWorkspaceID, listingID string) (price int64, refusal string, err error)
	// ChargeScheduledListing records, in tx, one billed use of listingID by agentID at the tick at, once
	// judge lets its price through: never more than maxULXC. refusal is non-empty (and nothing recorded)
	// when the listing cannot be paid; judge's error is returned as it is.
	ChargeScheduledListing(ctx context.Context, tx pgx.Tx, buyerWorkspaceID, agentID, listingID string, maxULXC int64, at time.Time,
		judge func(price int64) error) (useID, refusal string, err error)
}

// LicenceJudge is what renewing a marketplace licence asks of the economy (B32.20), as its first charge did: its
// agent's rules, and the capability its charge is recorded under. *DualTokenStore satisfies it.
type LicenceJudge interface {
	JudgeAgentPurchase(ctx context.Context, workspaceID, agentID, listingID string, amount int64, c Commitment, what string,
		record func(pgx.Tx) error) error
	RequireBilledCapability(ctx context.Context, workspaceID, key string) error
}

// LicenceRenewer renews the marketplace's licences whose period has ended (B32.20). *market.Store satisfies it.
type LicenceRenewer interface {
	RenewLicences(ctx context.Context, now time.Time, judge LicenceJudge) (renewed, unpaid int, err error)
}

// IdleLicence is a renewing marketplace licence its buyer has not used for 30 days (B41.176).
type IdleLicence struct{ WorkspaceID, LicenceID, Title string }

// IdleLicenceAlerter records each licence newly idle at now, once per idle stretch. *market.Store satisfies it.
type IdleLicenceAlerter interface {
	AlertIdleLicences(ctx context.Context, now time.Time) ([]IdleLicence, error)
}

// SetListingCharger lets schedules pay marketplace listings.
func (s *DualTokenStore) SetListingCharger(c ListingCharger) { s.listings = c }

// ErrListingPayee: a schedule to a listing that the workspace cannot pay.
var ErrListingPayee = errors.New("economy: this schedule cannot pay that listing")

// CreateAgentListingSchedule schedules fromAgentID to pay listingID every hour, day, week or month, the
// first time at firstRunAt. Each tick is one use of the listing at its price then, on the company's monthly
// marketplace bill; amount is the most a tick pays (0: the listing's price now), so a seller raising the
// price is refused rather than paid.
func (s *DualTokenStore) CreateAgentListingSchedule(ctx context.Context, workspaceID, fromAgentID, listingID string, amount int64,
	memo, every string, firstRunAt time.Time) (AgentSchedule, error) {
	if every != "hour" && every != "day" && every != "week" && every != "month" {
		return AgentSchedule{}, fmt.Errorf("%w: every must be hour, day, week or month", ErrAgentRule)
	}
	if s.listings == nil {
		return AgentSchedule{}, fmt.Errorf("%w: the marketplace is not configured", ErrListingPayee)
	}
	price, refusal, err := s.listings.ListingPrice(ctx, workspaceID, listingID)
	if err != nil {
		return AgentSchedule{}, err
	}
	if refusal != "" {
		return AgentSchedule{}, fmt.Errorf("%w: %s", ErrListingPayee, refusal)
	}
	if amount == 0 {
		amount = price
	}
	if amount < price {
		return AgentSchedule{}, fmt.Errorf("%w: the listing costs %s LXC a use, more than %s LXC", ErrListingPayee, lxcString(price), lxcString(amount))
	}
	sc, err := scanAgentSchedule(s.pool.QueryRow(ctx, `
		INSERT INTO agent_payment_schedules (id, workspace_id, from_agent_id, to_listing_id, amount_ulxc, memo, every, next_run_at)
		SELECT $1, $2, f.id, $4, $5, $6, $7, $8 FROM agent_accounts f WHERE f.id = $3 AND f.workspace_id = $2
		RETURNING `+agentScheduleColumns,
		"sch_"+uuid.NewString(), workspaceID, fromAgentID, listingID, amount, memo, every, firstRunAt.UTC()))
	if errors.Is(err, pgx.ErrNoRows) {
		return sc, ErrAgentNotFound
	}
	if err != nil {
		return sc, fmt.Errorf("economy: create schedule: %w", err)
	}
	return sc, nil
}

// CreateAgentSchedule schedules fromAgentID to pay toAgentID amount µLXC every hour, day, week or month,
// the first time at firstRunAt.
func (s *DualTokenStore) CreateAgentSchedule(ctx context.Context, workspaceID, fromAgentID, toAgentID string, amount int64,
	memo, every string, firstRunAt time.Time) (AgentSchedule, error) {
	switch {
	case amount <= 0:
		return AgentSchedule{}, fmt.Errorf("%w: the amount must be positive", ErrAgentRule)
	case every != "hour" && every != "day" && every != "week" && every != "month":
		return AgentSchedule{}, fmt.Errorf("%w: every must be hour, day, week or month", ErrAgentRule)
	case fromAgentID == toAgentID:
		return AgentSchedule{}, ErrSameAgent
	}
	var payeeWorkspace string
	if err := s.pool.QueryRow(ctx, `SELECT workspace_id FROM agent_accounts WHERE id = $1`, toAgentID).Scan(&payeeWorkspace); err == nil {
		if err := workspace.CheckMoneyWall(ctx, s.pool, workspaceID, payeeWorkspace); err != nil {
			return AgentSchedule{}, err
		}
	}
	sc, err := scanAgentSchedule(s.pool.QueryRow(ctx, `
		INSERT INTO agent_payment_schedules (id, workspace_id, from_agent_id, to_agent_id, amount_ulxc, memo, every, next_run_at)
		SELECT $1, $2, f.id, t.id, $5, $6, $7, $8 FROM agent_accounts f, agent_accounts t
		 WHERE f.id = $3 AND f.workspace_id = $2 AND t.id = $4 -- B22.3: the payee may be any agent on Talyvor
		RETURNING `+agentScheduleColumns,
		"sch_"+uuid.NewString(), workspaceID, fromAgentID, toAgentID, amount, memo, every, firstRunAt.UTC()))
	if errors.Is(err, pgx.ErrNoRows) {
		return sc, ErrAgentNotFound
	}
	if err != nil {
		return sc, fmt.Errorf("economy: create schedule: %w", err)
	}
	return sc, nil
}

// ListAgentSchedules reads a workspace's schedules, active first, then newest.
func (s *DualTokenStore) ListAgentSchedules(ctx context.Context, workspaceID string) ([]AgentSchedule, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+agentScheduleColumns+` FROM agent_payment_schedules WHERE workspace_id = $1
		ORDER BY active DESC, created_at DESC, id LIMIT 500`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: schedules: %w", err)
	}
	defer rows.Close()
	out := []AgentSchedule{}
	for rows.Next() {
		sc, err := scanAgentSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// ListAgentScheduleRuns reads a schedule's runs, newest first.
func (s *DualTokenStore) ListAgentScheduleRuns(ctx context.Context, workspaceID, scheduleID string) ([]AgentScheduleRun, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_payment_schedules WHERE id = $1 AND workspace_id = $2)`,
		scheduleID, workspaceID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("economy: schedule runs: %w", err)
	}
	if !exists {
		return nil, ErrScheduleNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT tick_at, outcome, COALESCE(entry_id::text, ''), COALESCE(use_id, ''), COALESCE(payment_id, ''), detail, created_at
		FROM agent_schedule_runs WHERE schedule_id = $1 ORDER BY tick_at DESC LIMIT 500`, scheduleID)
	if err != nil {
		return nil, fmt.Errorf("economy: schedule runs: %w", err)
	}
	defer rows.Close()
	out := []AgentScheduleRun{}
	for rows.Next() {
		var r AgentScheduleRun
		if err := rows.Scan(&r.TickAt, &r.Outcome, &r.EntryID, &r.UseID, &r.PaymentID, &r.Detail, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CancelAgentSchedule stops a schedule; its runs are kept.
func (s *DualTokenStore) CancelAgentSchedule(ctx context.Context, workspaceID, scheduleID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE agent_payment_schedules SET active = false WHERE id = $1 AND workspace_id = $2`, scheduleID, workspaceID)
	if err != nil {
		return fmt.Errorf("economy: cancel schedule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrScheduleNotFound
	}
	return nil
}

// SetAgentTopUp sets an agent's automatic top-up: below belowULXC, back up to toULXC.
func (s *DualTokenStore) SetAgentTopUp(ctx context.Context, workspaceID, agentID string, belowULXC, toULXC int64) (AgentTopUp, error) {
	t := AgentTopUp{AgentID: agentID, BelowULXC: belowULXC, ToULXC: toULXC}
	if belowULXC <= 0 || toULXC <= belowULXC {
		return t, fmt.Errorf("%w: a top-up needs 0 < below_ulxc < to_ulxc", ErrAgentRule)
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO agent_topups (agent_id, workspace_id, below_ulxc, to_ulxc)
		SELECT id, workspace_id, $3, $4 FROM agent_accounts WHERE id = $1 AND workspace_id = $2
		ON CONFLICT (agent_id) DO UPDATE SET below_ulxc = EXCLUDED.below_ulxc, to_ulxc = EXCLUDED.to_ulxc, updated_at = now()`,
		agentID, workspaceID, belowULXC, toULXC)
	if err != nil {
		return t, fmt.Errorf("economy: set top-up: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return t, ErrAgentNotFound
	}
	return t, nil
}

// GetAgentTopUp reads an agent's top-up; ok is false when it has none.
func (s *DualTokenStore) GetAgentTopUp(ctx context.Context, workspaceID, agentID string) (t AgentTopUp, ok bool, err error) {
	t.AgentID = agentID
	err = s.pool.QueryRow(ctx, `SELECT below_ulxc, to_ulxc FROM agent_topups WHERE agent_id = $1 AND workspace_id = $2`,
		agentID, workspaceID).Scan(&t.BelowULXC, &t.ToULXC)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, false, nil
	}
	return t, err == nil, err
}

// RemoveAgentTopUp removes an agent's top-up.
func (s *DualTokenStore) RemoveAgentTopUp(ctx context.Context, workspaceID, agentID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM agent_topups WHERE agent_id = $1 AND workspace_id = $2`, agentID, workspaceID); err != nil {
		return fmt.Errorf("economy: remove top-up: %w", err)
	}
	return nil
}

// RunAgentSchedules runs every schedule tick due at now, each exactly once, then renews every marketplace licence due
// (B32.20), then every top-up due.
func (s *DualTokenStore) RunAgentSchedules(ctx context.Context, now time.Time) (ScheduleRunResult, error) {
	var res ScheduleRunResult
	var tickErr error
	for i := 0; i < maxTicksPerRun; i++ {
		outcome, err := s.runScheduleTick(ctx, now)
		if err != nil {
			tickErr = err
			break
		}
		if outcome == "" {
			break
		}
		if outcome == "paid" {
			res.Paid++
		} else {
			res.Refused++
		}
	}
	if r, ok := s.listings.(LicenceRenewer); ok { // a failed schedule tick does not hold the licences' renewals up
		var err error
		if res.Renewed, res.Unpaid, err = r.RenewLicences(ctx, now, s); err != nil {
			return res, errors.Join(tickErr, err)
		}
	}
	if a, ok := s.listings.(IdleLicenceAlerter); ok { // an alert that fails holds no money up: it is logged and tried next tick
		idle, err := a.AlertIdleLicences(ctx, now)
		if err != nil {
			slog.Warn("agents: idle licences", slog.String("err", err.Error()))
		}
		for _, l := range idle {
			s.pushDevices(ctx, l.WorkspaceID, nativepush.Message{Title: "Not used in 30 days",
				Body: l.Title + " renews, but nobody has used it for 30 days. Cancel it?",
				Data: map[string]string{"type": "idle_licence", "licence_id": l.LicenceID, "workspace_id": l.WorkspaceID}})
		}
		res.IdleAlerted = len(idle)
	}
	if tickErr != nil {
		return res, tickErr
	}
	rows, err := s.pool.Query(ctx, `SELECT workspace_id, agent_id FROM agent_topups`)
	if err != nil {
		return res, fmt.Errorf("economy: top-ups: %w", err)
	}
	var due [][2]string
	for rows.Next() {
		var ws, agent string
		if err := rows.Scan(&ws, &agent); err != nil {
			rows.Close()
			return res, err
		}
		due = append(due, [2]string{ws, agent})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("economy: top-ups: %w", err)
	}
	for _, d := range due {
		topped, err := s.topUpAgent(ctx, d[0], d[1])
		if err != nil {
			return res, err
		}
		if topped > 0 {
			res.ToppedUp++
		}
	}
	return res, nil
}

// runScheduleTick runs the earliest due tick of any schedule, in one transaction holding the schedule's
// row: the payment, its run row and the next tick. It returns "" when nothing is due.
func (s *DualTokenStore) runScheduleTick(ctx context.Context, now time.Time) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var workspaceID string
	var sc AgentSchedule
	err = tx.QueryRow(ctx, `SELECT workspace_id, id, from_agent_id, COALESCE(to_agent_id, ''), COALESCE(to_listing_id, ''), COALESCE(to_payee_id, ''),
		amount_ulxc, amount_minor, currency, memo, every, next_run_at
		FROM agent_payment_schedules WHERE active AND next_run_at <= $1 ORDER BY next_run_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, now).
		Scan(&workspaceID, &sc.ID, &sc.FromAgentID, &sc.ToAgentID, &sc.ToListingID, &sc.ToPayeeID, &sc.AmountULXC, &sc.AmountMinor, &sc.Currency,
			&sc.Memo, &sc.Every, &sc.NextRunAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("economy: due schedule: %w", err)
	}
	payee := Payee{Kind: "agent", ID: sc.ToAgentID} // the payer's payee lists judge a scheduled payment too (B28.303)
	if sc.ToListingID != "" {
		payee = Payee{Kind: "listing", ID: sc.ToListingID}
	}
	payCtx := WithAgentRequest(ctx, AgentRequest{Payment: true, At: now, Listing: sc.ToListingID, Payee: payee,
		Fingerprint: "schedule:" + sc.ID + ":" + sc.NextRunAt.UTC().Format(time.RFC3339)})
	// The payment in a savepoint: a refusal is undone and recorded, and the tick still counts as run.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return "", err
	}
	outcome, entryID, useID, paymentID, detail := "paid", any(nil), any(nil), any(nil), ""
	entry := uuid.New()
	var perr error
	var payeeWS string
	if sc.ToAgentID != "" {
		if err := tx.QueryRow(ctx, `SELECT workspace_id FROM agent_accounts WHERE id = $1`, sc.ToAgentID).Scan(&payeeWS); err != nil {
			return "", fmt.Errorf("economy: schedule payee: %w", err)
		}
	}
	switch {
	case sc.ToPayeeID != "": // B30.18: a payment out in its own transactions, under the tick's key, so a tick run twice pays once
		var p OutsidePayment
		p, perr = s.PayOutside(ctx, workspaceID, OutsidePayment{AgentID: sc.FromAgentID, PayeeID: sc.ToPayeeID, AmountMinor: sc.AmountMinor,
			Currency: sc.Currency, Reference: sc.Memo, IdempotencyKey: "schedule:" + sc.ID + ":" + sc.NextRunAt.UTC().Format(time.RFC3339), at: now})
		if perr == nil {
			paymentID, detail = p.ID, p.Status
		}
	case sc.ToListingID != "":
		useID, perr = s.payListingTx(payCtx, sp, workspaceID, sc)
	case payeeWS != workspaceID: // B22.3: another workspace's agent is paid by a transfer
		var t AgentTransfer
		t, perr = s.transferTx(payCtx, sp, AgentTransfer{FromWorkspaceID: workspaceID, FromAgentID: sc.FromAgentID, ToWorkspaceID: payeeWS,
			ToAgentID: sc.ToAgentID, AmountULXC: sc.AmountULXC, Memo: sc.Memo, ScheduleID: sc.ID}, true)
		if perr == nil {
			entry = uuid.MustParse(t.EntryID)
		}
	default:
		_, perr = payAgentTx(payCtx, sp, workspaceID,
			AgentPayment{FromAgentID: sc.FromAgentID, ToAgentID: sc.ToAgentID, AmountULXC: sc.AmountULXC, Memo: sc.Memo}, entry)
	}
	switch {
	case perr == nil:
		if err := sp.Commit(ctx); err != nil {
			return "", err
		}
		if sc.ToListingID == "" && sc.ToPayeeID == "" {
			entryID = entry
		}
	case errors.Is(perr, ErrAgentFunds), errors.Is(perr, ErrAgentRule), errors.Is(perr, ErrApprovalRequired), errors.Is(perr, ErrAgentNotFound),
		errors.Is(perr, ErrAgentOwnerless), errors.Is(perr, ErrListingPayee), errors.Is(perr, ErrOwnerUnverified),
		errors.Is(perr, ErrCapabilityNotCleared), errors.Is(perr, workspace.ErrMoneyWall),
		errors.Is(perr, ErrOutsidePayment), errors.Is(perr, ErrPayeeNotFound), errors.Is(perr, ErrPayeeUnconfirmed), errors.Is(perr, ErrMoneyAccountNotFound),
		errors.Is(perr, ErrMoneyAccountNotOpen), errors.As(perr, new(*screening.Refusal)),
		// a tick's key a caller used first, or no partner or screener: refused and advanced, never a tick that stalls every schedule
		errors.Is(perr, ErrIdempotencyKeyReused), errors.Is(perr, ErrAccountNotOpened), errors.Is(perr, partners.ErrScreeningUnavailable):
		if err := sp.Rollback(ctx); err != nil {
			return "", err
		}
		outcome, detail = "refused", perr.Error()
	default:
		return "", perr
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_schedule_runs (schedule_id, tick_at, outcome, entry_id, use_id, payment_id, detail) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		sc.ID, sc.NextRunAt, outcome, entryID, useID, paymentID, detail); err != nil {
		return "", fmt.Errorf("economy: record schedule run: %w", err)
	}
	if _, err := advanceSchedule(ctx, tx, sc); err != nil {
		return "", err
	}
	return outcome, tx.Commit(ctx)
}

// topUpAgent brings an agent below its top-up threshold back up to its target, from the workspace's
// unallocated LXC (as much as there is). It returns what it moved.
func (s *DualTokenStore) topUpAgent(ctx context.Context, workspaceID, agentID string) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Lock order everywhere: the agent, then the workspace balance.
	if err := lockAgent(ctx, tx, workspaceID, agentID); err != nil {
		return 0, err
	}
	var below, to int64
	err = tx.QueryRow(ctx, `SELECT below_ulxc, to_ulxc FROM agent_topups WHERE agent_id = $1`, agentID).Scan(&below, &to)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("economy: top-up: %w", err)
	}
	if requireOwner(ctx, tx, agentID) != nil { // B19.11: an agent with no owner is not given a balance
		return 0, nil
	}
	bal, err := accountBalance(ctx, tx, workspaceID, agentAccount(agentID))
	if err != nil || bal >= below {
		return 0, err
	}
	wsBal, _, _, err := readLXCBalance(ctx, tx, workspaceID)
	if err != nil {
		return 0, err
	}
	var allocated int64
	if err := tx.QueryRow(ctx, allocatedSQL, workspaceID).Scan(&allocated); err != nil {
		return 0, fmt.Errorf("economy: allocated LXC: %w", err)
	}
	amount := min(to-bal, wsBal-allocated)
	if amount <= 0 {
		return 0, nil
	}
	if err := postEntry(ctx, tx, workspaceID, "topup", "", leg{"workspace", -amount}, leg{agentAccount(agentID), amount}); err != nil {
		return 0, err
	}
	return amount, tx.Commit(ctx)
}

// payListingTx pays one tick of a schedule to a marketplace listing, in sp: under the paying agent's row
// lock, the listing's price then is judged by its rules and recorded as a billed use, stamped at the tick,
// which the marketplace meters onto the company's monthly bill. Nothing is taken from the agent's balance.
func (s *DualTokenStore) payListingTx(ctx context.Context, sp pgx.Tx, workspaceID string, sc AgentSchedule) (any, error) {
	if s.listings == nil {
		return nil, fmt.Errorf("%w: the marketplace is not configured", ErrListingPayee)
	}
	if err := lockAgent(ctx, sp, workspaceID, sc.FromAgentID); err != nil {
		return nil, err
	}
	useID, refusal, err := s.listings.ChargeScheduledListing(ctx, sp, workspaceID, sc.FromAgentID, sc.ToListingID, sc.AmountULXC, sc.NextRunAt,
		func(price int64) error {
			return enforceAgentRules(ctx, sp, workspaceID, sc.FromAgentID, price, "schedule:"+sc.ID)
		})
	if err != nil {
		return nil, err
	}
	if refusal != "" {
		return nil, fmt.Errorf("%w: %s", ErrListingPayee, refusal)
	}
	return useID, nil
}
