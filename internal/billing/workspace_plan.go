package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/fees"
	"github.com/talyvor/lens/internal/operatoraudit"
	"github.com/talyvor/lens/internal/plans"
)

// workspace_plan.go — B32.10: which plan a workspace is on, for every feature that charges or gates by plan.
// A plan is bought through Stripe (subscriptions.plan, set by the webhook from the Price), or — Enterprise —
// contracted and invoiced by the operator, who records it here with the contract's own fees.

// PlanQuerier is a pool or a transaction.
type PlanQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PlanOf answers which plan workspaceID is on: enterprise while the operator has it on a contract; else the
// plan its paying subscription bills (trialing, active or past_due — unpaid is Stripe having given up); else, for
// a synthetic workspace, the plan the testers put it on (B35.1); else free. A subscription whose Price is no
// plan's answers free too: there is no plan to charge or gate it by. internal/plans answers it, so the packages
// billing builds on can gate by plan too (B32.12).
func PlanOf(ctx context.Context, db PlanQuerier, workspaceID string) (string, error) {
	return plans.PlanOf(ctx, db, workspaceID)
}

// PlatformFeeBPS is the platform fee on workspaceID's AI spend charged to credits, in basis points (B32.11):
// the Enterprise contract's own figure where the operator recorded one; else its plan's in s.PlatformFeeBPS —
// plus, pro, max and byok take team's, and a workspace with no plan, or a plan the setting does not name,
// takes free's.
func PlatformFeeBPS(ctx context.Context, db PlanQuerier, workspaceID string, s fees.Settings) (int64, error) {
	c, err := ContractOf(ctx, db, workspaceID)
	if err != nil {
		return 0, err
	}
	if c != nil && c.PlatformFeeBPS != nil {
		return *c.PlatformFeeBPS, nil
	}
	plan, err := PlanOf(ctx, db, workspaceID)
	if err != nil {
		return 0, err
	}
	switch plan {
	case "plus", "pro", "max", BYOKPlan:
		plan = TeamPlan
	}
	if bps, ok := s.PlatformFeeBPS[plan]; ok {
		return bps, nil
	}
	return s.PlatformFeeBPS[FreePlan], nil
}

// FXMarginBPS is the margin over the reference rate on workspaceID's currency conversions, in basis points (B56.23):
// the Enterprise contract's own figure where the operator recorded one; else its plan's in s.FXMarginBPS, the plans
// mapped as PlatformFeeBPS maps them. It answers the plan too, mapped the same way (B30.29: free converts test money only).
func FXMarginBPS(ctx context.Context, db PlanQuerier, workspaceID string, s fees.Settings) (int64, string, error) {
	plan, err := PlanOf(ctx, db, workspaceID)
	if err != nil {
		return 0, "", err
	}
	c, err := ContractOf(ctx, db, workspaceID)
	if err != nil {
		return 0, "", err
	}
	if c != nil && c.FXMarginBPS != nil {
		return *c.FXMarginBPS, plan, nil
	}
	switch plan {
	case "plus", "pro", "max", BYOKPlan:
		plan = TeamPlan
	}
	if bps, ok := s.FXMarginBPS[plan]; ok {
		return bps, plan, nil
	}
	return s.FXMarginBPS[FreePlan], FreePlan, nil
}

// Contract is an Enterprise contract as the operator records it: the contract's own platform fee and FX
// margin, in basis points, where it agreed one (nil follows the enterprise figure in internal/fees), and its own
// limits on agents and seats, -1 unlimited (nil follows enterprise's gates in LENS_PLAN_GATES, B32.12).
type Contract struct {
	WorkspaceID    string    `json:"workspace_id"`
	Plan           string    `json:"plan"`
	PlatformFeeBPS *int64    `json:"platform_fee_bps"`
	FXMarginBPS    *int64    `json:"fx_margin_bps"`
	Agents         *int64    `json:"agents"`
	Seats          *int64    `json:"seats"`
	Reference      string    `json:"reference"`
	SetBy          string    `json:"set_by"`
	SetAt          time.Time `json:"set_at"`
}

// ErrInvalidContract is a contract the operator cannot record; the message says why.
var ErrInvalidContract = errors.New("billing: invalid contract")

// ErrNoContract is ending a contract a workspace does not have.
var ErrNoContract = errors.New("billing: the workspace has no contract")

