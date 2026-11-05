package economy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/money"
)

// agent_wallets.go — B46.4: EVERY AGENT HAS ITS OWN WALLET, ON THE MONEY LAYER.
//
// A new agent gets a USDC wallet on Base through the money layer's Wallets (B46.3) — the sandbox until a partner is
// switched on for it and stablecoins (RED) is cleared. The ledger keeps the wallet as the agent's USDC account under
// the partner's reference for it, mirrored by a partner account, so money the partner reports arriving at the
// wallet's address (POST /v1/money/webhooks/{partner}) posts into it through ReceivePayment, once. Its balance is the
// ledger's — what the owner's screens and the agent's own agent_balance tool both read — and its receive details are
// the partner's. An agent made before wallets has one opened the first time it is read.
//
// Before money leaves a wallet the money layer asks MoneyLimits: the agent's own rules judge it, as they judge every
// payment the agent makes, so its limits refuse it and its approval amount files an approval for its owner.

// ErrAgentWalletsOff: the money layer is not set, so no agent has a wallet.
var ErrAgentWalletsOff = errors.New("economy: agent wallets are not set up")

// AgentWallet is an agent's wallet: what it holds, and how to pay into it.
type AgentWallet struct {
	AccountID    string        `json:"account_id"`
	AgentID      string        `json:"agent_id"`
	Asset        string        `json:"asset"`
	Partner      string        `json:"partner"`
	BalanceMinor int64         `json:"balance_minor"`
	TestMinor    int64         `json:"test_minor"`
	LiveMinor    int64         `json:"live_minor"`
	Receive      money.Details `json:"receive"` // empty while the sandbox serves the wallet: its address is on no chain
	// PaymentReference is quoted with Receive when they are the owner's details, not the wallet's own (B48.1).
	PaymentReference string `json:"payment_reference,omitempty"`
	Mode             string `json:"mode"` // TEST or LIVE
}

// SetMoneyLayer opens every new agent's wallet on r, and reads them through it (B46.4).
func (s *DualTokenStore) SetMoneyLayer(r *money.Registry) { s.moneyLayer = r }

func agentWalletRoute(workspaceID, agentID, asset string) money.Route {
	c := CapabilityStablecoins
	if asset != money.USDC {
		c = CapabilityCurrencyAccounts
	}
	return money.Route{Workspace: workspaceID, Agent: agentID, Capability: c}
}

// agentWalletKey is the one idempotency key an agent's wallet in asset is created under, so asking again opens no
// second one; a wallet opened again after its kind was removed (B48.1) is the reopened'th, under a key of its own.
func agentWalletKey(agentID, asset string, reopened int) string {
	k := "agent_wallet:" + agentID + ":" + asset
	if reopened > 0 {
		k += ":" + strconv.Itoa(reopened)
	}
	return k
}

// openNewAgentWallet opens a just-created agent's wallets. A failure leaves the agent without one until it is read.
func (s *DualTokenStore) openNewAgentWallet(ctx context.Context, workspaceID, agentID, kind, fiat string) {
	if s.moneyLayer == nil {
		return
	}
	for _, asset := range walletKindAssets(kind, fiat) {
		if err := s.openAgentWallet(ctx, workspaceID, agentID, asset); err != nil {
			slog.Warn("economy: a new agent's wallet was not opened; it opens when it is first read",
				slog.String("agent_id", agentID), slog.String("asset", asset), slog.String("err", err.Error()))
		}
	}
}

