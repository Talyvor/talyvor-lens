package economy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// money_payouts.go — B30.21: BULK PAYOUTS.
//
// A payout batch is up to 1,000 payments out (B30.17) uploaded at once — each row a saved payee, an amount, a currency
// and a reference — paid from the company's accounts, or with an agent's key from that agent's. Every row is validated
// when the batch arrives: its payee must be payable (saved, confirmed and screened), its amount positive in GBP, EUR or
// USD, its reference up to 140 characters, the payment screened, and the paying account must hold enough money for it
// and the valid rows before it. A row that fails is reported with the reason and never paid; the rest wait, moving
// nothing, for one approval. When the owner or an admin approves the batch, each valid row is paid as a payment out
// under its own idempotency key, so approving twice — or again after a crash half-way — pays nothing twice. An agent's
// rows are judged by its rules as any payment is; a row they refuse is reported failed with the reason.

var (
	// ErrPayoutBatch: a batch without rows, with more than 1,000, or without an idempotency key.
	ErrPayoutBatch = errors.New("economy: invalid payout batch")
	// ErrPayoutBatchNotFound: no such batch in this workspace, or not the agent's.
	ErrPayoutBatchNotFound = errors.New("economy: no such payout batch")
)

// The states of a payout batch, and of a row before it is paid.
const (
	PayoutAwaitingApproval = "awaiting_approval"
	PayoutSending          = "sending"
	PayoutSent             = "sent"
	PayoutRowValid         = "valid"
	PayoutRowInvalid       = "invalid"
	MaxPayoutRows          = 1000
)

// PayoutRow is one payment in a batch: as uploaded, how it was validated, and then the payment it became.
type PayoutRow struct {
	Line        int    `json:"line"`
	PayeeID     string `json:"payee_id"`
	PayeeName   string `json:"payee_name,omitempty"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Reference   string `json:"reference"`
	Status      string `json:"status"`               // valid or invalid at upload; then its payment's: pending, sent, completed, failed, returned
	PaymentID   string `json:"payment_id,omitempty"` // the payment out it became
	Detail      string `json:"detail,omitempty"`     // why it is invalid or failed; a row uploaded with one is invalid for that reason
}

// PayoutBatch is an upload of payments out and where it stands.
type PayoutBatch struct {
	ID             string           `json:"id"`
	WorkspaceID    string           `json:"workspace_id"`
	AgentID        string           `json:"agent_id,omitempty"`
	Funding        string           `json:"funding"`
	IdempotencyKey string           `json:"idempotency_key"`
	Status         string           `json:"status"`
	Rows           []PayoutRow      `json:"rows,omitempty"` // every row when one batch is read; none in a list
	RowsTotal      int              `json:"rows_total"`
	RowsValid      int              `json:"rows_valid"` // valid at upload: what an approval pays
	RowsInvalid    int              `json:"rows_invalid"`
	TotalsMinor    map[string]int64 `json:"totals_minor"` // of the valid rows, by currency
	ApprovedBy     string           `json:"approved_by,omitempty"`
	ApprovedAt     *time.Time       `json:"approved_at,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`

	rowsSHA256 string // of the rows as uploaded
}

// payoutRowKey is the idempotency key of the payment out a row becomes.
func payoutRowKey(batchID string, line int) string {
	return "payout:" + batchID + ":" + strconv.Itoa(line)
}

const payoutBatchCols = `id, workspace_id, agent_id, funding, idempotency_key, status, rows_total, rows_valid, totals_minor, approved_by,
	approved_at, created_at, updated_at, rows_sha256`

func scanPayoutBatch(row pgx.Row, withRows bool) (PayoutBatch, error) {
	var b PayoutBatch
	dest := []any{&b.ID, &b.WorkspaceID, &b.AgentID, &b.Funding, &b.IdempotencyKey, &b.Status, &b.RowsTotal, &b.RowsValid, &b.TotalsMinor,
		&b.ApprovedBy, &b.ApprovedAt, &b.CreatedAt, &b.UpdatedAt, &b.rowsSHA256}
	if withRows {
		dest = append(dest, &b.Rows)
	}
	err := row.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, ErrPayoutBatchNotFound
	}
	b.RowsInvalid = b.RowsTotal - b.RowsValid
	return b, err
}

