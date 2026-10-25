package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/plans"
)

// agent_accounts.go — B19.1: AGENT ACCOUNTS, EVERY AGENT WITH ITS OWN BALANCE, ON A DOUBLE-ENTRY LEDGER.
//
// It EXTENDS agent_subbudget.go rather than beside it. An agent key already spends through
// SpendLXCForAgent and the reservation (hold → settle | release), which debit the workspace's
// lxc_balances and move the key's spent_lxc. A key attached to an agent account additionally posts
// every one of those movements against its agent in the SAME transaction (agentMovement), and is bound
// by the agent's balance instead of the per-key ceiling. Funding and withdrawing post between the
// workspace and the agent. A balance is the sum of its account's postings (migration 0140), kept as a
// running total in agent_account_balances by a trigger in the posting's own transaction (0185, B28.297),
// so reading one is a single row; every entry sums to zero, enforced at commit; postings are append-only.

// ErrAgentNotFound: no such agent in this workspace.
var ErrAgentNotFound = errors.New("economy: no such agent in this workspace")

// ErrAgentOwnerless: the agent has no owner, so it cannot hold a balance until a person claims it (B19.11).
var ErrAgentOwnerless = errors.New("economy: this agent has no owner — a person of its workspace must claim it before it can hold a balance")

// ErrAgentFunds: the workspace's unallocated LXC (its balance less what its agents hold), or for a
// withdrawal the agent's balance, does not cover the amount.
var ErrAgentFunds = errors.New("economy: not enough funds for this movement")

// Agent is one agent account, its balance derived from its postings.
type Agent struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	BalanceULXC int64     `json:"balance_ulxc"`
	SpentULXC   int64     `json:"spent_ulxc"`
	Keys        []string  `json:"keys"`
	CreatedAt   time.Time `json:"created_at"`
	// PausedAt is set while the agent is paused (B19.6): its every movement is refused until it is resumed.
	PausedAt     *time.Time `json:"paused_at,omitempty"`
	PausedReason string     `json:"paused_reason,omitempty"`
	// B19.11: the person who owns the agent, and whether that owner is verified — the badge. Lens knows a
	// person only as their workspace, so the badge is the workspace's verification: a completed card
	// purchase, or Talyvor's vouch (internal/earnverify). An agent with no owner is never verified.
	OwnerUserID string `json:"owner_user_id"`
	Verified    bool   `json:"verified"`
	// Handle is the agent's address besides its id, once its owner picks one (B22.3).
	Handle string `json:"handle,omitempty"`
	// PotsULXC is what the agent keeps aside in its pots (B22.7): its, but not spendable until moved back.
	PotsULXC int64 `json:"pots_ulxc"`
	// B28.298: what the agent is for, in its owner's words, and when it was archived — swept to zero, its
	// keys revoked, and refused every movement of its own from then on (agent_lifecycle.go).
	Description string     `json:"description"`
	ArchivedAt  *time.Time `json:"archived_at,omitempty"`
	// Kind is agent, or room for a room's wallet (B32.32): its owner funds it and sets its rules as any agent's.
	Kind string `json:"kind"`
}

// AgentKindRoom is a room's wallet (B32.32). It does not count toward the agents a plan allows.
const AgentKindRoom = "room"

// AgentBook reconciles a workspace with its agents: WorkspaceBalanceULXC (lxc_balances) =
// UnallocatedULXC + AllocatedULXC, AllocatedULXC = Σ agent balances and pots (B22.7), and SpentULXC is the
// 'spend' account — what the agents spent.
type AgentBook struct {
	WorkspaceBalanceULXC int64   `json:"workspace_balance_ulxc"`
	AllocatedULXC        int64   `json:"allocated_ulxc"`
	UnallocatedULXC      int64   `json:"unallocated_ulxc"`
	SpentULXC            int64   `json:"spent_ulxc"`
	Agents               []Agent `json:"agents"`
	// AllPausedAt is set while every agent in the workspace is paused (B19.7).
	AllPausedAt     *time.Time `json:"all_paused_at,omitempty"`
	AllPausedReason string     `json:"all_paused_reason,omitempty"`
}

func agentAccount(id string) string { return "agent:" + id }

type leg struct {
	account string
	amount  int64
}

type postingMemoKey struct{}

// WithPostingMemo carries what the postings a movement writes are for, in words (B32.32): a charge on a room names the
// room and the member who spent, so the room wallet's statement says whose each spend was.
func WithPostingMemo(ctx context.Context, memo string) context.Context {
	return context.WithValue(ctx, postingMemoKey{}, memo)
}

