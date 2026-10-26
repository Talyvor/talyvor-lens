package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/money"
)

// agent_wallet_kinds.go — B48.1: AN AGENT IS FIAT, CRYPTO OR BOTH, AND OWNS ONE WALLET OF EACH KIND IT IS.
//
// A fiat agent owns a wallet in pounds, euros or dollars, a crypto agent a USDC wallet, a "both" agent one of each, all
// opened through the money layer's gate (B46.3) and kept in the ledger as the agent's accounts. A fiat wallet pays in at
// details of its own where the partner gives the agent an account (the sandbox does), and otherwise at its owner's
// details with the agent's payment reference (a B30.13 sub-account). The owner chooses the kind at creation and changes
// it later: a kind added opens its wallet; a kind removed closes it, and is refused while the wallet holds money — the
// owner sweeps it out first. Every change is in the audit trail.
//
// The owner has a wallet of each asset their agents use — the workspace's account in it, where Lens knows a person only
// as their workspace — opened through the same gate when first read; it is what they fund their agents from.

// The kinds of agent wallets.
const (
	WalletKindFiat   = "fiat"
	WalletKindCrypto = "crypto"
	WalletKindBoth   = "both"
)

var (
	// ErrWalletKind: a kind that is not fiat, crypto or both, or a fiat currency that is not GBP, EUR or USD.
	ErrWalletKind = errors.New("economy: an agent's wallet kind is fiat, crypto or both, its fiat currency GBP, EUR or USD")
	// ErrWalletNotEmpty: a kind removed while its wallet holds money.
	ErrWalletNotEmpty = errors.New("economy: the wallet still holds money: move it to the owner's wallet first")
)

// checkWalletKind refuses a kind or fiat currency an agent cannot have, and answers the fiat currency, EUR for "".
func checkWalletKind(kind, fiat string) (string, error) {
	fiat = strings.ToUpper(strings.TrimSpace(fiat))
	if fiat == "" {
		fiat = CurrencyEUR
	}
	if (kind != WalletKindFiat && kind != WalletKindCrypto && kind != WalletKindBoth) ||
		(fiat != CurrencyGBP && fiat != CurrencyEUR && fiat != CurrencyUSD) {
		return "", fmt.Errorf("%w, not %q in %q", ErrWalletKind, kind, fiat)
	}
	return fiat, nil
}

// walletKindAssets is the assets of an agent of kind's wallets: its fiat currency first, then USDC.
func walletKindAssets(kind, fiat string) []string {
	switch kind {
	case WalletKindFiat:
		return []string{fiat}
	case WalletKindBoth:
		return []string{fiat, money.USDC}
	}
	return []string{money.USDC}
}

// AgentWallets is an agent's — or with no agent, its owner's — wallets.
type AgentWallets struct {
	AgentID      string        `json:"agent_id,omitempty"`
	WalletKind   string        `json:"wallet_kind,omitempty"`
	FiatCurrency string        `json:"fiat_currency,omitempty"`
	Wallets      []AgentWallet `json:"wallets"`
}

// agentWalletKind is agentID's kind and fiat currency.
func (s *DualTokenStore) agentWalletKind(ctx context.Context, workspaceID, agentID string) (kind, fiat string, err error) {
	err = s.pool.QueryRow(ctx, `SELECT wallet_kind, wallet_fiat_currency FROM agent_accounts WHERE id = $1 AND workspace_id = $2`,
		agentID, workspaceID).Scan(&kind, &fiat)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrAgentNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("economy: agent wallet kind: %w", err)
	}
	return kind, fiat, nil
}

// AgentWallets is agentID's wallets, one of each kind it is, with their balances and receive details; a wallet it
// should have and has not is opened now.
func (s *DualTokenStore) AgentWallets(ctx context.Context, workspaceID, agentID string) (AgentWallets, error) {
	if s.moneyLayer == nil {
		return AgentWallets{}, ErrAgentWalletsOff
	}
	kind, fiat, err := s.agentWalletKind(ctx, workspaceID, agentID)
	if err != nil {
		return AgentWallets{}, err
	}
	out := AgentWallets{AgentID: agentID, WalletKind: kind, FiatCurrency: fiat, Wallets: []AgentWallet{}}
	for _, asset := range walletKindAssets(kind, fiat) {
		w, err := s.agentWalletIn(ctx, workspaceID, agentID, asset)
		if err != nil {
			return AgentWallets{}, err
		}
		out.Wallets = append(out.Wallets, w)
	}
	return out, nil
}

