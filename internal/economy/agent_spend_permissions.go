package economy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/passkey"
)

// agent_spend_permissions.go — B48.3: THE OWNER'S LIMITS ON WHAT A CRYPTO AGENT'S KEY MAY PAY, SIGNED BY THEM, AND AN
// INSTANT FREEZE.
//
// The person writes a spend permission for their agent (internal/money/smartaccount.Permission: per token an allowance
// per day, week or month and a cap on one payment, the counterparties allowed and blocked, an expiry) and signs it with
// the passkey or device key that owns the agent's smart account — one tap. PrepareSpendPermission answers it as
// canonical JSON, numbered one after the permission it replaces, and its challenge (the SHA-256 of that JSON);
// SetSpendPermission keeps it only if the account's own owner signed exactly that. Changing it is signing the next
// one; revoking it is signing one with no tokens. AuthoriseAgentCryptoPayment is Talyvor's gate: it checks the
// signature again, refuses everything for a frozen agent, and keeps each payment it allows so the period's allowance
// counts it, before the agent's key is asked to sign (smartaccount.SignPaymentAsAgent). FreezeAgent refuses
// everything at once, and answers the call the person's passkey signs to take the agent's key out of the account on
// chain (smartaccount.FreezeCall). Class: rules_approvals_statements_pots, GREEN — nothing here moves money.

// ErrSpendPermission: a permission that cannot be kept, or a signature that does not sign it.
var ErrSpendPermission = errors.New("economy: invalid spend permission")

// SpendPermissionVersion names the canonical form signed.
const SpendPermissionVersion = "talyvor.spend_permission.v1"

// SpendPermissionTerms is what the person signs: whose account, which number, and the permission.
type SpendPermissionTerms struct {
	Version     string                  `json:"version"`
	WorkspaceID string                  `json:"workspace_id"`
	AgentID     string                  `json:"agent_id"`
	Account     string                  `json:"account"`
	Number      int64                   `json:"number"` // one after the permission it replaces: an older one cannot come back
	Permission  smartaccount.Permission `json:"permission"`
}

func (t SpendPermissionTerms) canonical() string {
	b, _ := json.Marshal(t)
	return string(b)
}

// SpendPermission is an agent's signed permission, and whether the agent is frozen.
type SpendPermission struct {
	SpendPermissionTerms
	Canonical    string     `json:"canonical"`
	CredentialID string     `json:"credential_id,omitempty"`
	DeviceID     string     `json:"device_id,omitempty"`
	SignedBy     string     `json:"signed_by"`
	SignedAt     time.Time  `json:"signed_at"`
	FrozenAt     *time.Time `json:"frozen_at,omitempty"`
}

// spendAccount is the agent's smart account as the permission needs it, locked in tx when tx is not nil.
type spendAccount struct {
	address, credentialID, deviceID, agentSigner string
	ownerKey                                     []byte
	agentIndex                                   uint64
	rotatedFrom                                  []string // B48.15: every key the agent rotated from, and their indexes
	rotatedFromIndexes                           []int64
	frozenAt                                     *time.Time
}

func (s *DualTokenStore) spendAccount(ctx context.Context, q pgx.Tx, workspaceID, agentID string) (spendAccount, error) {
	var a spendAccount
	err := q.QueryRow(ctx, `SELECT address, owner_credential_id, owner_device_id, agent_signer, owner_public_key, agent_index, rotated_from_signers,
		rotated_from_indexes, frozen_at FROM agent_smart_accounts WHERE agent_id = $1 AND workspace_id = $2 FOR UPDATE`, agentID, workspaceID).
		Scan(&a.address, &a.credentialID, &a.deviceID, &a.agentSigner, &a.ownerKey, &a.agentIndex, &a.rotatedFrom, &a.rotatedFromIndexes, &a.frozenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, _, kerr := s.agentWalletKind(ctx, workspaceID, agentID); kerr != nil {
			return a, kerr
		}
		return a, ErrSmartAccountNotFound
	}
	if err != nil {
		return a, fmt.Errorf("economy: spend permission: %w", err)
	}
	return a, nil
}