// insertPosting writes one posting: cols and args without the memo, which is added when ctx carries one, so a posting
// without one is written exactly as it was before postings had memos.
func insertPosting(ctx context.Context, tx pgx.Tx, cols string, args ...any) error {
	if memo, _ := ctx.Value(postingMemoKey{}).(string); memo != "" {
		cols, args = cols+", memo", append(args, memo)
	}
	params := make([]string, len(args))
	for i := range args {
		params[i] = fmt.Sprintf("$%d", i+1)
	}
	_, err := tx.Exec(ctx, `INSERT INTO agent_postings (`+cols+`) VALUES (`+strings.Join(params, ", ")+`)`, args...)
	return err
}

// postEntry writes one balanced entry. The deferred trigger refuses the commit if it does not balance.
func postEntry(ctx context.Context, tx pgx.Tx, workspaceID, kind, ref string, legs ...leg) error {
	entry := uuid.New()
	for _, l := range legs {
		if err := insertPosting(ctx, tx, `entry_id, workspace_id, account, amount_ulxc, kind, ref`,
			entry, workspaceID, l.account, l.amount, kind, ref); err != nil {
			return fmt.Errorf("economy: post %s: %w", kind, err)
		}
	}
	return nil
}

// postModelEntry is postEntry for a question to a model: its postings name the model as its caps know it
// (modelCapKey), which is what an agent's per-model daily caps count (B28.301). An empty model names none.
func postModelEntry(ctx context.Context, tx pgx.Tx, workspaceID, kind, ref, model string, legs ...leg) error {
	if model = modelCapKey(model); model == "" {
		return postEntry(ctx, tx, workspaceID, kind, ref, legs...)
	}
	entry := uuid.New()
	for _, l := range legs {
		if err := insertPosting(ctx, tx, `entry_id, workspace_id, account, amount_ulxc, kind, ref, model`,
			entry, workspaceID, l.account, l.amount, kind, ref, model); err != nil {
			return fmt.Errorf("economy: post %s: %w", kind, err)
		}
	}
	return nil
}

// balanceSQL reads an account's stored running balance (B28.297): one row, however many postings it has.
// Every account but the shared 'workspace', 'spend' and 'cashed_out' sides is kept (0185).
const balanceSQL = `SELECT COALESCE((SELECT balance_ulxc FROM agent_account_balances WHERE workspace_id = $1 AND account = $2), 0)::bigint`

func accountBalance(ctx context.Context, tx pgx.Tx, workspaceID, account string) (int64, error) {
	var bal int64
	err := tx.QueryRow(ctx, balanceSQL, workspaceID, account).Scan(&bal)
	return bal, err
}

// allocatedSQL is what a workspace's agents hold: Σ their balances and their pots (B22.7), one stored row each.
// Read directly, not as −(workspace side) − spend: a transfer between workspaces (B22.3) is an entry whose two
// postings are in different workspaces.
const allocatedSQL = `SELECT COALESCE(sum(balance_ulxc), 0)::bigint FROM agent_account_balances
  WHERE workspace_id = $1 AND (account LIKE 'agent:%' OR account LIKE 'pot:%')`

// requireUnallocated refuses a debit of the workspace's OWN spending — anything not made with an agent's
// key — that would reach into the LXC its agents hold (B19.13). bal is the lxc_balances balance the
// caller has locked, so a concurrent funding waits on the same row.
func requireUnallocated(ctx context.Context, tx pgx.Tx, workspaceID string, bal, amount int64) error {
	var allocated int64
	if err := tx.QueryRow(ctx, allocatedSQL, workspaceID).Scan(&allocated); err != nil {
		return fmt.Errorf("economy: allocated LXC: %w", err)
	}
	if bal-allocated < amount {
		return fmt.Errorf("%w: %d µLXC is not held by the workspace's agents", ErrInsufficientLXC, bal-allocated)
	}
	return nil
}

