package economy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/nativepush"
)

// recurring_payments.go — B45.13: RECURRING PAYMENTS FROM A PASTED LIST, PREPARED BY AN ASSISTANT, SIGNED BY THE OWNER.
//
// PrepareRecurringPayments takes a list of saved payees, amounts and frequencies and pays nothing: each becomes a
// standing order (B30.18) that does not run, and the plan keeps one permission mandate draft (B40.2) for the owner to
// sign — those payees only, each payment at most the largest on the list, a month at most what the list adds up to,
// approval above the owner's ask-above amount, approval before a new payee, until the date given. Its one client is
// "schedule:<plan id>", which no caller can name, so the mandate lets only these standing orders pay on its own.
// When the owner signs exactly that draft, the orders start (CreatePermissionMandate). Each tick is then a payment
// request (B40.3) under the tick's key: inside the mandate it is paid at once and the owner's phones are told;
// outside it, and on an order's first payment, it waits for the owner's approval (B40.5). Payments out are RED.

// ErrRecurringPlan: a list that cannot become recurring payments.
var ErrRecurringPlan = errors.New("economy: invalid recurring payments")

// ClientSchedulePrefix names the client a recurring plan's standing orders prepare their payments as.
const ClientSchedulePrefix = "schedule:"

// maxRecurringPayments bounds one list.
const maxRecurringPayments = 50

// RecurringPaymentIn is one line of the list.
type RecurringPaymentIn struct {
	PayeeID     string     `json:"payee_id"`
	Currency    string     `json:"currency"`
	AmountMinor int64      `json:"amount_minor"`
	Every       string     `json:"every"` // day | week | month
	FirstRunAt  *time.Time `json:"first_run_at,omitempty"`
	Reference   string     `json:"reference,omitempty"`
}

// RecurringPlanIn is the list, whose mandate speaks for Principal, paid by AgentID ("" for the person), until Until.
// AskAbove is the owner's rule: a payment above it in its currency always waits for approval.
type RecurringPlanIn struct {
	AgentID   string               `json:"agent_id"`
	Principal MandatePrincipal     `json:"principal"`
	Payments  []RecurringPaymentIn `json:"payments"`
	AskAbove  map[string]int64     `json:"ask_above"`
	Until     time.Time            `json:"until"`
}

// RecurringMandateDraft is the mandate the owner signs: its canonical JSON, the WebAuthn challenge over it, and the
// workspace's passkeys that may sign it.
type RecurringMandateDraft struct {
	ID            string   `json:"id"`
	Canonical     string   `json:"canonical"`
	Challenge     string   `json:"challenge"`
	CredentialIDs []string `json:"credential_ids"`
}

// RecurringPlan is a prepared list: its standing orders and the mandate that starts them.
type RecurringPlan struct {
	ID        string                `json:"id"`
	Status    string                `json:"status"` // awaiting_signature | signed
	Schedules []AgentSchedule       `json:"schedules"`
	Mandate   RecurringMandateDraft `json:"mandate"`
	CreatedAt time.Time             `json:"created_at"`
}

// ticksPerMonth is the most ticks of a frequency a calendar month holds.
var ticksPerMonth = map[string]int64{"day": 31, "week": 5, "month": 1}