// CreatePayoutBatch validates in.Rows for workspaceID — from in.AgentID's accounts when that is set, else the
// company's — and files them, moving nothing, for approval. The same key with the same rows answers the batch it
// first made; with other rows, ErrIdempotencyKeyReused.
func (s *DualTokenStore) CreatePayoutBatch(ctx context.Context, workspaceID string, in PayoutBatch) (PayoutBatch, error) {
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.Funding == "" {
		in.Funding = FundingTest
	}
	switch {
	case len(in.Rows) == 0 || len(in.Rows) > MaxPayoutRows:
		return PayoutBatch{}, fmt.Errorf("%w: a batch has from 1 to %d rows, not %d", ErrPayoutBatch, MaxPayoutRows, len(in.Rows))
	case in.Funding != FundingTest && in.Funding != FundingLive:
		return PayoutBatch{}, fmt.Errorf("%w: funding is test or live, not %q", ErrPayoutBatch, in.Funding)
	case in.IdempotencyKey == "" || len(in.IdempotencyKey) > 100:
		return PayoutBatch{}, fmt.Errorf("%w: a batch carries an idempotency key (the Idempotency-Key header, or idempotency_key), up to 100 characters", ErrPayoutBatch)
	case s.screener == nil:
		return PayoutBatch{}, fmt.Errorf("%w: no screener is set, so no money moves out", partners.ErrScreeningUnavailable)
	}
	rows := make([]PayoutRow, len(in.Rows))
	for i, r := range in.Rows {
		rows[i] = PayoutRow{Line: i + 1, PayeeID: strings.TrimSpace(r.PayeeID), AmountMinor: r.AmountMinor, Detail: r.Detail,
			Currency: strings.ToUpper(strings.TrimSpace(r.Currency)), Reference: strings.Join(strings.Fields(r.Reference), " ")}
	}
	uploaded, _ := json.Marshal(rows)
	sum := sha256.Sum256(uploaded)
	hash := hex.EncodeToString(sum[:])
	prior, err := s.payoutBatch(ctx, workspaceID, "", "", in.IdempotencyKey, true)
	switch {
	case err == nil && prior.rowsSHA256 == hash && prior.Funding == in.Funding && prior.AgentID == in.AgentID:
		return prior, nil
	case err == nil:
		return PayoutBatch{}, ErrIdempotencyKeyReused
	case !errors.Is(err, ErrPayoutBatchNotFound):
		return PayoutBatch{}, err
	}

	b := PayoutBatch{ID: "mpob_" + uuid.NewString(), WorkspaceID: workspaceID, AgentID: in.AgentID, Funding: in.Funding,
		IdempotencyKey: in.IdempotencyKey, Status: PayoutAwaitingApproval, Rows: rows, RowsTotal: len(rows), TotalsMinor: map[string]int64{}}
	held := map[string]int64{} // what the paying account in each currency holds, less the valid rows before this one
	for i := range b.Rows {
		r := &b.Rows[i]
		reason, err := s.validatePayoutRow(ctx, b, r, held)
		if err != nil {
			return PayoutBatch{}, err
		}
		r.Status = PayoutRowValid
		if reason != "" {
			r.Status, r.Detail = PayoutRowInvalid, reason
			continue
		}
		b.RowsValid++
		b.TotalsMinor[r.Currency] += r.AmountMinor
		held[r.Currency] -= r.AmountMinor
	}
	b, err = scanPayoutBatch(s.pool.QueryRow(ctx, `INSERT INTO money_payout_batches (id, workspace_id, idempotency_key, rows_sha256, agent_id,
		funding, status, rows, rows_total, rows_valid, totals_minor) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING `+payoutBatchCols+`, rows`, b.ID, workspaceID, b.IdempotencyKey, hash, b.AgentID, b.Funding, b.Status, b.Rows, b.RowsTotal,
		b.RowsValid, b.TotalsMinor), true)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // the same key, uploaded at the same moment: answer as that one did
		return s.CreatePayoutBatch(ctx, workspaceID, in)
	}
	if err != nil {
		return PayoutBatch{}, fmt.Errorf("economy: payout batch: %w", err)
	}
	return b, nil
}