// GetUnallocatedLXC is what the workspace can spend itself: its LXC balance less what its agents hold.
// The pre-serve gates read it for any request not made with an agent's key.
func (s *DualTokenStore) GetUnallocatedLXC(ctx context.Context, workspaceID string) (int64, error) {
	if s.pool == nil {
		return 0, nil
	}
	var unallocated int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT balance FROM lxc_balances WHERE workspace_id = $1), 0)::bigint - (`+
		allocatedSQL+`)`, workspaceID).Scan(&unallocated)
	if err != nil {
		return 0, fmt.Errorf("economy: unallocated LXC: %w", err)
	}
	return unallocated, nil
}

// OwnerVerifier says whether a workspace's people are verified (internal/earnverify.Verifier).
type OwnerVerifier interface {
	MayEarn(ctx context.Context, tx pgx.Tx, workspaceID string) (bool, error)
}

// SetOwnerVerifier sets what the verified-agent badge follows (B19.11). A test workspace's people are verified
// for its money, which is test money the wall keeps among test workspaces (B25.3).
func (s *DualTokenStore) SetOwnerVerifier(v OwnerVerifier) {
	if v != nil {
		v = testOwnersVerified{v}
	}
	s.ownerVerifier = v
}

// testOwnersVerified verifies a test workspace's people, and asks the verifier it wraps about everyone else's.
type testOwnersVerified struct{ OwnerVerifier }

func (v testOwnersVerified) MayEarn(ctx context.Context, tx pgx.Tx, workspaceID string) (bool, error) {
	if test, err := testWorkspace(ctx, tx, workspaceID); err != nil || test {
		return test, err
	}
	return v.OwnerVerifier.MayEarn(ctx, tx, workspaceID)
}

// requireOwner refuses to give an agent with no owner a balance (B19.11).
func requireOwner(ctx context.Context, tx pgx.Tx, agentID string) error {
	var owner string
	if err := tx.QueryRow(ctx, `SELECT owner_user_id FROM agent_accounts WHERE id = $1`, agentID).Scan(&owner); err != nil {
		return fmt.Errorf("economy: agent owner: %w", err)
	}
	if owner == "" {
		return ErrAgentOwnerless
	}
	return nil
}

// ClaimAgent makes userID the agent's owner.
func (s *DualTokenStore) ClaimAgent(ctx context.Context, workspaceID, agentID, userID string) error {
	if userID == "" {
		return ErrAgentOwnerless
	}
	tag, err := s.pool.Exec(ctx, `UPDATE agent_accounts SET owner_user_id = $3 WHERE id = $1 AND workspace_id = $2`, agentID, workspaceID, userID)
	if err != nil {
		return fmt.Errorf("economy: claim agent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAgentNotFound
	}
	return nil
}

// lockAgent locks the agent's row — every movement of its balance serialises on it — and checks it
// belongs to workspaceID.
func lockAgent(ctx context.Context, tx pgx.Tx, workspaceID, agentID string) error {
	var ws string
	err := tx.QueryRow(ctx, `SELECT workspace_id FROM agent_accounts WHERE id = $1 FOR UPDATE`, agentID).Scan(&ws)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && ws != workspaceID) {
		return ErrAgentNotFound
	}
	return err
}

// CreateAgent creates an agent account in workspaceID, with no balance, owned by ownerUserID — the person
// creating it (B19.11) — or refuses it with a *plans.Refusal past the agents its plan allows (B32.12).
func (s *DualTokenStore) CreateAgent(ctx context.Context, workspaceID, name, ownerUserID string) (Agent, error) {
	if workspaceID == "" || name == "" {
		return Agent{}, errors.New("economy: an agent needs a workspace and a name")
	}
	if ownerUserID == "" {
		return Agent{}, ErrAgentOwnerless
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Agent{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockWorkspaceAgents(ctx, tx, workspaceID); err != nil {
		return Agent{}, err
	}
	a, err := createAgentTx(ctx, tx, workspaceID, name, ownerUserID)
	if err != nil {
		return Agent{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Agent{}, err
	}
	s.openNewAgentWallet(ctx, workspaceID, a.ID) // B46.4
	return a, nil
}

// lockWorkspaceAgents takes workspaceID's agents lock for the rest of tx: what counts or creates its agents
// serialises on it.
func lockWorkspaceAgents(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('agents:' || $1, 0))`, workspaceID); err != nil {
		return fmt.Errorf("economy: lock the workspace's agents: %w", err)
	}
	return nil
}

