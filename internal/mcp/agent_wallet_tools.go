package mcp

import (
	"cmp"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	tokens "github.com/talyvor/lens/internal/money/holdings"
	"github.com/talyvor/lens/internal/money/smartaccount"
)

// agent_wallet_tools.go — B48.16: THE AGENT'S OWN WALLETS OVER MCP, FOR ASSISTANTS AND AGENTS.
//
// An agent's own key reads its wallets (B48.1) — each one, its balance as the ledger keeps it and, for the USDC
// wallet, what its smart account holds on each network — where each is paid into, and its history; it sends USDC from
// its smart account (B48.9) and asks to be paid into it (B48.10). wallet_send and wallet_request were the credit tools'
// already (B22.11): a call that names a network is the agent's USDC wallet's, one that does not stays a credit one.
//
// A send goes through SendAgentCrypto as the agent's HTTP route's does, never around it: inside the owner's signed
// spend permission, to an address the workspace has paid, it is signed and sent, once per idempotency_key; to an
// address the workspace never paid it is prepared and waits for the owner's tap, and nothing is signed; over the
// permission it is refused before anything is signed. Every call is logged with the agent's other tool calls, and every
// money call is in the money layer's audit trail as the store keeps it.
//
// B48.39: wallet_topup_link asks the agent's owner to top its USDC wallet up. It quotes the top-up through B48.8's
// QuoteWalletMove, as the owner's own quote route does — a card or a bank transfer buying USDC at the ramp's rate and
// fee — and answers the link the owner opens and the route their confirm goes to. The agent moves nothing: only the
// owner's confirm charges them and credits the wallet, once, at the quote.

// AgentWalletBank is what the wallet tools need of the economy store for the agent's own wallets.
type AgentWalletBank interface {
	AgentWallets(ctx context.Context, workspaceID, agentID string) (economy.AgentWallets, error)
	AgentSmartAccount(ctx context.Context, workspaceID, agentID string) (economy.AgentSmartAccount, error)
	AgentCryptoBalances(ctx context.Context, workspaceID, agentID, currency string) (tokens.Statement, error)
	WalletActivity(ctx context.Context, workspaceID, agentID string, limit int) ([]economy.WalletLine, error)
	SendAgentCrypto(ctx context.Context, workspaceID, agentID, by string, byOwner bool, key string, in economy.CryptoSendRequest) (economy.CryptoSend, error)
	AgentCryptoSends(ctx context.Context, workspaceID, agentID string) ([]economy.CryptoSend, error)
}

// AgentTopUpBank is B48.8's quote of a top-up of an agent's wallet, which wallet_topup_link asks for.
type AgentTopUpBank interface {
	QuoteWalletMove(ctx context.Context, workspaceID, agentID string, r economy.WalletMoveRequest, by string, workspaceOwner bool) (economy.WalletMove, error)
}

// WalletTopUpLinkBase is the app's page for an agent, where its owner opens a top-up the agent asked for (?top_up=).
const WalletTopUpLinkBase = "https://app.talyvor.com/agents/"

// CryptoRequester is B48.10's chain indexer: the networks whose deposits it sees, and the requests to be paid
// wallet_request makes.
type CryptoRequester interface {
	Networks() []string
	CreatePaymentRequest(ctx context.Context, in economy.CryptoRequestIn) (economy.CryptoRequest, error)
}

// SetCryptoRequests lets wallet_request ask to be paid into the agent's USDC wallet, and names the networks
// wallet_receive_details says it is paid on; nil refuses the one and names none.
func (s *Server) SetCryptoRequests(c CryptoRequester) { s.cryptoRequests = c }

// agentWalletRefusals are the store's answers that say no to what the agent asked of its own wallets.
var agentWalletRefusals = []error{
	economy.ErrAgentWalletsOff, economy.ErrSmartAccountsOff, economy.ErrSmartAccountNotFound, economy.ErrCryptoSendsOff,
	economy.ErrCryptoSend, economy.ErrSpendPermission, smartaccount.ErrInvalid, smartaccount.ErrRefused,
	economy.ErrCryptoRequest, economy.ErrCryptoRequestConflict, economy.ErrCryptoRequestLimit, economy.ErrCryptoRequestChain,
	economy.ErrWalletMove, economy.ErrNoRampPartner,
}

const (
	walletHistoryDefault = 20
	walletHistoryMost    = 100
)

