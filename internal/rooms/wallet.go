package rooms

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/plans"
	"github.com/talyvor/lens/internal/tenant"
)

// wallet.go — B32.32: THE ROOM'S BUDGET.
//
// Opening a room opens its wallet in the same transaction: an agent account of the owner's workspace, of kind room,
// named "Room: <title>", with one proxy-scoped key whose plaintext is never kept, so only Lens, acting for the room,
// ever spends with it. The owner funds it and sets its rules as any agent's, and its approvals reach the owner's; its
// monthly limit is the room's budget, at most the owner's plan's room_budget_max_usd (CheckRoomBudget, which economy
// asks every time the limit changes). It does not count toward the agents the plan allows.
//
// The room's budget is spent by its owner and, when the room's terms say members_with_spend, by each member the owner
// or an editor gave may_spend (MaySpend). Every charge on the room records the room and the member who spent: on the
// marketplace use (market.UseRequest's RoomID and ActorWorkspaceID) and in the memo of the wallet's postings
// (Spender.Context). A member never pays into the owner's wallet: that would be a payment between owners.

// Keys issues and revokes the workspace API keys a room's wallet spends with (tenant.Store).
type Keys interface {
	CreateAPIKey(ctx context.Context, workspaceID, name string, scopes []string, expiresAt *time.Time) (string, *tenant.WorkspaceAPIKey, error)
	RevokeAPIKey(ctx context.Context, keyID string) error
}

// SetKeys gives every room opened from now on its wallet, its key issued by keys. Unset, a room has no wallet.
func (s *Store) SetKeys(k Keys) { s.keys = k }

// Wallet is a room's wallet as the room's members see it: its budget — the monthly limit and what has been spent of it
// this month — its balance, its limit per request and the amount above which a spend waits for the owner's approval,
// the most the owner's plan lets the budget be, who may spend it, and whether the caller may.
type Wallet struct {
	AgentID string `json:"agent_id"`
	Name    string `json:"name"`
	economy.AgentBudget
	// BudgetMaxULXC is room_budget_max_usd for the owner's plan, in µLXC; -1 is unlimited.
	BudgetMaxULXC int64  `json:"budget_max_ulxc"`
	SpendPolicy   string `json:"spend_policy"`
	MaySpend      bool   `json:"may_spend"`
	// WhyNot says why the caller may not spend it; empty when it may.
	WhyNot string `json:"why_not,omitempty"`
}

// walletName is a room wallet's name, as its owner's agents list it.
func walletName(title string) string { return "Room: " + title }

// usdULXC is a dollar of a room's budget in µLXC.
var usdULXC = 1_000_000 * economy.ULXCPerUSDMicro

// issueWalletKey issues the proxy-scoped key a room's wallet spends with, discarding its plaintext.
func (s *Store) issueWalletKey(ctx context.Context, ws, title string) (string, error) {
	_, key, err := s.keys.CreateAPIKey(ctx, ws, walletName(title), []string{"proxy"}, nil)
	if err != nil {
		return "", fmt.Errorf("rooms: the room wallet's key: %w", err)
	}
	return key.ID, nil
}

// revokeWalletKey revokes a key issued for a wallet that was not opened. Its plaintext was never kept, so a key left
// behind when this fails spends nothing; the room's failure is the error worth answering.
func (s *Store) revokeWalletKey(ctx context.Context, keyID string) {
	_ = s.keys.RevokeAPIKey(context.WithoutCancel(ctx), keyID)
}