// createAgentTx is CreateAgent in tx, which holds the workspace's agents lock.
func createAgentTx(ctx context.Context, tx pgx.Tx, workspaceID, name, ownerUserID string) (Agent, error) {
	// B32.12: no more agents than the workspace's plan allows (LENS_PLAN_GATES); an archived agent is retired
	// for good and does not count, nor does a room's wallet (B32.32). Counted under the workspace's lock, so two
	// creates cannot both take the last place.
	plan, err := plans.Of(ctx, tx, workspaceID)
	if err != nil {
		return Agent{}, err
	}
	var n int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_accounts WHERE workspace_id = $1 AND archived_at IS NULL AND kind = 'agent'`,
		workspaceID).Scan(&n); err != nil {
		return Agent{}, fmt.Errorf("economy: count agents: %w", err)
	}
	if !plans.Within(n+1, plan.Agents) {
		return Agent{}, plan.RefuseCount(plans.Current(), "agents", plan.Agents)
	}
	a := Agent{ID: "agt_" + uuid.NewString(), Name: name, Keys: []string{}, OwnerUserID: ownerUserID, Kind: "agent"}
	err = tx.QueryRow(ctx, `INSERT INTO agent_accounts (id, workspace_id, name, owner_user_id) VALUES ($1, $2, $3, $4) RETURNING created_at`,
		a.ID, workspaceID, name, ownerUserID).Scan(&a.CreatedAt)
	if err != nil {
		return Agent{}, fmt.Errorf("economy: create agent: %w", err)
	}
	return a, nil
}

// CreateRoomAgentTx creates a room's wallet in tx, the transaction that opens the room (B32.32): an agent account of
// kind room in workspaceID — the room owner's — named name, owned by ownerUserID, with scopedKeyID, a proxy-scoped key
// of that workspace's, as its one key. It does not count toward the agents the workspace's plan allows. ownerUserID is
// "" when the room was opened by a credential that names no person: the room is not refused for that, and its wallet,
// like any agent without an owner, holds no balance until a person of the workspace claims it (B19.11).
func CreateRoomAgentTx(ctx context.Context, tx pgx.Tx, workspaceID, name, ownerUserID, scopedKeyID string) (Agent, error) {
	if workspaceID == "" || name == "" || scopedKeyID == "" {
		return Agent{}, errors.New("economy: a room's wallet needs a workspace, a name and a key")
	}
	a := Agent{ID: "agt_" + uuid.NewString(), Name: name, Keys: []string{scopedKeyID}, OwnerUserID: ownerUserID, Kind: AgentKindRoom}
	if err := tx.QueryRow(ctx, `INSERT INTO agent_accounts (id, workspace_id, name, owner_user_id, kind) VALUES ($1, $2, $3, $4, 'room')
		RETURNING created_at`, a.ID, workspaceID, name, ownerUserID).Scan(&a.CreatedAt); err != nil {
		return Agent{}, fmt.Errorf("economy: create a room's wallet: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_account_keys (scoped_key_id, agent_id) VALUES ($1, $2)`, scopedKeyID, a.ID); err != nil {
		return Agent{}, fmt.Errorf("economy: attach a room wallet's key: %w", err)
	}
	return a, nil
}

// ErrRoomWalletKey: a room's wallet spends only through its room, with the one key Lens holds for it (B32.32). Another
// key would make it an agent the plan does not count.
var ErrRoomWalletKey = errors.New("economy: a room's wallet spends only through its room, and takes no other key")