func agentWalletToolDefinitions() []map[string]any {
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	tool := func(name, desc string, props map[string]any) map[string]any {
		return map[string]any{"name": name, "description": desc, "inputSchema": map[string]any{"type": "object", "properties": props, "required": []string{}}}
	}
	asset := str("one wallet only: GBP, EUR, USD or USDC; leave it out for all of them")
	return []map[string]any{
		tool("wallet_list", "Your own wallets: a pounds, euros or dollars wallet, a USDC wallet, or one of each, with each one's balance.", map[string]any{}),
		tool("wallet_balance", "What each of your wallets holds: its balance, and for your USDC wallet what it holds on each network.",
			map[string]any{"asset": asset}),
		tool("wallet_receive_details", "Where each of your wallets is paid into: a currency wallet's account details and payment reference, "+
			"your USDC wallet's address and the networks a payment to it is seen on.", map[string]any{"asset": asset}),
		tool("wallet_history", "Your wallets' latest movements, newest first, each with the balance after it, and every USDC send you made or were sent.",
			map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "description": "how many movements, at most 100; 20 when left out"}}),
		{"name": "wallet_topup_link", "description": "Ask your owner to top your USDC wallet up: a quote of what they pay by card or bank " +
			"transfer, the fee and the USDC it buys, and the link they open to confirm it. Nothing moves until they do.",
			"inputSchema": map[string]any{"type": "object", "required": []string{"amount_minor", "currency"}, "properties": map[string]any{
				"amount_minor": map[string]any{"type": "integer", "minimum": 1, "description": "what your owner pays, in pence or cents, the fee included"},
				"currency":     map[string]any{"type": "string", "enum": []string{"GBP", "EUR", "USD"}, "description": "what your owner pays in"},
				"method": map[string]any{"type": "string", "enum": []string{economy.WalletMoveCard, economy.WalletMoveBankTransfer},
					"description": "how your owner pays: card (the default) or bank_transfer"}}}},
	}
}

// agentWalletTool answers the agent-wallet tools, and wallet_send and wallet_request when they name a network; ok is
// false for any other call, which stays the credit tools'.
func (s *Server) agentWalletTool(ctx context.Context, name, ws, agent string, a walletArgs) (res any, ok bool, err error) {
	switch name {
	case "wallet_list", "wallet_balance", "wallet_receive_details", "wallet_history", "wallet_topup_link":
	case "wallet_send", "wallet_request":
		if a.Network == "" {
			return nil, false, nil
		}
	default:
		return nil, false, nil
	}
	bank, has := s.agentBank.(AgentWalletBank)
	if !has {
		return nil, true, errors.New("the agent wallet tools are not configured")
	}
	res, err = s.runAgentWalletTool(ctx, bank, name, ws, agent, a)
	for _, e := range agentWalletRefusals {
		if errors.Is(err, e) {
			return nil, true, &toolRefusal{err.Error()}
		}
	}
	return res, true, err
}