// SetAgentWalletKind makes agentID kind, with its fiat wallet in fiat ("" keeps the one it has): the wallet of a kind
// it gains is opened, and that of a kind it loses closed — refused, closing none, while any of them holds money.
func (s *DualTokenStore) SetAgentWalletKind(ctx context.Context, workspaceID, agentID, kind, fiat string) (AgentWallets, error) {
	if s.moneyLayer == nil {
		return AgentWallets{}, ErrAgentWalletsOff
	}
	was, wasFiat, err := s.agentWalletKind(ctx, workspaceID, agentID)
	if err != nil {
		return AgentWallets{}, err
	}
	if strings.TrimSpace(fiat) == "" {
		fiat = wasFiat
	}
	if fiat, err = checkWalletKind(kind, fiat); err != nil {
		return AgentWallets{}, err
	}
	var archived bool
	if err := s.pool.QueryRow(ctx, `SELECT archived_at IS NOT NULL FROM agent_accounts WHERE id = $1`, agentID).Scan(&archived); err != nil {
		return AgentWallets{}, fmt.Errorf("economy: agent wallet kind: %w", err)
	}
	if archived {
		return AgentWallets{}, ErrAgentArchived
	}
	keep := map[string]bool{}
	for _, a := range walletKindAssets(kind, fiat) {
		keep[a] = true
	}
	type removal struct {
		w   AgentWallet
		ref string
	}
	var removed []removal
	for _, asset := range walletKindAssets(was, wasFiat) {
		if keep[asset] {
			continue
		}
		w, ref, _, err := s.readAgentWallet(ctx, workspaceID, agentID, asset)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return AgentWallets{}, err
		}
		if w.TestMinor != 0 || w.LiveMinor != 0 {
			return AgentWallets{}, fmt.Errorf("%w: its %s wallet holds %s", ErrWalletNotEmpty, asset, minorString(w.BalanceMinor, asset))
		}
		removed = append(removed, removal{w, ref})
	}
	for _, r := range removed {
		if err := s.closeAgentWallet(ctx, workspaceID, r.w, r.ref); err != nil {
			return AgentWallets{}, err
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE agent_accounts SET wallet_kind = $3, wallet_fiat_currency = $4 WHERE id = $1 AND workspace_id = $2`,
		agentID, workspaceID, kind, fiat); err != nil {
		return AgentWallets{}, fmt.Errorf("economy: agent wallet kind: %w", err)
	}
	closed := []string{}
	for _, r := range removed {
		closed = append(closed, r.w.Asset)
	}
	if err := s.AppendAudit(ctx, workspaceID, "agent:"+agentID, "agent.wallet_kind", map[string]any{"agent_id": agentID,
		"from": was, "from_fiat_currency": wasFiat, "to": kind, "fiat_currency": fiat, "closed": closed}); err != nil {
		return AgentWallets{}, err
	}
	return s.AgentWallets(ctx, workspaceID, agentID) // opens the wallet of a kind it gained
}

// closeAgentWallet closes an empty wallet in the ledger and at its partner, under its reference ref: the ledger's row
// is locked, closed and its balance read again first, and kept closed only once the partner has closed it too.
//
// shortcut: the partner is asked through the accounts capability, so a wallet must close where it opened; upgrade when
// the money layer's Wallets gains a close of its own.
func (s *DualTokenStore) closeAgentWallet(ctx context.Context, workspaceID string, w AgentWallet, ref string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: close agent wallet: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM money_accounts WHERE id = $1 FOR UPDATE`, w.AccountID); err != nil {
		return fmt.Errorf("economy: close agent wallet: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE money_accounts SET status = 'closed' WHERE id = $1 AND status = 'open'`, w.AccountID); err != nil {
		return fmt.Errorf("economy: close agent wallet: %w", err)
	}
	var held int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1`,
		w.AccountID).Scan(&held); err != nil {
		return fmt.Errorf("economy: close agent wallet: %w", err)
	}
	if held != 0 {
		return fmt.Errorf("%w: its %s wallet holds %s", ErrWalletNotEmpty, w.Asset, minorString(held, w.Asset))
	}
	if ref != "" {
		accounts, err := s.moneyLayer.Accounts(ctx, agentWalletRoute(workspaceID, w.AgentID, w.Asset))
		if err != nil {
			return err
		}
		_, err = accounts.Close(ctx, "agent_wallet_close:"+w.AccountID, ref)
		// The sandbox forgets its wallets when Lens restarts: one it has no record of holds nothing.
		if err != nil && (!errors.Is(err, money.ErrNotFound) || w.Partner != money.SandboxName) {
			return fmt.Errorf("economy: close agent wallet: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// subAccountWallet is w's receive details when it is a sub-account of its owner's: the owner's details, and the
// agent's payment reference to quote with them.
func (s *DualTokenStore) subAccountWallet(ctx context.Context, workspaceID string, w AgentWallet) (AgentWallet, error) {
	d, err := s.CurrencyAccountDetails(ctx, workspaceID, w.AgentID, w.AccountID)
	if err != nil {
		return AgentWallet{}, err
	}
	w.Receive = money.Details{Asset: w.Asset, Holder: d.Holder, IBAN: d.IBAN, BIC: d.BIC, SortCode: d.SortCode,
		AccountNumber: d.AccountNumber, RoutingNumber: d.RoutingNumber}
	w.PaymentReference, w.Mode, w.Notice = d.PaymentReference, d.Mode, d.Notice
	return w, nil
}

// OwnerWallets is the owner's wallets in workspaceID: one in each asset its agents' wallets are in, opened now if it
// has none yet.
func (s *DualTokenStore) OwnerWallets(ctx context.Context, workspaceID string) (AgentWallets, error) {
	if s.moneyLayer == nil {
		return AgentWallets{}, ErrAgentWalletsOff
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT v.asset COLLATE "C" FROM agent_accounts a,
		LATERAL (VALUES (CASE WHEN a.wallet_kind IN ('crypto', 'both') THEN 'USDC' END),
		                (CASE WHEN a.wallet_kind IN ('fiat', 'both') THEN a.wallet_fiat_currency END)) v(asset)
		WHERE a.workspace_id = $1 AND a.archived_at IS NULL AND v.asset IS NOT NULL ORDER BY 1`, workspaceID)
	if err != nil {
		return AgentWallets{}, fmt.Errorf("economy: owner wallets: %w", err)
	}
	assets, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return AgentWallets{}, fmt.Errorf("economy: owner wallets: %w", err)
	}
	out := AgentWallets{Wallets: []AgentWallet{}}
	for _, asset := range assets {
		w, err := s.ownerWallet(ctx, workspaceID, asset)
		if err != nil {
			return AgentWallets{}, err
		}
		out.Wallets = append(out.Wallets, w)
	}
	return out, nil
}