// AttachAgentKey makes scopedKeyID one of agentID's keys: from then on its spending is the agent's. A room's wallet
// takes no other key than the one it was opened with.
func (s *DualTokenStore) AttachAgentKey(ctx context.Context, workspaceID, agentID, scopedKeyID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, workspaceID, agentID); err != nil {
		return err
	}
	var kind string
	if err := tx.QueryRow(ctx, `SELECT kind FROM agent_accounts WHERE id = $1`, agentID).Scan(&kind); err != nil {
		return fmt.Errorf("economy: attach key: %w", err)
	}
	if kind == AgentKindRoom {
		return ErrRoomWalletKey
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_account_keys (scoped_key_id, agent_id) VALUES ($1, $2)`, scopedKeyID, agentID); err != nil {
		return fmt.Errorf("economy: attach key: %w", err)
	}
	return tx.Commit(ctx)
}

// FundAgent moves amount of the workspace's unallocated LXC to the agent. Returns the agent's balance.
func (s *DualTokenStore) FundAgent(ctx context.Context, workspaceID, agentID string, amount int64) (int64, error) {
	return s.moveAgentFunds(ctx, workspaceID, agentID, amount, "fund")
}

// WithdrawAgent moves amount of the agent's balance back to the workspace. Returns the agent's balance.
func (s *DualTokenStore) WithdrawAgent(ctx context.Context, workspaceID, agentID string, amount int64) (int64, error) {
	return s.moveAgentFunds(ctx, workspaceID, agentID, amount, "withdraw")
}

type moveKeyKey struct{}

// WithMoveKey carries the Idempotency-Key of a fund or withdraw (B17.26), or of a move into or out of a pot
// (B17.33): the app sends a move again, with the same key, when the first met a restart and got no answer,
// and the move lands once.
func WithMoveKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, moveKeyKey{}, key)
}

func (s *DualTokenStore) moveAgentFunds(ctx context.Context, workspaceID, agentID string, amount int64, kind string) (int64, error) {
	if amount <= 0 {
		return 0, errors.New("economy: the amount must be positive")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Lock order everywhere: the agent, then the workspace balance (readLXCBalance is FOR UPDATE).
	if err := lockAgent(ctx, tx, workspaceID, agentID); err != nil {
		return 0, err
	}
	bal, err := accountBalance(ctx, tx, workspaceID, agentAccount(agentID))
	if err != nil {
		return 0, err
	}
	// B17.26: the key is the entry's ref; under the agent's lock, a key already posted is a retry of a
	// move that landed, answered with the balance and moved nothing.
	ref, _ := ctx.Value(moveKeyKey{}).(string)
	if ref != "" {
		var landed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_postings
		  WHERE workspace_id = $1 AND account = $2 AND kind = $3 AND ref = $4)`,
			workspaceID, agentAccount(agentID), kind, ref).Scan(&landed); err != nil {
			return 0, fmt.Errorf("economy: earlier %s: %w", kind, err)
		}
		if landed {
			return bal, nil
		}
	}
	if kind == "fund" {
		if err := requireOwner(ctx, tx, agentID); err != nil {
			return 0, err
		}
		if err := requireNotArchived(ctx, tx, agentID); err != nil {
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
		if unallocated := wsBal - allocated; unallocated < amount {
			return 0, fmt.Errorf("%w: the workspace has %d µLXC not held by its agents", ErrAgentFunds, unallocated)
		}
		err = postEntry(ctx, tx, workspaceID, kind, ref, leg{"workspace", -amount}, leg{agentAccount(agentID), amount})
		if err != nil {
			return 0, err
		}
		bal += amount
	} else {
		if bal < amount {
			return 0, fmt.Errorf("%w: the agent holds %d µLXC", ErrAgentFunds, bal)
		}
		if err := postEntry(ctx, tx, workspaceID, kind, ref, leg{agentAccount(agentID), -amount}, leg{"workspace", amount}); err != nil {
			return 0, err
		}
		bal -= amount
	}
	return bal, tx.Commit(ctx)
}

// agentMovement posts delta µLXC of an agent key's spending against its agent, inside the caller's
// spend transaction: delta > 0 (a spend or a hold) moves agent → spend and is refused, as a sub-budget
// refusal, beyond the agent's balance; delta < 0 (a settle's refund or a release) moves it back.
// isAgent is false for a key attached to no agent, which keeps its per-key ceiling and posts nothing. model is
// the model the question asked for, which the postings name (B28.301): a refund names its hold's or debit's.
func agentMovement(ctx context.Context, tx pgx.Tx, scopedKeyID string, delta int64, kind, ref, model string) (isAgent bool, err error) {
	return agentMovementFee(ctx, tx, scopedKeyID, delta, 0, 0, kind, ref, model)
}