// ContractOf is workspaceID's contract, or nil.
func ContractOf(ctx context.Context, db PlanQuerier, workspaceID string) (*Contract, error) {
	c := Contract{WorkspaceID: workspaceID}
	err := db.QueryRow(ctx, `
		SELECT plan, platform_fee_bps, fx_margin_bps, agents, seats, reference, set_by, set_at
		FROM workspace_contracts WHERE workspace_id = $1`, workspaceID).
		Scan(&c.Plan, &c.PlatformFeeBPS, &c.FXMarginBPS, &c.Agents, &c.Seats, &c.Reference, &c.SetBy, &c.SetAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("billing: the contract of %s: %w", workspaceID, err)
	}
	return &c, nil
}

// SetContract puts c.WorkspaceID on an Enterprise contract, or replaces its terms, and records the operator who
// did it in the operator audit trail — in one transaction.
func SetContract(ctx context.Context, pool *pgxpool.Pool, c Contract, actor string) (*Contract, error) {
	c.WorkspaceID, c.Reference, actor = strings.TrimSpace(c.WorkspaceID), strings.TrimSpace(c.Reference), strings.TrimSpace(actor)
	if c.Plan == "" {
		c.Plan = EnterprisePlan
	}
	switch {
	case c.Plan != EnterprisePlan:
		return nil, fmt.Errorf("%w: the operator contracts only %s; %q is bought through Stripe", ErrInvalidContract, EnterprisePlan, c.Plan)
	case c.WorkspaceID == "":
		return nil, fmt.Errorf("%w: no workspace", ErrInvalidContract)
	case actor == "":
		return nil, fmt.Errorf("%w: no operator named. Name one in X-Talyvor-Operator or the body's actor", ErrInvalidContract)
	}
	for name, v := range map[string]*int64{"platform_fee_bps": c.PlatformFeeBPS, "fx_margin_bps": c.FXMarginBPS} {
		if v != nil && (*v < 0 || *v > fees.BPSDenominator) {
			return nil, fmt.Errorf("%w: %s is %d, outside 0 to %d basis points", ErrInvalidContract, name, *v, fees.BPSDenominator)
		}
	}
	for name, v := range map[string]*int64{"agents": c.Agents, "seats": c.Seats} {
		if v != nil && *v < plans.Unlimited {
			return nil, fmt.Errorf("%w: %s is %d; it is a count, or -1 for unlimited", ErrInvalidContract, name, *v)
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspaces WHERE id = $1)`, c.WorkspaceID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: no workspace %q", ErrInvalidContract, c.WorkspaceID)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO workspace_contracts (workspace_id, plan, platform_fee_bps, fx_margin_bps, agents, seats, reference, set_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (workspace_id) DO UPDATE SET plan = EXCLUDED.plan, platform_fee_bps = EXCLUDED.platform_fee_bps,
			fx_margin_bps = EXCLUDED.fx_margin_bps, agents = EXCLUDED.agents, seats = EXCLUDED.seats,
			reference = EXCLUDED.reference, set_by = EXCLUDED.set_by, set_at = NOW()
		RETURNING set_by, set_at`,
		c.WorkspaceID, c.Plan, c.PlatformFeeBPS, c.FXMarginBPS, c.Agents, c.Seats, c.Reference, actor).Scan(&c.SetBy, &c.SetAt); err != nil {
		return nil, fmt.Errorf("billing: record the contract: %w", err)
	}
	detail, _ := json.Marshal(c)
	if _, err := operatoraudit.RecordIn(ctx, tx, operatoraudit.Entry{Actor: actor, Action: "workspace.contract.set",
		Target: "workspace:" + c.WorkspaceID, Detail: string(detail)}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &c, nil
}

// EndContract takes workspaceID off its contract, back to the plan its subscription bills (or free), and records
// the operator who did it in the operator audit trail — in one transaction.
func EndContract(ctx context.Context, pool *pgxpool.Pool, workspaceID, actor string) error {
	if actor = strings.TrimSpace(actor); actor == "" {
		return fmt.Errorf("%w: no operator named. Name one in X-Talyvor-Operator or the body's actor", ErrInvalidContract)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ct, err := tx.Exec(ctx, `DELETE FROM workspace_contracts WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return fmt.Errorf("billing: end the contract: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNoContract
	}
	if _, err := operatoraudit.RecordIn(ctx, tx, operatoraudit.Entry{Actor: actor, Action: "workspace.contract.end",
		Target: "workspace:" + workspaceID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