// validatePayoutRow is why r may not be paid — "" when it may — and an error only when nothing can be validated
// (screening unavailable). held caches each currency's balance less the valid rows before r.
func (s *DualTokenStore) validatePayoutRow(ctx context.Context, b PayoutBatch, r *PayoutRow, held map[string]int64) (string, error) {
	switch {
	case r.Detail != "":
		return r.Detail, nil
	case r.PayeeID == "":
		return "a row names its payee (payee_id): save one at /v1/money/payees first", nil
	case MoneyCurrencies[r.Currency] == 0 || r.Currency == CurrencyUSDC:
		return fmt.Sprintf("a payment is in GBP, EUR or USD, not %q", r.Currency), nil
	case r.AmountMinor <= 0:
		return "a row's amount is a positive number of pence or cents", nil
	case len(r.Reference) > 140:
		return "a row's reference is up to 140 characters", nil
	}
	payee, err := s.PayableOutsidePayee(ctx, b.WorkspaceID, r.PayeeID)
	var refusal *screening.Refusal
	switch {
	case errors.Is(err, partners.ErrScreeningUnavailable):
		return "", err
	case errors.Is(err, ErrPayeeNotFound):
		return "no such payee " + r.PayeeID + ": save one at /v1/money/payees first", nil
	case err != nil:
		return err.Error(), nil
	}
	r.PayeeName = payee.Name
	err = s.screener.ScreenPayment(ctx, screening.Payment{WorkspaceID: b.WorkspaceID, ID: "payment_out:" + payoutRowKey(b.ID, r.Line),
		Capability: CapabilityPaymentsOut, Direction: "out", Counterparty: payee.Name, AmountMinor: r.AmountMinor, Currency: r.Currency, Funding: b.Funding})
	switch {
	case errors.As(err, &refusal):
		return err.Error(), nil
	case err != nil:
		return "", err
	}
	balance, ok := held[r.Currency]
	if !ok {
		from, err := s.payingAccount(ctx, b.WorkspaceID, OutsidePayment{AgentID: b.AgentID, Currency: r.Currency})
		if err != nil {
			return err.Error(), nil
		}
		if err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1 AND funding = $2`,
			from.id, b.Funding).Scan(&balance); err != nil {
			return "", fmt.Errorf("economy: payout batch: %w", err)
		}
		held[r.Currency] = balance
	}
	if balance < r.AmountMinor {
		return fmt.Sprintf("the account holds %s of %s money after the rows before this one, and this row is %s",
			minorString(balance, r.Currency), b.Funding, minorString(r.AmountMinor, r.Currency)), nil
	}
	return "", nil
}

// ApprovePayoutBatch is by's approval of workspaceID's batch id: every valid row is paid as a payment out, and the
// batch records each one's result. A batch already sent is answered as it stands; one left sending is finished.
func (s *DualTokenStore) ApprovePayoutBatch(ctx context.Context, workspaceID, id, by string) (PayoutBatch, error) {
	b, err := s.payoutBatch(ctx, workspaceID, "", id, "", true)
	if err != nil || b.Status == PayoutSent {
		return b, err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE money_payout_batches SET status = $2, approved_by = $3, approved_at = now(), updated_at = now()
		WHERE id = $1 AND status = $4`, b.ID, PayoutSending, by, PayoutAwaitingApproval); err != nil {
		return b, fmt.Errorf("economy: payout batch: %w", err)
	}
	ctx = context.WithoutCancel(ctx) // a caller that goes away mid-batch does not leave rows half sent
	for i := range b.Rows {
		r := &b.Rows[i]
		if r.Status == PayoutRowInvalid || r.PaymentID != "" {
			continue
		}
		p, err := s.PayOutside(ctx, workspaceID, OutsidePayment{AgentID: b.AgentID, PayeeID: r.PayeeID, AmountMinor: r.AmountMinor,
			Currency: r.Currency, Funding: b.Funding, Reference: r.Reference, IdempotencyKey: payoutRowKey(b.ID, r.Line)})
		if err != nil {
			r.Status, r.Detail = PaymentFailed, err.Error()
			continue
		}
		r.PaymentID, r.Status, r.Detail = p.ID, p.Status, p.Detail
	}
	b, err = scanPayoutBatch(s.pool.QueryRow(ctx, `UPDATE money_payout_batches SET status = $2, rows = $3, updated_at = now() WHERE id = $1
		RETURNING `+payoutBatchCols+`, rows`, b.ID, PayoutSent, b.Rows), true)
	if err != nil {
		return b, fmt.Errorf("economy: payout batch: %w", err)
	}
	return b, nil
}

// payoutBatch reads workspaceID's batch id, or the one under key — with agentID set, only that agent's.
func (s *DualTokenStore) payoutBatch(ctx context.Context, workspaceID, agentID, id, key string, withRows bool) (PayoutBatch, error) {
	b, err := scanPayoutBatch(s.pool.QueryRow(ctx, `SELECT `+payoutBatchCols+`, rows FROM money_payout_batches
		WHERE workspace_id = $1 AND ($2 = '' OR agent_id = $2) AND (($3 <> '' AND id = $3) OR ($3 = '' AND idempotency_key = $4))`,
		workspaceID, agentID, id, key), true)
	if err != nil && !errors.Is(err, ErrPayoutBatchNotFound) {
		return b, fmt.Errorf("economy: payout batch: %w", err)
	}
	if !withRows {
		b.Rows = nil
	}
	return b, err
}

// PayoutBatch reads workspaceID's batch id with every row — with agentID set, only that agent's.
func (s *DualTokenStore) PayoutBatch(ctx context.Context, workspaceID, agentID, id string) (PayoutBatch, error) {
	return s.payoutBatch(ctx, workspaceID, agentID, id, "", true)
}

// PayoutBatches is workspaceID's batches without their rows — with agentID set, only that agent's — newest first,
// at most 200.
func (s *DualTokenStore) PayoutBatches(ctx context.Context, workspaceID, agentID string) ([]PayoutBatch, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+payoutBatchCols+` FROM money_payout_batches WHERE workspace_id = $1 AND ($2 = '' OR agent_id = $2)
		ORDER BY created_at DESC, id LIMIT 200`, workspaceID, agentID)
	if err != nil {
		return nil, fmt.Errorf("economy: payout batches: %w", err)
	}
	defer rows.Close()
	out := []PayoutBatch{}
	for rows.Next() {
		b, err := scanPayoutBatch(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