// inTx runs f in one transaction.
func (s *DualTokenStore) inTx(ctx context.Context, f func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := f(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PrepareSpendPermission answers p for agentID's account as the canonical JSON its owner signs, and the challenge.
func (s *DualTokenStore) PrepareSpendPermission(ctx context.Context, workspaceID, agentID string, p smartaccount.Permission) (canonical, challenge string, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		a, err := s.spendAccount(ctx, tx, workspaceID, agentID)
		if err != nil {
			return err
		}
		if a.frozenAt != nil {
			return fmt.Errorf("%w: %w", ErrSpendPermission, smartaccount.ErrFrozen)
		}
		p.Normalise()
		if err := p.Validate(time.Now()); err != nil {
			return err
		}
		var last int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(number), 0) FROM agent_spend_permissions WHERE agent_id = $1`, agentID).Scan(&last); err != nil {
			return fmt.Errorf("economy: spend permission: %w", err)
		}
		canonical = SpendPermissionTerms{SpendPermissionVersion, workspaceID, agentID, a.address, last + 1, p}.canonical()
		return nil
	})
	return canonical, MandateChallenge(canonical), err
}

// parseSpendPermission reads canonical and refuses anything that is not exactly the canonical form of terms that may
// be signed at now.
func parseSpendPermission(canonical string) (SpendPermissionTerms, error) {
	var t SpendPermissionTerms
	dec := json.NewDecoder(strings.NewReader(canonical))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil || t.Version != SpendPermissionVersion {
		return t, fmt.Errorf("%w: not a spend permission's JSON", ErrSpendPermission)
	}
	n := t
	n.Permission.Normalise()
	if n.canonical() != canonical {
		return t, fmt.Errorf("%w: not in its canonical form, as prepare answered it", ErrSpendPermission)
	}
	return t, nil
}

// SetSpendPermission keeps the permission the account's owner signed: canonical as PrepareSpendPermission answered it,
// and an assertion over its challenge by the very passkey or device key that owns the agent's account. Another
// signer, another agent's or workspace's permission, an altered one or one that is not the next number is refused.
func (s *DualTokenStore) SetSpendPermission(ctx context.Context, workspaceID, agentID, by, canonical string, a *passkey.Assertion) (SpendPermission, error) {
	t, err := parseSpendPermission(canonical)
	if err != nil {
		return SpendPermission{}, err
	}
	if t.WorkspaceID != workspaceID || t.AgentID != agentID {
		return SpendPermission{}, fmt.Errorf("%w: it is another agent's", ErrSpendPermission)
	}
	if err := t.Permission.Validate(time.Now()); err != nil {
		return SpendPermission{}, err
	}
	if a == nil {
		return SpendPermission{}, fmt.Errorf("%w: it is not signed: sign its challenge with the passkey that owns the agent's account", ErrSpendPermission)
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		acct, err := s.spendAccount(ctx, tx, workspaceID, agentID)
		if err != nil {
			return err
		}
		var last int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(number), 0) FROM agent_spend_permissions WHERE agent_id = $1`, agentID).Scan(&last); err != nil {
			return fmt.Errorf("economy: spend permission: %w", err)
		}
		switch {
		case acct.frozenAt != nil:
			return fmt.Errorf("%w: %w", ErrSpendPermission, smartaccount.ErrFrozen)
		case !strings.EqualFold(t.Account, acct.address):
			return fmt.Errorf("%w: it names another account", ErrSpendPermission)
		case t.Number != last+1:
			return fmt.Errorf("%w: it is number %d and the next is %d: prepare it again", ErrSpendPermission, t.Number, last+1)
		case a.DeviceID != acct.deviceID || (a.DeviceID == "" && a.CredentialID != acct.credentialID):
			return fmt.Errorf("%w: only the passkey that owns the agent's account signs its permission", passkey.ErrInvalid)
		}
		challenge := MandateChallenge(canonical)
		if a.DeviceID != "" {
			if err := signDevice(ctx, tx, workspaceID, challenge, *a); err != nil {
				return err
			}
			a = &passkey.Assertion{DeviceID: a.DeviceID, Signature: a.Signature}
		} else {
			if s.approvalRP == nil {
				return fmt.Errorf("%w: passkeys are not configured on this server", passkey.ErrInvalid)
			}
			var spki []byte
			var count int64
			err := tx.QueryRow(ctx, `SELECT public_key, sign_count FROM workspace_passkeys WHERE credential_id = $1 AND workspace_id = $2 AND removed_at IS NULL AND revoked_at IS NULL FOR UPDATE`,
				a.CredentialID, workspaceID).Scan(&spki, &count)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: the passkey that owns the account is no longer the workspace's", passkey.ErrInvalid)
			}
			if err != nil {
				return fmt.Errorf("economy: passkey: %w", err)
			}
			next, err := s.approvalRP.VerifyAssertion(*a, challenge, spki, uint32(count))
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE workspace_passkeys SET sign_count = $2, last_used_at = now() WHERE credential_id = $1`,
				a.CredentialID, int64(next)); err != nil {
				return fmt.Errorf("economy: passkey: %w", err)
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO agent_spend_permissions (agent_id, number, workspace_id, canonical, credential_id, device_id,
			client_data_json, authenticator_data, signature, signed_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			agentID, t.Number, workspaceID, canonical, a.CredentialID, a.DeviceID, a.ClientDataJSON, a.AuthenticatorData, a.Signature, by)
		if err != nil {
			return fmt.Errorf("economy: spend permission: %w", err)
		}
		return nil
	})
	if err != nil {
		return SpendPermission{}, err
	}
	if err := s.AppendAudit(ctx, workspaceID, "agent:"+agentID, "agent.spend_permission", map[string]any{"agent_id": agentID,
		"number": t.Number, "canonical": canonical, "credential_id": a.CredentialID, "device_id": a.DeviceID, "signed_by": by}); err != nil {
		return SpendPermission{}, err
	}
	return s.AgentSpendPermission(ctx, workspaceID, agentID)
}

// ErrNoSpendPermission: the owner has signed no permission for this agent yet.
var ErrNoSpendPermission = errors.New("economy: the owner has signed no spend permission for this agent")

// AgentSpendPermission is agentID's latest signed permission, with whether it is frozen.
func (s *DualTokenStore) AgentSpendPermission(ctx context.Context, workspaceID, agentID string) (SpendPermission, error) {
	var p SpendPermission
	err := s.pool.QueryRow(ctx, `SELECT p.canonical, p.credential_id, p.device_id, p.signed_by, p.signed_at, a.frozen_at
		FROM agent_spend_permissions p JOIN agent_smart_accounts a ON a.agent_id = p.agent_id AND a.workspace_id = p.workspace_id
		WHERE p.agent_id = $1 AND p.workspace_id = $2 ORDER BY p.number DESC LIMIT 1`, agentID, workspaceID).
		Scan(&p.Canonical, &p.CredentialID, &p.DeviceID, &p.SignedBy, &p.SignedAt, &p.FrozenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := s.AgentSmartAccount(ctx, workspaceID, agentID); err != nil {
			return p, err
		}
		return p, ErrNoSpendPermission
	}
	if err != nil {
		return p, fmt.Errorf("economy: spend permission: %w", err)
	}
	p.SpendPermissionTerms, err = parseSpendPermission(p.Canonical)
	return p, err
}

// AuthoriseAgentCryptoPayment is Talyvor's gate before agentID's key is asked to sign pay: the agent is not frozen,
// archived or paused (on its own or with every agent, B55.23), the permission it pays under is the one its account's owner signed, and pay is inside it, counting what it already
// paid in the token this period. A payment it allows is kept, and counts against the allowance from then on: its use,
// on chainID, which SendAgentCryptoPayment sends and SettleAgentCryptoPayments settles from the chain.
func (s *DualTokenStore) AuthoriseAgentCryptoPayment(ctx context.Context, workspaceID, agentID string, chainID int64, pay smartaccount.Payment) (int64, error) {
	var use int64
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		acct, err := s.spendAccount(ctx, tx, workspaceID, agentID) // locked: two payments are counted one after the other
		if err != nil {
			return err
		}
		if acct.frozenAt != nil {
			return smartaccount.ErrFrozen
		}
		if err := refuseIfPaused(ctx, tx, agentID); err != nil {
			return err
		}
		var canonical string
		var a passkey.Assertion
		var spki, deviceSPKI []byte
		err = tx.QueryRow(ctx, `SELECT p.canonical, p.credential_id, p.device_id, p.client_data_json, p.authenticator_data, p.signature,
			k.public_key, d.public_key FROM agent_spend_permissions p
			LEFT JOIN workspace_passkeys k ON k.credential_id = p.credential_id AND k.workspace_id = p.workspace_id AND k.removed_at IS NULL AND k.revoked_at IS NULL
			LEFT JOIN workspace_device_keys d ON d.id = p.device_id AND d.workspace_id = p.workspace_id AND d.revoked_at IS NULL
			WHERE p.agent_id = $1 AND p.workspace_id = $2 ORDER BY p.number DESC LIMIT 1`, agentID, workspaceID).
			Scan(&canonical, &a.CredentialID, &a.DeviceID, &a.ClientDataJSON, &a.AuthenticatorData, &a.Signature, &spki, &deviceSPKI)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %w", smartaccount.ErrPermission, ErrNoSpendPermission)
		}
		if err != nil {
			return fmt.Errorf("economy: spend permission: %w", err)
		}
		// The signature is checked again: a permission altered after it was signed allows nothing.
		t, err := parseSpendPermission(canonical)
		notSigned := fmt.Errorf("%w: the permission is not what the account's owner signed", smartaccount.ErrPermission)
		switch {
		case err != nil || t.AgentID != agentID || t.WorkspaceID != workspaceID || !strings.EqualFold(t.Account, acct.address):
			return notSigned
		case a.DeviceID != "":
			if a.DeviceID != acct.deviceID || deviceSPKI == nil || devicekey.Verify(deviceSPKI, MandateChallenge(canonical), a.Signature) != nil {
				return notSigned
			}
		default:
			if a.CredentialID != acct.credentialID || spki == nil || s.approvalRP == nil {
				return notSigned
			}
			if _, err := s.approvalRP.VerifyAssertion(a, MandateChallenge(canonical), spki, 0); err != nil {
				return notSigned
			}
		}
		now := time.Now()
		token, to := smartaccount.Checksum(pay.Token), smartaccount.Checksum(pay.To)
		var spent int64
		if l, ok := t.Permission.Limit(token); ok {
			if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0) FROM agent_spend_permission_uses
				WHERE agent_id = $1 AND token = $2 AND used_at >= $3`, agentID, token, smartaccount.PeriodStart(l.Period, now)).Scan(&spent); err != nil {
				return fmt.Errorf("economy: spend permission: %w", err)
			}
		}
		if err := t.Permission.Check(pay, spent, now); err != nil {
			return err
		}
		fee, paymaster := tokenFee(pay)
		if err := tx.QueryRow(ctx, `INSERT INTO agent_spend_permission_uses (agent_id, workspace_id, permission_number, token, counterparty, amount, fee,
			paymaster, chain_id, used_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
			agentID, workspaceID, t.Number, token, to, pay.Total().Int64(), fee, paymaster, chainID, now).Scan(&use); err != nil {
			return fmt.Errorf("economy: spend permission: %w", err)
		}
		return nil
	})
	return use, err
}

// tokenFee is the most pay's ERC-20 paymaster may take for its network fee, and the paymaster: nil for a payment with no
// such fee.
func tokenFee(pay smartaccount.Payment) (fee *int64, paymaster *string) {
	if pay.Fee == nil {
		return nil, nil
	}
	f, p := pay.Fee.Int64(), smartaccount.Checksum(pay.Paymaster)
	return &f, &p
}

// SendAgentCryptoPayment hands op, the operation agentID's key signed for its payment use, to chain c's bundler and
// answers its hash. op is kept with the use first, so SettleAgentCryptoPayments settles it from the chain however the
// bundler answers: an operation the bundler refuses could still reach the chain, so it counts until its account uses its
// nonce for another. op must be exactly the payment the gate allowed, on c, out of the agent's own account.
func (s *DualTokenStore) SendAgentCryptoPayment(ctx context.Context, c smartaccount.Chain, workspaceID, agentID string, use int64, op smartaccount.UserOp) (string, error) {
	hash, err := s.keepAgentCryptoOp(ctx, c, workspaceID, agentID, use, op)
	if err != nil {
		return "", err
	}
	sent, err := c.Send(ctx, op)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(sent, hash) {
		return "", fmt.Errorf("smartaccount: the bundler hashes the operation %s, Lens %s", sent, hash)
	}
	return hash, nil
}

// keepAgentCryptoOp keeps op with use, agentID's payment the gate allowed, before any bundler sees it, and answers its
// hash: from then on the use is settled from the chain. Keeping the same operation again changes nothing.
func (s *DualTokenStore) keepAgentCryptoOp(ctx context.Context, c smartaccount.Chain, workspaceID, agentID string, use int64, op smartaccount.UserOp) (string, error) {
	pay, err := smartaccount.PaymentOf(op.CallData)
	if err != nil {
		return "", err
	}
	h, err := op.Hash(c.ID)
	if err != nil {
		return "", err
	}
	hash := "0x" + hex.EncodeToString(h[:])
	raw, err := json.Marshal(op)
	if err != nil {
		return "", err
	}
	head, err := c.Head(ctx)
	if err != nil {
		return "", err
	}
	fee, paymaster := tokenFee(pay)
	// op as text: under the simple protocol a []byte is sent as bytea, which is not JSON.
	tag, err := s.pool.Exec(ctx, `UPDATE agent_spend_permission_uses SET op = $4, op_hash = $5, sent_block = COALESCE(sent_block, $6)
		WHERE id = $1 AND agent_id = $2 AND workspace_id = $3 AND chain_id = $7 AND token = $8 AND counterparty = $9 AND amount = $10
		AND fee IS NOT DISTINCT FROM $11 AND paymaster IS NOT DISTINCT FROM $12 AND outcome IS NULL AND (op_hash IS NULL OR op_hash = $5)
		AND EXISTS (SELECT 1 FROM agent_smart_accounts a WHERE a.agent_id = $2 AND a.workspace_id = $3 AND lower(a.address) = lower($13))`,
		use, agentID, workspaceID, string(raw), hash, head, c.ID, smartaccount.Checksum(pay.Token), smartaccount.Checksum(pay.To),
		pay.Total().Int64(), fee, paymaster, op.Sender)
	if err != nil {
		return "", fmt.Errorf("economy: spend permission: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", fmt.Errorf("%w: the operation is not the agent's payment %d on chain %d, still to be sent", smartaccount.ErrInvalid, use, c.ID)
	}
	return hash, nil
}

// How a payment's operation ended on the chain (agent_spend_permission_uses.outcome).
const (
	outcomePaid     = "paid"     // the payment moved
	outcomeReverted = "reverted" // it did not, though its network fee may have
	outcomeDropped  = "dropped"  // its account used its nonce for another operation: it never lands
)

// SettleAgentCryptoPayments settles, from chain c, every payment sent there whose outcome is not known yet, and answers
// how many it settled: one on the chain counts what moved (a fee paid in the token through settleAgentCryptoUse), one
// that never lands counts nothing, and one still on its way is left for the next run. The leader runs it each minute.
func (s *DualTokenStore) SettleAgentCryptoPayments(ctx context.Context, c smartaccount.Chain) (int, error) {
	type sent struct {
		id                 int64
		workspaceID, agent string
		op                 smartaccount.UserOp
		block              int64
		withFee            bool
	}
	rows, err := s.pool.Query(ctx, `SELECT id, workspace_id, agent_id, op, sent_block, fee IS NOT NULL FROM agent_spend_permission_uses
		WHERE chain_id = $1 AND op IS NOT NULL AND outcome IS NULL ORDER BY id LIMIT 500`, c.ID)
	if err != nil {
		return 0, fmt.Errorf("economy: spend permission: %w", err)
	}
	pending, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (sent, error) {
		var u sent
		var raw []byte
		if err := r.Scan(&u.id, &u.workspaceID, &u.agent, &raw, &u.block, &u.withFee); err != nil {
			return u, err
		}
		return u, json.Unmarshal(raw, &u.op)
	})
	if err != nil {
		return 0, fmt.Errorf("economy: spend permission: %w", err)
	}
	settled := 0
	var errs []error
	for _, u := range pending {
		outcome, charged, left, err := settleFromChain(ctx, c, u.op, u.block, u.withFee)
		if err == nil && outcome != "" {
			err = s.settleAgentCryptoUse(ctx, u.workspaceID, u.agent, u.id, outcome, charged, left)
		}
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("payment %d: %w", u.id, err))
		case outcome != "":
			settled++
		}
	}
	return settled, errors.Join(errs...)
}

// settleFromChain reads how op, sent at block, ended on c: its outcome, and for a fee paid in the token what the
// paymaster took and what is still approved to it; no outcome while it is still on its way. Everything is read from
// c's node in finalized blocks, never taken from the bundler's word, so a reorg cannot release what moved.
func settleFromChain(ctx context.Context, c smartaccount.Chain, op smartaccount.UserOp, block int64, withFee bool) (string, *big.Int, *big.Int, error) {
	found, success, tx, err := c.Landed(ctx, op, block)
	if err != nil {
		return "", nil, nil, err
	}
	if !found {
		dropped, err := c.Dropped(ctx, op, block)
		if err != nil || !dropped {
			return "", nil, nil, err
		}
		return outcomeDropped, nil, nil, nil
	}
	outcome := outcomePaid
	if !success {
		outcome = outcomeReverted
	}
	if !withFee {
		return outcome, nil, nil, nil
	}
	charged, err := c.TokenFeeTaken(ctx, op, tx)
	if err != nil || charged == nil { // its transaction is not read yet
		return "", nil, nil, err
	}
	left, err := c.FeeApprovalLeft(ctx, op)
	if err != nil {
		return "", nil, nil, err
	}
	return outcome, charged, left, nil
}

// SettleAgentCryptoFee settles use, agentID's payment whose network fee an ERC-20 paymaster takes in the token, as paid:
// what the paymaster took for it (smartaccount.Chain.TokenFeeCharged), and left, what the account approves the
// paymaster now (Chain.FeeApprovalLeft), as settleAgentCryptoUse counts them.
func (s *DualTokenStore) SettleAgentCryptoFee(ctx context.Context, workspaceID, agentID string, use int64, charged, left *big.Int) error {
	if charged == nil || left == nil {
		return fmt.Errorf("%w: a fee charged and what is left approved are whole amounts, not negative", smartaccount.ErrInvalid)
	}
	return s.settleAgentCryptoUse(ctx, workspaceID, agentID, use, outcomePaid, charged, left)
}

// settleAgentCryptoUse records how agentID's payment use ended. Paid, it counts its amount; reverted or dropped, it
// does not. A fee an ERC-20 paymaster takes in the token counts what the paymaster took (charged) instead of the most
// it could be, once the operation is on the chain, and left, what the account approves the paymaster on the chain now,
// which the paymaster could still take. Each such payment's approval replaces the one before to the same paymaster on
// the same chain, so left is counted once, on the newest settled use of the agent's token with that paymaster on that
// chain, and an older one counts only what was taken: the rest of every quote is the allowance's again. Another
// paymaster's approval, or another chain's, is its own. What a use counts never falls on settling it again, and an
// approval left moves only to a newer use, never to an older period's.
//
// shortcut: left is read before the agent's lock is taken, so two of an agent's fees settled at once, with a third
// payment landing between, can count the older approval; upgrade by settling each agent's fees one at a time.
func (s *DualTokenStore) settleAgentCryptoUse(ctx context.Context, workspaceID, agentID string, use int64, outcome string, charged, left *big.Int) error {
	if (charged == nil) != (left == nil) || charged != nil && (charged.Sign() < 0 || !charged.IsInt64() || left.Sign() < 0 || !left.IsInt64()) {
		return fmt.Errorf("%w: a fee charged and what is left approved are whole amounts, not negative", smartaccount.ErrInvalid)
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.spendAccount(ctx, tx, workspaceID, agentID); err != nil { // locked, as the gate is
			return err
		}
		var token string
		var chainID, fee, was *int64
		var paymaster, had *string
		var amount int64
		err := tx.QueryRow(ctx, `SELECT token, chain_id, paymaster, fee, fee_charged, amount, outcome FROM agent_spend_permission_uses
			WHERE id = $1 AND agent_id = $2 AND workspace_id = $3`, use, agentID, workspaceID).
			Scan(&token, &chainID, &paymaster, &fee, &was, &amount, &had)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("%w: no payment %d of the agent's", smartaccount.ErrInvalid, use)
		case err != nil:
			return fmt.Errorf("economy: spend permission: %w", err)
		case fee == nil && charged != nil:
			return fmt.Errorf("%w: payment %d's fee is not paid in the token", smartaccount.ErrInvalid, use)
		case had != nil && *had == outcome && (was == nil && charged == nil || was != nil && charged != nil && *was == charged.Int64()):
			return nil
		case had != nil && was != nil:
			return fmt.Errorf("%w: payment %d's fee was settled at %d", smartaccount.ErrInvalid, use, *was)
		case had != nil:
			return fmt.Errorf("%w: payment %d was settled %s", smartaccount.ErrInvalid, use, *had)
		case fee != nil && outcome != outcomeDropped && charged == nil:
			return fmt.Errorf("%w: payment %d's fee is paid in the token: settle it with what the paymaster took", smartaccount.ErrInvalid, use)
		case charged != nil && charged.Int64() > *fee:
			return fmt.Errorf("%w: a fee of %s is more than the %d approved for it", smartaccount.ErrInvalid, charged, *fee)
		}
		if charged == nil { // no fee in the token, or an operation that never landed
			counts := amount
			if outcome != outcomePaid {
				counts = 0
			}
			if _, err := tx.Exec(ctx, `UPDATE agent_spend_permission_uses SET amount = $2, outcome = $3 WHERE id = $1`, use, counts, outcome); err != nil {
				return fmt.Errorf("economy: spend permission: %w", err)
			}
			return nil
		}
		var newest int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(id), 0) FROM agent_spend_permission_uses
			WHERE agent_id = $1 AND workspace_id = $2 AND token = $3 AND chain_id IS NOT DISTINCT FROM $4 AND paymaster IS NOT DISTINCT FROM $5
			AND fee_charged IS NOT NULL AND id > $6`, agentID, workspaceID, token, chainID, paymaster, use).Scan(&newest); err != nil {
			return fmt.Errorf("economy: spend permission: %w", err)
		}
		mine := left.Int64()
		if newest != 0 { // a newer use holds what is left approved
			mine = 0
			if _, err := tx.Exec(ctx, `UPDATE agent_spend_permission_uses SET amount = amount - fee_left + $2, fee_left = $2
				WHERE id = $1 AND fee_left < $2`, newest, left.Int64()); err != nil {
				return fmt.Errorf("economy: spend permission: %w", err)
			}
		} else if _, err := tx.Exec(ctx, `UPDATE agent_spend_permission_uses SET amount = amount - fee_left, fee_left = 0
			WHERE agent_id = $1 AND workspace_id = $2 AND token = $3 AND chain_id IS NOT DISTINCT FROM $4 AND paymaster IS NOT DISTINCT FROM $5
			AND fee_left > 0 AND id < $6`, agentID, workspaceID, token, chainID, paymaster, use); err != nil {
			return fmt.Errorf("economy: spend permission: %w", err)
		}
		payment := amount - *fee
		if outcome != outcomePaid {
			payment = 0
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_spend_permission_uses SET amount = $2::bigint + $3::bigint + $4::bigint, fee_charged = $3, fee_left = $4, outcome = $5
			WHERE id = $1`, use, payment, charged.Int64(), mine, outcome); err != nil {
			return fmt.Errorf("economy: spend permission: %w", err)
		}
		return nil
	})
}

// AgentFreeze is a frozen agent: when, and the account's call that takes its key out of the account's owners, which
// the person's passkey signs in a user operation on each chain the account is on.
type AgentFreeze struct {
	AgentID    string    `json:"agent_id"`
	Account    string    `json:"account"`
	FrozenAt   time.Time `json:"frozen_at"`
	RevokeCall string    `json:"revoke_call"` // 0x calldata
	// B48.15: the calls that take out each key the agent rotated from, in case a rotation is not on chain yet; on chain
	// the call for a key no longer there is refused, harmlessly, and the one for the key still there takes it out.
	PreviousKeyRevokeCalls []string `json:"previous_key_revoke_calls,omitempty"`
	// B54.9: why the calls above could not all be built. The freeze holds at Talyvor's gate regardless; the failure is
	// stored on the agent and retried (B54.25).
	RevokeCallsError string `json:"revoke_calls_error,omitempty"`
}

// FreezeAgent freezes agentID at once: the gate refuses everything for it from now on. Freezing again answers the
// same freeze. The freeze and its audit entry commit before the revoke calls are built (B54.9): a call that cannot be
// built never leaves the agent able to spend, and is answered in RevokeCallsError instead.
func (s *DualTokenStore) FreezeAgent(ctx context.Context, workspaceID, agentID, by string) (AgentFreeze, error) {
	f := AgentFreeze{AgentID: agentID}
	var acct spendAccount
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		if acct, err = s.spendAccount(ctx, tx, workspaceID, agentID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `UPDATE agent_smart_accounts SET frozen_at = COALESCE(frozen_at, now()),
			frozen_by = CASE WHEN frozen_at IS NULL THEN $3 ELSE frozen_by END
			WHERE agent_id = $1 AND workspace_id = $2 RETURNING frozen_at`, agentID, workspaceID, by).Scan(&f.FrozenAt); err != nil {
			return fmt.Errorf("economy: freeze: %w", err)
		}
		if acct.frozenAt != nil {
			return nil
		}
		return appendAudit(ctx, tx, workspaceID, "agent:"+agentID, "agent.frozen", map[string]any{"agent_id": agentID,
			"account": acct.address, "by": by})
	})
	if err != nil {
		return AgentFreeze{}, err
	}
	f.Account = acct.address
	err = f.buildRevokeCalls(acct)
	if err != nil {
		slog.Error("economy: freeze: the revoke calls were not built", slog.String("agent_id", agentID), slog.String("err", err.Error()))
	}
	f.RevokeCallsError = revokeCallsError(err)
	// B54.25: stored, so the owner sees it on the agent and RetryRevokeCalls retries it; the freeze holds either way.
	if _, err := s.pool.Exec(ctx, `UPDATE agent_smart_accounts SET revoke_calls_error = $3 WHERE agent_id = $1 AND workspace_id = $2`,
		agentID, workspaceID, f.RevokeCallsError); err != nil {
		slog.Error("economy: freeze: the revoke calls' failure was not stored", slog.String("agent_id", agentID), slog.String("err", err.Error()))
	}
	return f, nil
}

// errRotatedFromRecord: the keys an agent rotated from and their indexes in the account disagree.
var errRotatedFromRecord = errors.New("economy: freeze: the keys the agent rotated from and their indexes differ")

// revokeCallsError is what the owner reads when buildRevokeCalls failed with err, "" when it did not. Building is
// decided by the agent's record alone, so freezing again helps only once Talyvor fixes the record; RetryRevokeCalls
// clears it then (B54.25).
func revokeCallsError(err error) string {
	if err == nil {
		return ""
	}
	what := "Talyvor's record of its key is not a valid key"
	if errors.Is(err, errRotatedFromRecord) {
		what = "Talyvor's record of the keys it rotated from does not match their places in the account"
	}
	return "the agent is frozen at Talyvor, but its key could not be taken out of the account on chain: " + what +
		". Talyvor has to fix that record, and once it is fixed, freezing it again answers the calls"
}

// buildRevokeCalls fills f's calls that take acct's agent key, and each key it rotated from, out of the account.
func (f *AgentFreeze) buildRevokeCalls(acct spendAccount) error {
	call, err := smartaccount.FreezeCall(smartaccount.Account{Address: acct.address, AgentKey: acct.agentSigner, AgentIndex: acct.agentIndex})
	if err != nil {
		return err
	}
	f.RevokeCall = fmt.Sprintf("0x%x", call)
	for i, key := range acct.rotatedFrom {
		if i >= len(acct.rotatedFromIndexes) {
			return errRotatedFromRecord
		}
		prev, err := smartaccount.FreezeCall(smartaccount.Account{Address: acct.address, AgentKey: key, AgentIndex: uint64(acct.rotatedFromIndexes[i])})
		if err != nil {
			return err
		}
		f.PreviousKeyRevokeCalls = append(f.PreviousKeyRevokeCalls, fmt.Sprintf("0x%x", prev))
	}
	return nil
}