func (s *Server) runAgentWalletTool(ctx context.Context, bank AgentWalletBank, name, ws, agent string, a walletArgs) (any, error) {
	switch name {
	case "wallet_topup_link":
		quotes, ok := bank.(AgentTopUpBank)
		if !ok {
			return nil, errors.New("topping up an agent's wallet is not configured")
		}
		method := cmp.Or(a.Method, economy.WalletMoveCard)
		if method != economy.WalletMoveCard && method != economy.WalletMoveBankTransfer {
			return nil, &toolRefusal{"method is card or bank_transfer"}
		}
		m, err := quotes.QuoteWalletMove(ctx, ws, agent, economy.WalletMoveRequest{Route: method, AmountMinor: a.AmountMinor, Currency: a.Currency},
			"agent:"+agent, false)
		if err != nil {
			return nil, err
		}
		return map[string]any{"move": m, "link": WalletTopUpLinkBase + agent + "?top_up=" + m.ID,
			"confirm": "/v1/workspaces/" + ws + "/agents/" + agent + "/wallet-moves/" + m.ID + "/confirm",
			"message": "Send your owner this link. They see what they pay, the fee and the USDC it buys, and confirm it before the quote " +
				"expires. Nothing has moved."}, nil
	case "wallet_send":
		if a.AmountULXC != 0 {
			return nil, &toolRefusal{"a send on a network is in amount_minor (1 USDC = 1,000,000), not amount_ulxc"}
		}
		if a.To == "" || a.AmountMinor <= 0 || a.IdempotencyKey == "" {
			return nil, &toolRefusal{"to, a positive amount_minor and an idempotency_key are required to send on a network"}
		}
		c, err := bank.SendAgentCrypto(ctx, ws, agent, "agent:"+agent, false, a.IdempotencyKey,
			economy.CryptoSendRequest{To: a.To, Network: a.Network, Token: cmp.Or(a.Token, money.USDC), AmountMinor: a.AmountMinor})
		if err != nil {
			return nil, err
		}
		if c.Status == "awaiting_owner" {
			return struct {
				economy.CryptoSend
				Message string `json:"message"`
			}{c, "Waiting for the owner to approve a first payment to this address. Nothing has moved."}, nil
		}
		return c, nil
	case "wallet_request":
		if a.AmountULXC != 0 || a.From != "" {
			return nil, &toolRefusal{"a request on a network is in amount_minor (1 USDC = 1,000,000), from payer_address if it names one"}
		}
		if s.cryptoRequests == nil {
			return nil, &toolRefusal{economy.ErrCryptoRequestChain.Error()}
		}
		if a.AmountMinor <= 0 || a.ExpiresAt == "" || a.IdempotencyKey == "" {
			return nil, &toolRefusal{"a positive amount_minor, expires_at and an idempotency_key are required to ask on a network"}
		}
		expires, err := time.Parse(time.RFC3339, a.ExpiresAt)
		if err != nil {
			return nil, &toolRefusal{"expires_at must be an RFC 3339 time"}
		}
		return s.cryptoRequests.CreatePaymentRequest(ctx, economy.CryptoRequestIn{WorkspaceID: ws, AgentID: agent, Token: cmp.Or(a.Token, money.USDC),
			Network: a.Network, AmountMinor: a.AmountMinor, Memo: a.Memo, ExpiresAt: expires, PayerAddress: a.PayerAddress,
			IdempotencyKey: a.IdempotencyKey, CreatedBy: "agent:" + agent})
	case "wallet_history":
		limit := walletHistoryDefault
		if a.Limit > 0 {
			limit = min(a.Limit, walletHistoryMost)
		}
		lines, err := bank.WalletActivity(ctx, ws, agent, limit)
		if err != nil {
			return nil, err
		}
		sends, err := bank.AgentCryptoSends(ctx, ws, agent)
		if errors.Is(err, economy.ErrCryptoSendsOff) || errors.Is(err, economy.ErrSmartAccountsOff) {
			sends, err = []economy.CryptoSend{}, nil
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"lines": lines, "sends": sends}, nil
	}

	wallets, err := bank.AgentWallets(ctx, ws, agent)
	if err != nil {
		return nil, err
	}
	mine := wallets.Wallets[:0:0]
	for _, w := range wallets.Wallets {
		if a.Asset == "" || strings.EqualFold(w.Asset, a.Asset) {
			mine = append(mine, w)
		}
	}
	switch name {
	case "wallet_list":
		wallets.Wallets = mine
		return wallets, nil
	case "wallet_balance":
		type balance struct {
			AccountID    string            `json:"account_id"`
			Asset        string            `json:"asset"`
			BalanceMinor int64             `json:"balance_minor"`
			Mode         string            `json:"mode"`
			OnChain      *tokens.Statement `json:"on_chain,omitempty"` // what the smart account holds, network by network
		}
		out := []balance{}
		for _, w := range mine {
			b := balance{AccountID: w.AccountID, Asset: w.Asset, BalanceMinor: w.BalanceMinor, Mode: w.Mode}
			if w.Asset == money.USDC {
				st, err := bank.AgentCryptoBalances(ctx, ws, agent, "")
				switch {
				case err == nil:
					b.OnChain = &st
				case !errors.Is(err, economy.ErrSmartAccountsOff) && !errors.Is(err, economy.ErrSmartAccountNotFound):
					return nil, err
				}
			}
			out = append(out, b)
		}
		return map[string]any{"balances": out}, nil
	}
	// wallet_receive_details
	type receive struct {
		AccountID        string        `json:"account_id"`
		Asset            string        `json:"asset"`
		Receive          money.Details `json:"receive"`
		PaymentReference string        `json:"payment_reference,omitempty"`
		Address          string        `json:"address,omitempty"` // the USDC wallet's smart account, the same on every network named
		Networks         []string      `json:"networks,omitempty"`
		Note             string        `json:"note,omitempty"`
	}
	out := []receive{}
	for _, w := range mine {
		r := receive{AccountID: w.AccountID, Asset: w.Asset, Receive: w.Receive, PaymentReference: w.PaymentReference}
		if w.Asset == money.USDC {
			acct, err := bank.AgentSmartAccount(ctx, ws, agent)
			switch {
			case err == nil && acct.Address != "":
				r.Address = acct.Address
				if s.cryptoRequests != nil {
					r.Networks = s.cryptoRequests.Networks()
				}
			case err == nil, errors.Is(err, economy.ErrSmartAccountNotFound), errors.Is(err, economy.ErrSmartAccountsOff):
				r.Note = "Your owner has not set up this wallet's address yet."
			default:
				return nil, err
			}
		}
		out = append(out, r)
	}
	return map[string]any{"wallets": out}, nil
}