// PrepareRecurringPayments prepares in for workspaceID under idempotencyKey, by `by`. The same key and list return the
// plan kept the first time (again = true); the same key with another list is ErrPaymentRequestConflict.
func (s *DualTokenStore) PrepareRecurringPayments(ctx context.Context, workspaceID, by, idempotencyKey string, in RecurringPlanIn) (p RecurringPlan, again bool, err error) {
	bad := func(format string, args ...any) (RecurringPlan, bool, error) {
		return p, false, fmt.Errorf("%w: %s", ErrRecurringPlan, fmt.Sprintf(format, args...))
	}
	if idempotencyKey = strings.TrimSpace(idempotencyKey); idempotencyKey == "" {
		return bad("an idempotency_key is required")
	}
	if len(in.Payments) == 0 || len(in.Payments) > maxRecurringPayments {
		return bad("name between 1 and %d payments", maxRecurringPayments)
	}
	now := time.Now().UTC().Truncate(time.Second)
	in.Until = in.Until.UTC().Truncate(time.Second)
	if !in.Until.After(now) {
		return bad("until is the last day to pay: it must be in the future")
	}
	terms := PermissionMandateTerms{Principal: in.Principal, AgentID: in.AgentID, NewPayee: MandateNewPayee{Policy: NewPayeeAlwaysAsk},
		Limits: MandateLimits{PerPayment: map[string]int64{}, PerMonth: map[string]int64{}}, RequiresApprovalAbove: in.AskAbove,
		ValidFrom: now, ExpiresAt: in.Until}
	for i := range in.Payments {
		x := &in.Payments[i]
		x.PayeeID, x.Reference, x.Every = strings.TrimSpace(x.PayeeID), strings.TrimSpace(x.Reference), strings.TrimSpace(x.Every)
		x.Currency = strings.ToUpper(strings.TrimSpace(x.Currency))
		switch {
		case x.PayeeID == "":
			return bad("payment %d: name a saved payee's payee_id", i+1)
		case x.AmountMinor <= 0:
			return bad("payment %d: amount_minor must be more than 0", i+1)
		case x.Currency != CurrencyGBP && x.Currency != CurrencyEUR && x.Currency != CurrencyUSD:
			return bad("payment %d: a recurring payment is in GBP, EUR or USD, not %q", i+1, x.Currency)
		case ticksPerMonth[x.Every] == 0:
			return bad("payment %d: every must be day, week or month", i+1)
		case len(x.Reference) > 140:
			return bad("payment %d: a reference is up to 140 characters", i+1)
		case x.FirstRunAt != nil && x.FirstRunAt.After(in.Until):
			return bad("payment %d: its first run is after until", i+1)
		}
		if _, err := s.PayableOutsidePayee(ctx, workspaceID, x.PayeeID); err != nil {
			return bad("payment %d: %s", i+1, strings.TrimPrefix(err.Error(), "economy: "))
		}
		terms.Scope.Payees = append(terms.Scope.Payees, x.PayeeID)
		terms.Scope.Currencies = append(terms.Scope.Currencies, x.Currency)
		terms.Limits.PerPayment[x.Currency] = max(terms.Limits.PerPayment[x.Currency], x.AmountMinor)
		terms.Limits.PerMonth[x.Currency] += x.AmountMinor * ticksPerMonth[x.Every]
	}
	if in.AgentID != "" {
		var ok bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`,
			in.AgentID, workspaceID).Scan(&ok); err != nil {
			return p, false, fmt.Errorf("economy: recurring payments agent: %w", err)
		}
		if !ok {
			return p, false, ErrAgentNotFound
		}
	}
	args, _ := json.Marshal(in)
	argsHash := sha256Hex(string(args))
	var id, kept string
	err = s.pool.QueryRow(ctx, `SELECT id, args_hash FROM recurring_payment_plans WHERE workspace_id = $1 AND idempotency_key = $2`,
		workspaceID, idempotencyKey).Scan(&id, &kept)
	switch {
	case err == nil && kept != argsHash:
		return p, false, ErrPaymentRequestConflict
	case err == nil:
		p, err = s.RecurringPlan(ctx, workspaceID, id)
		return p, true, err
	case !errors.Is(err, pgx.ErrNoRows):
		return p, false, fmt.Errorf("economy: recurring payments: %w", err)
	}

	id = "rpp_" + uuid.NewString()
	terms.Clients = []string{ClientSchedulePrefix + id}
	canonical, _, _, err := s.PreparePermissionMandate(ctx, workspaceID, terms)
	if err != nil {
		return p, false, err
	}
	drafted, _ := parseCanonical(canonical)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return p, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `INSERT INTO recurring_payment_plans (id, workspace_id, idempotency_key, args_hash, mandate_id, canonical, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (workspace_id, idempotency_key) DO NOTHING`,
		id, workspaceID, idempotencyKey, argsHash, drafted.ID, canonical, by)
	if err != nil {
		return p, false, fmt.Errorf("economy: recurring payments: %w", err)
	}
	if tag.RowsAffected() == 0 { // the same key prepared at the same moment: that plan
		_ = tx.Rollback(ctx)
		return s.PrepareRecurringPayments(ctx, workspaceID, by, idempotencyKey, in)
	}
	for _, x := range in.Payments {
		first := now
		if x.FirstRunAt != nil && x.FirstRunAt.After(now) {
			first = x.FirstRunAt.UTC()
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_payment_schedules (id, workspace_id, from_agent_id, to_payee_id, amount_ulxc, amount_minor,
			currency, memo, every, next_run_at, end_at, active, recurring_plan_id, awaiting_signature)
			VALUES ($1, $2, NULLIF($3, ''), $4, 0, $5, $6, $7, $8, $9, $10, false, $11, true)`,
			"sch_"+uuid.NewString(), workspaceID, in.AgentID, x.PayeeID, x.AmountMinor, x.Currency, x.Reference, x.Every, first, in.Until, id); err != nil {
			return p, false, fmt.Errorf("economy: recurring payment: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return p, false, err
	}
	p, err = s.RecurringPlan(ctx, workspaceID, id)
	return p, false, err
}

// RecurringPlan reads workspaceID's plan id: its standing orders and its mandate draft, and whether it was signed.
func (s *DualTokenStore) RecurringPlan(ctx context.Context, workspaceID, id string) (RecurringPlan, error) {
	p := RecurringPlan{ID: id, Status: "awaiting_signature", Schedules: []AgentSchedule{}}
	var signed bool
	err := s.pool.QueryRow(ctx, `SELECT p.mandate_id, p.canonical, p.created_at, EXISTS (SELECT 1 FROM permission_mandates m WHERE m.id = p.mandate_id)
		FROM recurring_payment_plans p WHERE p.id = $1 AND p.workspace_id = $2`, id, workspaceID).
		Scan(&p.Mandate.ID, &p.Mandate.Canonical, &p.CreatedAt, &signed)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, fmt.Errorf("%w: no such plan in this workspace", ErrRecurringPlan)
	}
	if err != nil {
		return p, fmt.Errorf("economy: recurring plan: %w", err)
	}
	if signed {
		p.Status = "signed"
	}
	p.Mandate.Challenge = MandateChallenge(p.Mandate.Canonical)
	keys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return p, err
	}
	p.Mandate.CredentialIDs = []string{}
	for _, k := range keys {
		p.Mandate.CredentialIDs = append(p.Mandate.CredentialIDs, k.CredentialID)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+agentScheduleColumns+` FROM agent_payment_schedules WHERE recurring_plan_id = $1 AND workspace_id = $2
		ORDER BY created_at, id`, id, workspaceID)
	if err != nil {
		return p, fmt.Errorf("economy: recurring plan: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		sc, err := scanAgentSchedule(rows)
		if err != nil {
			return p, err
		}
		p.Schedules = append(p.Schedules, sc)
	}
	return p, rows.Err()
}

// startRecurringPlan starts the standing orders of the plan whose mandate draft is exactly canonical, now signed as
// mandateID; a tick whose time passed while it waited runs now, once.
func startRecurringPlan(ctx context.Context, tx pgx.Tx, workspaceID, mandateID, canonical string) error {
	if _, err := tx.Exec(ctx, `UPDATE agent_payment_schedules s SET active = true, awaiting_signature = false, next_run_at = GREATEST(s.next_run_at, now())
		FROM recurring_payment_plans p WHERE p.mandate_id = $2 AND p.workspace_id = $1 AND p.canonical = $3
		AND s.recurring_plan_id = p.id AND s.workspace_id = $1 AND s.awaiting_signature`, workspaceID, mandateID, canonical); err != nil {
		return fmt.Errorf("economy: start recurring payments: %w", err)
	}
	return nil
}

// runRecurringTick runs one tick of a plan's standing order as a payment request under the tick's key, so a tick run
// twice prepares and pays once: paid when the mandate lets it pay on its own, awaiting_approval otherwise, refused
// when it cannot be paid. A standing order's first payment always waits for the owner.
func (s *DualTokenStore) runRecurringTick(ctx context.Context, tx pgx.Tx, workspaceID string, sc AgentSchedule) (outcome, requestID, paymentID, detail string, err error) {
	var paidBefore bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_schedule_runs r JOIN payment_request_items i ON i.request_id = r.request_id
		WHERE r.schedule_id = $1 AND COALESCE(i.payment_id, '') <> '')`, sc.ID).Scan(&paidBefore); err != nil {
		return "", "", "", "", fmt.Errorf("economy: recurring payment paid before: %w", err)
	}
	key := "schedule:" + sc.ID + ":" + sc.NextRunAt.UTC().Format(time.RFC3339Nano) // the tick as kept, to the microsecond
	r, _, err := s.PreparePaymentRequest(ctx, workspaceID, ClientSchedulePrefix+sc.ID, key, PaymentRequestIn{
		Client: ClientSchedulePrefix + sc.RecurringPlanID, AgentID: sc.FromAgentID, FirstPayment: !paidBefore,
		Items: []PaymentRequestItemIn{{PayeeID: sc.ToPayeeID, Currency: sc.Currency, AmountMinor: sc.AmountMinor, Reference: sc.Memo}}})
	if errors.Is(err, ErrPaymentRequest) || errors.Is(err, ErrPaymentRequestConflict) {
		return "refused", "", "", err.Error(), nil
	}
	if err != nil {
		return "", "", "", "", err
	}
	it := r.Items[0]
	switch r.Status {
	case "pending_approval":
		return "awaiting_approval", r.ID, "", it.Reason, nil
	case ItemAuto, "approved":
		if r, err = s.PayPaymentRequest(ctx, workspaceID, "", r.ID); errors.Is(err, ErrPaymentRequest) {
			return "refused", r.ID, "", err.Error(), nil
		}
		if err != nil {
			return "", "", "", "", err
		}
		if it = r.Items[0]; it.PaymentID == "" {
			return "refused", r.ID, "", it.PayDetail, nil
		}
		s.notifyRecurringPaid(workspaceID, r)
		return "paid", r.ID, it.PaymentID, it.PayStatus, nil
	}
	return "refused", r.ID, "", it.Reason, nil
}

// notifyRecurringPaid tells the workspace's phones, in the background, of a recurring payment paid on its own: the
// payee and the amount, never account details.
func (s *DualTokenStore) notifyRecurringPaid(workspaceID string, r PaymentRequest) {
	if s.nativePusher == nil {
		return
	}
	var totals []string
	for _, c := range slices.Sorted(maps.Keys(r.Totals)) {
		totals = append(totals, formatMinor(c, r.Totals[c]))
	}
	m := nativepush.Message{Title: "Paid", Body: "Paid " + r.Items[0].PayeeName + " · " + strings.Join(totals, " + ") + " · recurring, inside your mandate",
		Data: map[string]string{"type": "recurring_payment", "request_id": r.ID, "workspace_id": workspaceID}}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s.pushDevices(ctx, workspaceID, m)
	}()
}