// ownerWallet is workspaceID's own account in asset, opened through the money layer if it has none.
func (s *DualTokenStore) ownerWallet(ctx context.Context, workspaceID, asset string) (AgentWallet, error) {
	rt := agentWalletRoute(workspaceID, "", asset)
	accounts, err := s.moneyLayer.Accounts(ctx, rt)
	if err != nil {
		return AgentWallet{}, err
	}
	var holder string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT name FROM workspaces WHERE id = $1), '')`, workspaceID).Scan(&holder); err != nil {
		return AgentWallet{}, fmt.Errorf("economy: owner wallet: %w", err)
	}
	if holder == "" {
		holder = workspaceID
	}
	open := money.OpenAccount{Key: "owner_wallet:" + workspaceID + ":" + asset, Holder: holder, Asset: asset}
	w, ref, err := s.readOwnerWallet(ctx, workspaceID, asset)
	if errors.Is(err, pgx.ErrNoRows) {
		partner, perr := s.moneyLayer.Partner(ctx, money.CapAccounts, rt)
		if perr != nil {
			return AgentWallet{}, perr
		}
		a, oerr := accounts.Open(ctx, open)
		if oerr != nil {
			return AgentWallet{}, fmt.Errorf("economy: open owner wallet: %w", oerr)
		}
		if a.Status != money.StatusCompleted {
			return AgentWallet{}, fmt.Errorf("%w: it answered %s: %s", ErrAccountNotOpened, a.Status, a.Detail)
		}
		if err := s.keepOwnerWallet(ctx, workspaceID, asset, holder, partner, a.Ref); err != nil && !errors.Is(err, ErrCurrencyAccountExists) {
			return AgentWallet{}, err // one opened at the same time is the owner's wallet
		}
		w, ref, err = s.readOwnerWallet(ctx, workspaceID, asset)
	}
	if err != nil {
		return AgentWallet{}, err
	}
	if w.Partner == "test" { // a company account opened at the account partner before the money layer (B30.13)
		d, err := s.CurrencyAccountDetails(ctx, workspaceID, "", w.AccountID)
		if err != nil {
			return AgentWallet{}, err
		}
		w.Receive = money.Details{Asset: asset, Holder: d.Holder, IBAN: d.IBAN, BIC: d.BIC, SortCode: d.SortCode,
			AccountNumber: d.AccountNumber, RoutingNumber: d.RoutingNumber}
		w.Mode, w.Notice = d.Mode, d.Notice
		return w, nil
	}
	w.Receive, err = accounts.Details(ctx, ref)
	if errors.Is(err, money.ErrNotFound) && w.Partner == money.SandboxName {
		// As an agent's wallet: the sandbox forgets it when Lens restarts, and opening it again gives back the same one.
		if again, oerr := accounts.Open(ctx, open); oerr == nil && again.Ref == ref {
			w.Receive, err = accounts.Details(ctx, ref)
		}
	}
	if err != nil {
		return AgentWallet{}, fmt.Errorf("economy: owner wallet's details: %w", err)
	}
	w.Mode = DetailsLive
	if w.Partner == money.SandboxName {
		w.Mode, w.Notice = DetailsTest, TestWalletNotice
	}
	return w, nil
}

// keepOwnerWallet keeps workspaceID's account in asset at partner, under ref, in the ledger: the company's money account
// and the partner account that mirrors it.
func (s *DualTokenStore) keepOwnerWallet(ctx context.Context, workspaceID, asset, holder, partner, ref string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: open owner wallet: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	insert := `INSERT INTO money_accounts (id, workspace_id, currency, purpose, name, partner_account_ref) VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := tx.Exec(ctx, insert, "macc_"+uuid.NewString(), workspaceID, asset, MoneyCompany, holder, ref); err != nil {
		return currencyAccountErr(err, "the owner's "+asset+" wallet")
	}
	if _, err := tx.Exec(ctx, insert, "macc_"+uuid.NewString(), workspaceID, asset, MoneyPartner, partner, ref); err != nil {
		return fmt.Errorf("economy: open owner wallet: %w", err)
	}
	return tx.Commit(ctx)
}