// openWallet opens the room's wallet in tx, which holds the room's row: a room-kind agent of ws, owned by user, with
// keyID as its one key. user is "" when the room was opened by a credential that names no person; a person of the
// workspace then claims the wallet before it holds a balance, as any agent without an owner.
func openWallet(ctx context.Context, tx pgx.Tx, roomID, ws, user, title, keyID string) error {
	a, err := economy.CreateRoomAgentTx(ctx, tx, ws, walletName(title), user, keyID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE rooms SET wallet_agent_id = $2 WHERE id = $1`, roomID, a.ID)
	return err
}

// ensureWallet opens the wallet of a room opened before rooms had one, when its owner reads it.
func (s *Store) ensureWallet(ctx context.Context, roomID string) error {
	if s.keys == nil {
		return nil
	}
	var ws, title, user string
	var wallet *string
	if err := s.pool.QueryRow(ctx, `SELECT r.owner_workspace_id, r.title, r.wallet_agent_id, COALESCE(m.user_id, '') FROM rooms r
		LEFT JOIN room_members m ON m.room_id = r.id AND m.role = 'owner' WHERE r.id = $1`, roomID).Scan(&ws, &title, &wallet, &user); err != nil {
		return fmt.Errorf("rooms: wallet: %w", err)
	}
	if wallet != nil {
		return nil
	}
	keyID, err := s.issueWalletKey(ctx, ws, title)
	if err != nil {
		return err
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var opened bool
		if err := tx.QueryRow(ctx, `SELECT wallet_agent_id IS NOT NULL FROM rooms WHERE id = $1 FOR UPDATE`, roomID).Scan(&opened); err != nil {
			return err
		}
		if opened { // another read opened it first
			return errWalletOpened
		}
		return openWallet(ctx, tx, roomID, ws, user, title, keyID)
	})
	if err != nil {
		s.revokeWalletKey(ctx, keyID)
		if errors.Is(err, errWalletOpened) {
			return nil
		}
		return fmt.Errorf("rooms: wallet: %w", err)
	}
	return nil
}

var errWalletOpened = errors.New("rooms: the room's wallet is open")

// readWallet reads the room's wallet in tx for a member whose membership is me, under the room's current terms.
func (s *Store) readWallet(ctx context.Context, tx pgx.Tx, r Room, walletID string, t Terms, me Member) (*Wallet, error) {
	w := &Wallet{AgentID: walletID, SpendPolicy: t.SpendPolicy}
	if err := tx.QueryRow(ctx, `SELECT name FROM agent_accounts WHERE id = $1`, walletID).Scan(&w.Name); err != nil {
		return nil, err
	}
	var err error
	if w.AgentBudget, err = economy.ReadAgentBudget(ctx, tx, r.OwnerWorkspaceID, walletID, time.Now()); err != nil {
		return nil, err
	}
	p, err := s.LimitsOf(ctx, tx, r.OwnerWorkspaceID)
	if err != nil {
		return nil, err
	}
	w.BudgetMaxULXC = budgetMaxULXC(p)
	w.WhyNot = whyNotSpend(r, t, me)
	if w.WhyNot == "" {
		if err := s.judgeBudget(p, w.MonthlyLimitULXC); err != nil {
			w.WhyNot = err.Error()
		}
	}
	w.MaySpend = w.WhyNot == ""
	return w, nil
}

// whyNotSpend says why me may not spend the room's budget under terms t, or "" when it may: the owner always may; a
// member may when the room's spend policy is members_with_spend and it was given may_spend.
func whyNotSpend(r Room, t Terms, me Member) string {
	switch {
	case r.Status == Closed:
		return "This room is closed, so its budget is not spent."
	case r.Status == Locked:
		return "Talyvor has locked this room: it is read-only until it is unlocked."
	case me.Role == RoleOwner:
		return ""
	case t.SpendPolicy != SpendMembersWithSpend:
		return "Only the room’s owner spends its budget."
	case !me.MaySpend:
		return "The room’s owner has not let you spend its budget. Ask the owner or an editor."
	}
	return ""
}

// budgetMaxULXC is the most p lets a room's monthly limit be, in µLXC; -1 is unlimited.
func budgetMaxULXC(p PlanLimits) int64 {
	if p.RoomBudgetMaxUSD == plans.Unlimited {
		return plans.Unlimited
	}
	return p.RoomBudgetMaxUSD * usdULXC
}

// CheckRoomBudget refuses monthlyULXC as the monthly limit of a room wallet of ws's — the room's budget — when ws's plan
// does not allow it: above room_budget_max_usd, or no monthly limit at all while the plan sets one. Economy asks it every
// time a room wallet's monthly limit changes (economy.RoomBudgets).
func (s *Store) CheckRoomBudget(ctx context.Context, q plans.Querier, ws string, monthlyULXC int64) error {
	p, err := s.LimitsOf(ctx, q, ws)
	if err != nil {
		return fmt.Errorf("rooms: limits: %w", err)
	}
	return s.judgeBudget(p, monthlyULXC)
}

// judgeBudget answers nil when p allows a room a monthly limit of monthlyULXC, and its refusal under rooms_plan_limits
// when it does not: "Free allows a room a budget of up to $100 (1,000 LXC) a month. Team allows $5,000."
func (s *Store) judgeBudget(p PlanLimits, monthlyULXC int64) error {
	ceiling := budgetMaxULXC(p)
	if ceiling == plans.Unlimited || (monthlyULXC > 0 && monthlyULXC <= ceiling) {
		return nil
	}
	e := &PlanLimitError{Setting: LimitsSetting, Plan: p.Plan, Limit: "room_budget_max_usd", Max: p.RoomBudgetMaxUSD}
	past := false
	for _, name := range plans.Order {
		if n := s.limits[name].RoomBudgetMaxUSD; past && (n == plans.Unlimited || n > p.RoomBudgetMaxUSD) {
			e.Allows = name
			break
		}
		past = past || name == p.LimitAs
	}
	upTo := fmt.Sprintf("up to $%s (%s LXC) a month", plans.Thousands(p.RoomBudgetMaxUSD), lxc(ceiling))
	if monthlyULXC <= 0 {
		e.Detail = fmt.Sprintf("This room has no budget yet. Its owner sets one in Room settings: on %s, %s.", plans.Name(p.Plan), upTo)
		return e
	}
	e.Detail = fmt.Sprintf("%s allows a room a budget of %s.", plans.Name(p.Plan), upTo)
	switch n := s.limits[e.Allows].RoomBudgetMaxUSD; {
	case e.Allows == "":
		e.Detail += " A contract with Talyvor sets more."
	case n == plans.Unlimited:
		e.Detail += fmt.Sprintf(" %s allows any budget.", plans.Name(e.Allows))
	default:
		e.Detail += fmt.Sprintf(" %s allows $%s.", plans.Name(e.Allows), plans.Thousands(n))
	}
	return e
}

// lxc writes µLXC as LXC, grouped in thousands and without trailing zeros.
func lxc(ulxc int64) string {
	s := fmt.Sprintf("%s.%06d", plans.Thousands(ulxc/1_000_000), ulxc%1_000_000)
	return strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
}

// Spender is one charge on a room: the room, its owner — the buyer of what the room buys —, the wallet it is spent
// from and the member who spends it.
type Spender struct {
	RoomID           string `json:"room_id"`
	OwnerWorkspaceID string `json:"owner_workspace_id"`
	WalletAgentID    string `json:"wallet_agent_id"`
	ActorWorkspaceID string `json:"actor_workspace_id"`
	// WalletKeyID is the wallet's key, which nobody holds: a run on the room makes its model calls with it (B32.33).
	WalletKeyID     string   `json:"-"`
	WalletKeyScopes []string `json:"-"`
}

// Memo is what the wallet's postings for the charge carry: the room and the member who spent.
func (sp Spender) Memo() string { return "room " + sp.RoomID + " · member " + sp.ActorWorkspaceID }

// Context carries the charge's memo to every posting the wallet makes for it.
func (sp Spender) Context(ctx context.Context) context.Context {
	return economy.WithPostingMemo(ctx, sp.Memo())
}

// MaySpend answers the wallet actor spends from when it charges something to the room, or refuses: the room must be
// open, actor its owner or — under members_with_spend — a member given may_spend, and the room's wallet must
// have a monthly limit the owner's plan allows. A private room answers ErrNotFound to a workspace that is not a member.
func (s *Store) MaySpend(ctx context.Context, actor, roomID string) (Spender, error) {
	sp := Spender{RoomID: roomID, ActorWorkspaceID: actor}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var wallet *string
		r, err := scanRoom(tx.QueryRow(ctx, `SELECT `+roomCols+` FROM rooms r WHERE r.id = $1`, roomID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no such room", ErrNotFound)
		}
		if err != nil {
			return err
		}
		me, ok, err := member(ctx, tx, roomID, actor)
		if err != nil {
			return err
		}
		if !ok {
			if r.Visibility == Private {
				return fmt.Errorf("%w: no such room", ErrNotFound)
			}
			return forbidden("join the room before you spend its budget")
		}
		var t Terms
		if err := tx.QueryRow(ctx, `SELECT t.spend_policy, r.wallet_agent_id FROM room_terms t JOIN rooms r ON r.id = t.room_id
			WHERE t.room_id = $1 AND t.version = r.terms_version`, roomID).Scan(&t.SpendPolicy, &wallet); err != nil {
			return err
		}
		if why := whyNotSpend(r, t, me); why != "" {
			return forbidden("%s", why)
		}
		if wallet == nil {
			return fmt.Errorf("%w: the room has no wallet yet: its owner opens the room to make one", ErrConflict)
		}
		b, err := economy.ReadAgentBudget(ctx, tx, r.OwnerWorkspaceID, *wallet, time.Now())
		if err != nil {
			return err
		}
		p, err := s.LimitsOf(ctx, tx, r.OwnerWorkspaceID)
		if err != nil {
			return err
		}
		if err := s.judgeBudget(p, b.MonthlyLimitULXC); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT a.scoped_key_id, k.scopes FROM agent_account_keys a JOIN workspace_api_keys k ON k.id::text = a.scoped_key_id
			WHERE a.agent_id = $1 AND k.workspace_id = $2 AND (k.expires_at IS NULL OR k.expires_at > now())
			ORDER BY a.created_at LIMIT 1`, *wallet, r.OwnerWorkspaceID).Scan(&sp.WalletKeyID, &sp.WalletKeyScopes)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: the room's wallet has no key to spend with", ErrConflict)
		}
		if err != nil {
			return err
		}
		sp.OwnerWorkspaceID, sp.WalletAgentID = r.OwnerWorkspaceID, *wallet
		return nil
	})
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrConflict) &&
		!errors.Is(err, ErrPlanLimit) {
		err = fmt.Errorf("rooms: spend: %w", err)
	}
	return sp, err
}