// closedAgentWallets is how many of agentID's wallets in asset were closed: the reopened count of its next one.
func (s *DualTokenStore) closedAgentWallets(ctx context.Context, agentID, asset string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM money_accounts WHERE agent_id = $1 AND currency = $2 AND purpose = 'agent'
		AND status = 'closed'`, agentID, asset).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("economy: open agent wallet: %w", err)
	}
	return n, nil
}

// openAgentWallet creates agentID's wallet in asset at the partner serving it and keeps it in the ledger, unless it has
// one — an agent's account in that currency (B30.13) is its wallet too.
func (s *DualTokenStore) openAgentWallet(ctx context.Context, workspaceID, agentID, asset string) error {
	rt := agentWalletRoute(workspaceID, agentID, asset)
	partner, err := s.moneyLayer.Partner(ctx, money.CapWallets, rt)
	if err != nil {
		return err
	}
	wallets, err := s.moneyLayer.Wallets(ctx, rt)
	if err != nil {
		return err
	}
	reopened, err := s.closedAgentWallets(ctx, agentID, asset)
	if err != nil {
		return err
	}
	wallet, err := wallets.Create(ctx, money.CreateWallet{Key: agentWalletKey(agentID, asset, reopened), AgentID: agentID, Asset: asset})
	if errors.Is(err, money.ErrInvalid) && asset != money.USDC {
		// B48.1: a partner that gives an agent no account of its own: the agent's sub-account of its owner's, paid
		// into at the owner's details with the agent's payment reference.
		if _, err := s.ownerWallet(ctx, workspaceID, asset); err != nil {
			return err
		}
		if _, err := s.openAgentCurrencyAccount(ctx, workspaceID, agentID, asset); err != nil && !errors.Is(err, ErrCurrencyAccountExists) {
			return err
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("economy: open agent wallet: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: open agent wallet: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, workspaceID, agentID); err != nil {
		return err
	}
	var name string
	var has bool
	if err := tx.QueryRow(ctx, `SELECT name, EXISTS (SELECT 1 FROM money_accounts WHERE agent_id = $1 AND currency = $2
		AND purpose = 'agent' AND status <> 'closed') FROM agent_accounts WHERE id = $1`, agentID, asset).Scan(&name, &has); err != nil {
		return fmt.Errorf("economy: open agent wallet: %w", err)
	}
	if has {
		return nil
	}
	insert := `INSERT INTO money_accounts (id, workspace_id, agent_id, currency, purpose, name, partner_account_ref)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7)`
	if _, err := tx.Exec(ctx, insert, "macc_"+uuid.NewString(), workspaceID, agentID, asset, MoneyAgent, name, wallet.Ref); err != nil {
		return fmt.Errorf("economy: open agent wallet: %w", err)
	}
	// shortcut: reconciliation reads statements from the account partner, which holds no USDC, so money through this
	// mirror shows as missing there; upgrade when the stablecoin adapter (B46.5) gives statements.
	if _, err := tx.Exec(ctx, insert, "macc_"+uuid.NewString(), workspaceID, "", asset, MoneyPartner, partner, wallet.Ref); err != nil {
		return fmt.Errorf("economy: open agent wallet: %w", err)
	}
	return tx.Commit(ctx)
}

// AgentWallet is agentID's wallet in workspaceID — its USDC wallet unless it is a fiat agent (B48.1), then its fiat
// one — opened now if it has none yet.
func (s *DualTokenStore) AgentWallet(ctx context.Context, workspaceID, agentID string) (AgentWallet, error) {
	if s.moneyLayer == nil {
		return AgentWallet{}, ErrAgentWalletsOff
	}
	kind, fiat, err := s.agentWalletKind(ctx, workspaceID, agentID)
	if err != nil {
		return AgentWallet{}, err
	}
	assets := walletKindAssets(kind, fiat)
	return s.agentWalletIn(ctx, workspaceID, agentID, assets[len(assets)-1])
}

// agentWalletIn is agentID's wallet in asset, opened now if it has none yet.
func (s *DualTokenStore) agentWalletIn(ctx context.Context, workspaceID, agentID, asset string) (AgentWallet, error) {
	w, ref, sub, err := s.readAgentWallet(ctx, workspaceID, agentID, asset)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := s.openAgentWallet(ctx, workspaceID, agentID, asset); err != nil {
			return AgentWallet{}, err
		}
		w, ref, sub, err = s.readAgentWallet(ctx, workspaceID, agentID, asset)
	}
	if err != nil {
		return AgentWallet{}, err
	}
	if sub {
		return s.subAccountWallet(ctx, workspaceID, w)
	}
	if w.Partner == money.SandboxName {
		// The sandbox's address is on no chain: the wallet shows no details rather than made-up ones (B49.1).
		w.Mode = DetailsTest
		return w, nil
	}
	wallets, err := s.moneyLayer.Wallets(ctx, agentWalletRoute(workspaceID, agentID, asset))
	if err != nil {
		return AgentWallet{}, err
	}
	if w.Receive, err = wallets.Receive(ctx, ref); err != nil {
		return AgentWallet{}, fmt.Errorf("economy: agent wallet's details: %w", err)
	}
	w.Mode = DetailsLive
	return w, nil
}

// readAgentWallet is agentID's open wallet in asset as the ledger keeps it, the partner's reference for it, and whether
// it is a sub-account of its owner's (B30.13); pgx.ErrNoRows when it has none.
func (s *DualTokenStore) readAgentWallet(ctx context.Context, workspaceID, agentID, asset string) (AgentWallet, string, bool, error) {
	w := AgentWallet{AgentID: agentID, Asset: asset}
	var ref string
	var sub bool
	err := s.pool.QueryRow(ctx, `SELECT a.id, a.partner_account_ref, a.parent_account_id IS NOT NULL, COALESCE(p.name, ''),
		COALESCE(sum(b.balance_minor) FILTER (WHERE b.funding = 'test'), 0)::bigint,
		COALESCE(sum(b.balance_minor) FILTER (WHERE b.funding = 'live'), 0)::bigint
		FROM money_accounts a
		LEFT JOIN money_accounts p ON p.workspace_id = a.workspace_id AND p.partner_account_ref = a.partner_account_ref AND p.purpose = 'partner'
		LEFT JOIN money_account_balances b ON b.account_id = a.id
		WHERE a.workspace_id = $1 AND a.agent_id = $2 AND a.currency = $3 AND a.purpose = 'agent' AND a.status = 'open'
		GROUP BY a.id, p.name`, workspaceID, agentID, asset).Scan(&w.AccountID, &ref, &sub, &w.Partner, &w.TestMinor, &w.LiveMinor)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		err = fmt.Errorf("economy: agent wallet: %w", err)
	}
	w.BalanceMinor = w.TestMinor + w.LiveMinor
	return w, ref, sub, err
}

// MoneyLimits is the owner's limits and one-tap approval the money layer asks before money leaves (money.Limits).
func (s *DualTokenStore) MoneyLimits() money.Limits { return moneyLimits{s} }

type moneyLimits struct{ s *DualTokenStore }

// Allow judges money leaving an agent's wallet by the agent's rules, at what it is worth in LXC: a limit refuses it
// (money.ErrLimit), and above the agent's approval amount it files an approval for the owner and moves nothing
// (money.ErrApprovalNeeded) until the owner approves it and the same call is made again. A payment out PayOutside
// already judged and holds goes (B46.8), as does a card of an approved batch whose cap is held (B46.10) and a reusable card the owner issued (B53.2), and so do
// earnings PayEarnings holds (B46.11). Other money out that is not an agent's is refused.
func (l moneyLimits) Allow(ctx context.Context, op money.Op) error {
	held, err := l.s.heldOutsidePayment(ctx, op)
	if err != nil {
		return fmt.Errorf("%w: %v", money.ErrLimit, err)
	}
	if held { // B46.8: judged by the agent's limits and the owner's approval, and its money held, when it was made
		return nil
	}
	if held, err = l.s.heldMoneyCard(ctx, op); err != nil {
		return fmt.Errorf("%w: %v", money.ErrLimit, err)
	}
	if held { // B46.10: a card of a batch the owner approved, its cap held
		return nil
	}
	if held, err = l.s.issuingReusableCard(ctx, op); err != nil {
		return fmt.Errorf("%w: %v", money.ErrLimit, err)
	}
	if held { // B53.2: a reusable card the owner or an admin issued; each charge on it is decided by its controls
		return nil
	}
	if held, err = l.s.heldEarningsPayout(ctx, op); err != nil {
		return fmt.Errorf("%w: %v", money.ErrLimit, err)
	}
	if held { // B46.11: earnings owed under a sale, a Room's terms or a royalty, their money held when they were paid out
		return nil
	}
	if op.Agent == "" {
		return fmt.Errorf("%w: only an agent's wallet moves money through the money layer yet", money.ErrLimit)
	}
	value, err := l.s.moneyValue(ctx, op)
	if err != nil {
		return fmt.Errorf("%w: %v", money.ErrLimit, err)
	}
	fp := sha256.Sum256([]byte("money\x00" + op.Agent + "\x00" + string(op.Capability) + "\x00" + op.Method + "\x00" + op.Key + "\x00" +
		op.Ref + "\x00" + op.Payee + "\x00" + strconv.FormatInt(op.Amount.Minor, 10) + "\x00" + op.Amount.Asset))
	ctx = WithAgentRequest(ctx, AgentRequest{Payment: true, Fingerprint: hex.EncodeToString(fp[:]),
		Payee: Payee{Kind: "outside", ID: op.Payee, Name: op.Payee}, Memo: minorString(op.Amount.Minor, op.Amount.Asset) + " to " + op.Payee})
	tx, err := l.s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: money limits: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, op.Workspace, op.Agent); err != nil {
		return fmt.Errorf("%w: %v", money.ErrLimit, err)
	}
	if err := enforceAgentRules(ctx, tx, op.Workspace, op.Agent, value, "money:"+op.Key); err != nil {
		err = l.s.refusedMovement(ctx, tx, err)
		if errors.Is(err, ErrApprovalRequired) {
			return fmt.Errorf("%w: %v", money.ErrApprovalNeeded, err)
		}
		return fmt.Errorf("%w: %v", money.ErrLimit, err)
	}
	return tx.Commit(ctx)
}

// moneyValue is what op's amount is worth in µLXC, as the agent's rules count it: USDC and dollars at the peg, pounds
// and euros at the conversion partner's rate for dollars.
func (s *DualTokenStore) moneyValue(ctx context.Context, op money.Op) (int64, error) {
	if op.Amount.Asset != money.USDC {
		if s.accountPartners == nil && op.Amount.Asset != CurrencyUSD {
			return 0, errors.New("no conversion partner is set to value it")
		}
		return s.paymentValue(ctx, OutsidePayment{WorkspaceID: op.Workspace, AmountMinor: op.Amount.Minor, Currency: op.Amount.Asset,
			IdempotencyKey: "money:" + op.Key})
	}
	if op.Amount.Minor > math.MaxInt64/ULXCPerUSDMicro {
		return 0, fmt.Errorf("%s is more than one payment can be", minorString(op.Amount.Minor, op.Amount.Asset))
	}
	return op.Amount.Minor * ULXCPerUSDMicro, nil
}
