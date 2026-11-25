package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/money"
)

// person_crypto_wallets.go — B48.32: AN AGENT SENDS USDC TO A PERSON ON TALYVOR BY THEIR NAME OR EMAIL, INTO THE
// WALLET THEY CHOSE.
//
// A person who is found (B61.32's names, numbers and emails, each its own switch) names, per token and network, the
// agent wallet of theirs that receives — one of the agents on their own accounts that holds a smart account. A send's
// `to` (B48.9) then takes @name, an email or a phone number as well as a 0x address, and resolves only to a person who
// said yes to being found that way and chose a wallet on that network: anyone else — nobody, a person who has not said
// yes, one with no wallet there — answers PersonNotFoundWords, padded to the same time, and every lookup, by a quote or
// a send, counts against the account's limit an hour. The sender sees the person's shown name; the first send to them
// waits for the owner as any new address does.

// PersonCryptoWallet is the agent wallet a person receives one token into on one network.
type PersonCryptoWallet struct {
	Network string `json:"network"`
	Token   string `json:"token"`
	AgentID string `json:"agent_id"`
}

// personCryptoWallets is what personID receives into, by network.
func personCryptoWallets(ctx context.Context, q pgxDB, personID string) ([]PersonCryptoWallet, error) {
	rows, err := q.Query(ctx, `SELECT network, token, agent_id FROM person_crypto_wallets WHERE person_id = $1 ORDER BY network, token`, personID)
	if err != nil {
		return nil, fmt.Errorf("economy: being found: %w", err)
	}
	defer rows.Close()
	out := []PersonCryptoWallet{}
	for rows.Next() {
		var w PersonCryptoWallet
		if err := rows.Scan(&w.Network, &w.Token, &w.AgentID); err != nil {
			return nil, fmt.Errorf("economy: being found: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// setPersonCryptoWallet names, in tx, the agent wallet personID receives ch's token into on ch's network; an AgentID
// of "" stops them receiving there.
func (s *DualTokenStore) setPersonCryptoWallet(ctx context.Context, tx pgx.Tx, personID, personKey, firstID string, ch PersonCryptoWallet) error {
	network, token := strings.TrimSpace(ch.Network), strings.ToUpper(strings.TrimSpace(ch.Token))
	if _, ok := s.cryptoSendNets[network]; !ok {
		return fmt.Errorf("%w: Talyvor sends on no network called %q", ErrPersonFinding, ch.Network)
	}
	if token != money.USDC {
		return fmt.Errorf("%w: a wallet receives USDC", ErrPersonFinding)
	}
	agentID := strings.TrimSpace(ch.AgentID)
	if agentID == "" {
		if _, err := tx.Exec(ctx, `DELETE FROM person_crypto_wallets WHERE person_id = $1 AND network = $2 AND token = $3`,
			personID, network, token); err != nil {
			return fmt.Errorf("economy: being found: %w", err)
		}
		return nil
	}
	var workspaceID string
	err := tx.QueryRow(ctx, `SELECT workspace_id FROM agent_smart_accounts WHERE agent_id = $1
		AND (workspace_id = $3 OR workspace_id IN (SELECT workspace_id FROM person_accounts WHERE person_key = $2))`,
		agentID, personKey, firstID).Scan(&workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: the wallet to receive into is an agent of yours with a crypto account", ErrPersonFinding)
	}
	if err != nil {
		return fmt.Errorf("economy: being found: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO person_crypto_wallets (person_id, network, token, agent_id, workspace_id) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (person_id, network, token) DO UPDATE SET agent_id = EXCLUDED.agent_id, workspace_id = EXCLUDED.workspace_id, updated_at = now()`,
		personID, network, token, agentID, workspaceID); err != nil {
		return fmt.Errorf("economy: being found: %w", err)
	}
	return nil
}

// lookUpPersonWallet finds the person to names for workspaceID, counting it against the account's limit an hour, and
// answers them with the address of the wallet they receive USDC into on n, or ErrPersonNotFound for anyone who has
// not said yes to being found that way or chose no wallet there.
func (s *DualTokenStore) lookUpPersonWallet(ctx context.Context, workspaceID, to string, n CryptoSendNetwork) (foundPerson, string, error) {
	if err := s.countLookup(ctx, workspaceID); err != nil {
		return foundPerson{}, "", err
	}
	floor := time.Now().Add(personFindFloor)
	defer func() { time.Sleep(time.Until(floor)) }()
	p, found, err := s.resolvePerson(ctx, to)
	switch {
	case err != nil:
		return foundPerson{}, "", err
	case !found:
		return foundPerson{}, "", ErrPersonNotFound
	}
	var address string
	err = s.pool.QueryRow(ctx, `SELECT a.address FROM person_crypto_wallets w
		JOIN agent_smart_accounts a ON a.agent_id = w.agent_id AND a.workspace_id = w.workspace_id
		WHERE w.person_id = $1 AND w.network = $2 AND w.token = $3
		  AND (w.workspace_id = $5 OR w.workspace_id IN (SELECT workspace_id FROM person_accounts WHERE person_key = $4))`,
		p.personID, n.Name, money.USDC, p.personKey, p.firstID).Scan(&address)
	if errors.Is(err, pgx.ErrNoRows) {
		return foundPerson{}, "", ErrPersonNotFound
	}
	if err != nil {
		return foundPerson{}, "", fmt.Errorf("economy: look up a person: %w", err)
	}
	if n.Funding == FundingLive && p.byPhone && p.phoneSandbox {
		return foundPerson{}, "", ErrPersonPhoneSandbox
	}
	return p, address, nil
}

// cryptoToHash is a person's name, number or email as a send asked for it, kept only as a hash, so a retry under the
// same key that finds someone else is refused; "" for a 0x address.
func cryptoToHash(to string) string {
	if isCryptoAddress(to) {
		return ""
	}
	return PersonPayment{To: to}.toHash()
}

// isCryptoAddress is whether to is written as a 0x address rather than a person's name, number or email.
func isCryptoAddress(to string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(to)), "0x")
}