// readOwnerWallet is workspaceID's open account in asset and the partner's reference for it; pgx.ErrNoRows when none.
func (s *DualTokenStore) readOwnerWallet(ctx context.Context, workspaceID, asset string) (AgentWallet, string, error) {
	w := AgentWallet{Asset: asset}
	var ref string
	err := s.pool.QueryRow(ctx, `SELECT a.id, a.partner_account_ref, COALESCE(p.name, ''),
		COALESCE(sum(b.balance_minor) FILTER (WHERE b.funding = 'test'), 0)::bigint,
		COALESCE(sum(b.balance_minor) FILTER (WHERE b.funding = 'live'), 0)::bigint
		FROM money_accounts a
		LEFT JOIN money_accounts p ON p.workspace_id = a.workspace_id AND p.partner_account_ref = a.partner_account_ref AND p.purpose = 'partner'
		LEFT JOIN money_account_balances b ON b.account_id = a.id
		WHERE a.workspace_id = $1 AND a.currency = $2 AND a.purpose = 'company' AND a.status = 'open'
		GROUP BY a.id, p.name`, workspaceID, asset).Scan(&w.AccountID, &ref, &w.Partner, &w.TestMinor, &w.LiveMinor)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		err = fmt.Errorf("economy: owner wallet: %w", err)
	}
	w.BalanceMinor = w.TestMinor + w.LiveMinor
	return w, ref, err
}
