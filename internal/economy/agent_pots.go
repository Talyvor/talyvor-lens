package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// agent_pots.go — B22.7: POTS: AN AGENT KEEPS MONEY ASIDE FOR A GOAL.
//
// An agent splits its balance into named pots — a goal (with a target), a budget or a reserve — and moves
// credits between a pot and its main balance (MoveToPot, MoveFromPot): one agent_postings entry of two
// postings, agent:<id> ↔ pot:<pot id>, in the agent's workspace. A pot's credits are still the agent's and are
// counted with what the workspace's agents hold (allocatedSQL), but the agent spends only its main balance. A
// pot locked until a date refuses to give anything back before then. No interest: interest is RED. Pots are
// GREEN (rules_approvals_statements_pots), and an agent's statement shows each of them.

// ErrPotNotFound: no such pot for this agent.
var ErrPotNotFound = errors.New("economy: no such pot")

// ErrPotLocked: the pot is locked until a date that has not come.
var ErrPotLocked = errors.New("economy: this pot is locked")

// ErrPot: a pot that is not one — a missing name, an unknown kind, a name the agent already uses.
var ErrPot = errors.New("economy: pot")

// Pot is credits an agent keeps aside.
type Pot struct {
	ID          string     `json:"id"`
	AgentID     string     `json:"agent_id"`
	Name        string     `json:"name"`
	Kind        string     `json:"kind"` // goal | budget | reserve
	TargetULXC  int64      `json:"target_ulxc,omitempty"`
	LockedUntil *time.Time `json:"locked_until,omitempty"`
	BalanceULXC int64      `json:"balance_ulxc"`
	CreatedAt   time.Time  `json:"created_at"`
}

func potAccount(id string) string { return "pot:" + id }

// CreatePot gives workspaceID's agent a pot. lockedUntil, when not nil, locks it until then.
func (s *DualTokenStore) CreatePot(ctx context.Context, workspaceID, agentID, name, kind string, target int64, lockedUntil *time.Time) (Pot, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "" || len(name) > 64:
		return Pot{}, fmt.Errorf("%w: a pot needs a name of up to 64 characters", ErrPot)
	case kind != "goal" && kind != "budget" && kind != "reserve":
		return Pot{}, fmt.Errorf("%w: kind must be goal, budget or reserve", ErrPot)
	case target < 0:
		return Pot{}, fmt.Errorf("%w: the target cannot be negative", ErrPot)
	}
	var p Pot
	err := s.pool.QueryRow(ctx, `INSERT INTO agent_pots (id, workspace_id, agent_id, name, kind, target_ulxc, locked_until)
		SELECT $1, $2, a.id, $4, $5, $6, $7 FROM agent_accounts a WHERE a.id = $3 AND a.workspace_id = $2
		RETURNING id, agent_id, name, kind, target_ulxc, locked_until, created_at`,
		"pot_"+uuid.NewString(), workspaceID, agentID, name, kind, target, lockedUntil).
		Scan(&p.ID, &p.AgentID, &p.Name, &p.Kind, &p.TargetULXC, &p.LockedUntil, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pot{}, ErrAgentNotFound
	}
	if err != nil && strings.Contains(err.Error(), "agent_pots_agent_id_name_key") {
		return Pot{}, fmt.Errorf("%w: the agent already has a pot called %q", ErrPot, name)
	}
	return p, err
}