// agentMovementFee is agentMovement for a spend that carries a platform fee (B32.11): the agent's balance, its
// credit line and its rules judge delta + fee, the whole price, and the fee is posted as its own movement.
func agentMovementFee(ctx context.Context, tx pgx.Tx, scopedKeyID string, delta, fee, bps int64, kind, ref, model string) (isAgent bool, err error) {
	var agentID, workspaceID string
	err = tx.QueryRow(ctx,
		`SELECT a.id, a.workspace_id FROM agent_account_keys k JOIN agent_accounts a ON a.id = k.agent_id
		  WHERE k.scoped_key_id = $1`, scopedKeyID).Scan(&agentID, &workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("economy: agent of key: %w", err)
	}
	if delta == 0 && fee == 0 {
		return true, nil
	}
	if err := lockAgent(ctx, tx, workspaceID, agentID); err != nil {
		return true, err
	}
	if need := delta + fee; delta > 0 {
		bal, err := accountBalance(ctx, tx, workspaceID, agentAccount(agentID))
		if err != nil {
			return true, err
		}
		if bal < need && (kind == "spend" || kind == "hold") { // B22.4: a company's credit line lends the rest
			drew, err := drawCreditLine(ctx, tx, workspaceID, agentID, need-bal, ref)
			if err != nil {
				return true, err
			}
			if drew {
				bal = need
			}
		}
		if bal < need {
			return true, fmt.Errorf("%w: agent %s holds %d µLXC, this needs %d", ErrSubBudgetExceeded, agentID, bal, need)
		}
		// B19.2: the agent's spending rules, under the same lock, before anything is held or debited.
		if err := enforceAgentRules(ctx, tx, workspaceID, agentID, need, ref); err != nil {
			return true, err
		}
	}
	if delta != 0 {
		if err := postModelEntry(ctx, tx, workspaceID, kind, ref, model, leg{agentAccount(agentID), -delta}, leg{"spend", delta}); err != nil {
			return true, err
		}
	}
	if fee != 0 {
		return true, postFeeEntry(ctx, tx, workspaceID, agentID, fee, bps, ref, model)
	}
	return true, nil
}

// AgentBook reads a workspace's agents and reconciles them with its LXC balance.
func (s *DualTokenStore) AgentBook(ctx context.Context, workspaceID string) (AgentBook, error) {
	var book AgentBook
	// One snapshot, so the workspace balance and the postings are read at the same instant.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return book, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY`); err != nil {
		return book, err
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT balance FROM lxc_balances WHERE workspace_id = $1), 0)::bigint`,
		workspaceID).Scan(&book.WorkspaceBalanceULXC); err != nil {
		return book, fmt.Errorf("economy: workspace balance: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT paused_at, reason FROM agent_workspace_pauses WHERE workspace_id = $1`,
		workspaceID).Scan(&book.AllPausedAt, &book.AllPausedReason); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return book, fmt.Errorf("economy: workspace pause: %w", err)
	}
	wsVerified := false
	if s.ownerVerifier != nil {
		if wsVerified, err = s.ownerVerifier.MayEarn(ctx, tx, workspaceID); err != nil {
			return book, fmt.Errorf("economy: owner verification: %w", err)
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT a.id, a.name, a.kind, a.description, a.archived_at, a.created_at, a.paused_at, a.paused_reason, a.owner_user_id, COALESCE(a.handle, ''),
		       COALESCE((SELECT b.balance_ulxc FROM agent_account_balances b WHERE b.workspace_id = a.workspace_id AND b.account = 'agent:' || a.id), 0)::bigint,
		       COALESCE((SELECT sum(amount_ulxc) FROM agent_postings p WHERE p.workspace_id = a.workspace_id AND p.account = 'agent:' || a.id
		                   AND p.kind IN ('spend', 'hold', 'settle', 'release', 'card', 'platform_fee')), 0)::bigint,
		       COALESCE((SELECT array_agg(k.scoped_key_id ORDER BY k.created_at) FROM agent_account_keys k WHERE k.agent_id = a.id), '{}'),
		       COALESCE((SELECT sum(b.balance_ulxc) FROM agent_account_balances b JOIN agent_pots t ON b.account = 'pot:' || t.id
		                  WHERE t.agent_id = a.id AND b.workspace_id = a.workspace_id), 0)::bigint
		  FROM agent_accounts a WHERE a.workspace_id = $1 ORDER BY a.created_at, a.id`, workspaceID)
	if err != nil {
		return book, fmt.Errorf("economy: agents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a Agent
		var spendLegs int64
		if err := rows.Scan(&a.ID, &a.Name, &a.Kind, &a.Description, &a.ArchivedAt, &a.CreatedAt, &a.PausedAt, &a.PausedReason, &a.OwnerUserID, &a.Handle, &a.BalanceULXC, &spendLegs, &a.Keys, &a.PotsULXC); err != nil {
			return book, err
		}
		a.SpentULXC = -spendLegs
		a.Verified = wsVerified && a.OwnerUserID != ""
		book.AllocatedULXC += a.BalanceULXC + a.PotsULXC
		book.SpentULXC += a.SpentULXC
		book.Agents = append(book.Agents, a)
	}
	if err := rows.Err(); err != nil {
		return book, err
	}
	book.UnallocatedULXC = book.WorkspaceBalanceULXC - book.AllocatedULXC
	return book, nil
}