// LockPot locks a pot until a date, or unlocks it (nil). A lock still in force can only be made longer: it is
// what keeps the credits aside, so nobody can lift it early.
func (s *DualTokenStore) LockPot(ctx context.Context, workspaceID, agentID, potID string, until *time.Time) (Pot, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Pot{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var current *time.Time
	var inForce bool
	err = tx.QueryRow(ctx, `SELECT locked_until, COALESCE(locked_until > now(), false) FROM agent_pots
		WHERE id = $3 AND agent_id = $2 AND workspace_id = $1 FOR UPDATE`, workspaceID, agentID, potID).Scan(&current, &inForce)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pot{}, ErrPotNotFound
	}
	if err != nil {
		return Pot{}, err
	}
	if inForce && (until == nil || until.Before(*current)) {
		return Pot{}, fmt.Errorf("%w until %s: a lock in force can only be made longer", ErrPotLocked, current.UTC().Format(time.RFC3339))
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_pots SET locked_until = $2 WHERE id = $1`, potID, until); err != nil {
		return Pot{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Pot{}, err
	}
	return s.pot(ctx, workspaceID, agentID, potID)
}

// MoveToPot moves amount of the agent's main balance into its pot.
func (s *DualTokenStore) MoveToPot(ctx context.Context, workspaceID, agentID, potID string, amount int64) (Pot, error) {
	return s.movePot(ctx, workspaceID, agentID, potID, amount, true)
}

// MoveFromPot moves amount of the pot back to the agent's main balance, unless the pot is locked.
func (s *DualTokenStore) MoveFromPot(ctx context.Context, workspaceID, agentID, potID string, amount int64) (Pot, error) {
	return s.movePot(ctx, workspaceID, agentID, potID, amount, false)
}

func (s *DualTokenStore) movePot(ctx context.Context, workspaceID, agentID, potID string, amount int64, in bool) (Pot, error) {
	if amount <= 0 {
		return Pot{}, errors.New("economy: the amount must be positive")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Pot{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, workspaceID, agentID); err != nil {
		return Pot{}, err
	}
	var locked bool
	var until *time.Time
	err = tx.QueryRow(ctx, `SELECT locked_until, COALESCE(locked_until > now(), false) FROM agent_pots
		WHERE id = $1 AND agent_id = $2 AND workspace_id = $3`, potID, agentID, workspaceID).Scan(&until, &locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pot{}, ErrPotNotFound
	}
	if err != nil {
		return Pot{}, err
	}
	from, to, kind := agentAccount(agentID), potAccount(potID), "pot_in"
	if !in {
		kind = "pot_out"
	}
	// B17.33: as for a fund (B17.26), the Idempotency-Key is the entry's ref; under the agent's lock, a key
	// already posted to this pot is a retry of a move that landed, answered with the pot and moved nothing.
	// Checked first: a lock set since the move landed must not turn its retry into a refusal.
	ref := potID
	if key, _ := ctx.Value(moveKeyKey{}).(string); key != "" {
		ref = key
		var landed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_postings
		  WHERE workspace_id = $1 AND account = $2 AND kind = $3 AND ref = $4)`,
			workspaceID, potAccount(potID), kind, ref).Scan(&landed); err != nil {
			return Pot{}, fmt.Errorf("economy: earlier %s: %w", kind, err)
		}
		if landed {
			_ = tx.Rollback(ctx)
			return s.pot(ctx, workspaceID, agentID, potID)
		}
	}
	if !in {
		if locked {
			return Pot{}, fmt.Errorf("%w until %s", ErrPotLocked, until.UTC().Format(time.RFC3339))
		}
		from, to = potAccount(potID), agentAccount(agentID)
	}
	have, err := accountBalance(ctx, tx, workspaceID, from)
	if err != nil {
		return Pot{}, err
	}
	if have < amount {
		if in {
			return Pot{}, agentHolds(ctx, tx, ErrAgentFunds, agentID, have, 0)
		}
		return Pot{}, fmt.Errorf("%w: the pot holds %s", ErrAgentFunds, LXCMoney(have))
	}
	if err := postEntry(ctx, tx, workspaceID, kind, ref, leg{from, -amount}, leg{to, amount}); err != nil {
		return Pot{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Pot{}, err
	}
	return s.pot(ctx, workspaceID, agentID, potID)
}

func (s *DualTokenStore) pot(ctx context.Context, workspaceID, agentID, potID string) (Pot, error) {
	pots, err := s.pots(ctx, workspaceID, agentID, potID, time.Now())
	if err != nil {
		return Pot{}, err
	}
	if len(pots) == 0 {
		return Pot{}, ErrPotNotFound
	}
	return pots[0], nil
}

// ListPots reads the agent's pots and what each holds.
func (s *DualTokenStore) ListPots(ctx context.Context, workspaceID, agentID string) ([]Pot, error) {
	return s.pots(ctx, workspaceID, agentID, "", time.Now())
}

// pots reads the agent's pots (one, when potID is not ""), each with its balance as it stood before at.
func (s *DualTokenStore) pots(ctx context.Context, workspaceID, agentID, potID string, at time.Time) ([]Pot, error) {
	rows, err := s.pool.Query(ctx, `SELECT p.id, p.agent_id, p.name, p.kind, p.target_ulxc, p.locked_until, p.created_at,
		COALESCE((SELECT sum(amount_ulxc) FROM agent_postings g WHERE g.workspace_id = p.workspace_id AND g.account = 'pot:' || p.id
		  AND g.created_at < $4), 0)::bigint
		FROM agent_pots p WHERE p.workspace_id = $1 AND p.agent_id = $2 AND ($3 = '' OR p.id = $3) ORDER BY p.created_at, p.id`,
		workspaceID, agentID, potID, at)
	if err != nil {
		return nil, fmt.Errorf("economy: pots: %w", err)
	}
	defer rows.Close()
	out := []Pot{}
	for rows.Next() {
		var p Pot
		if err := rows.Scan(&p.ID, &p.AgentID, &p.Name, &p.Kind, &p.TargetULXC, &p.LockedUntil, &p.CreatedAt, &p.BalanceULXC); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
