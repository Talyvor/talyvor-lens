package economy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	frozenAt                                     *time.Time
}

func (s *DualTokenStore) spendAccount(ctx context.Context, q pgx.Tx, workspaceID, agentID string) (spendAccount, error) {
	var a spendAccount
	err := q.QueryRow(ctx, `SELECT address, owner_credential_id, owner_device_id, agent_signer, owner_public_key, frozen_at
		FROM agent_smart_accounts WHERE agent_id = $1 AND workspace_id = $2 FOR UPDATE`, agentID, workspaceID).
		Scan(&a.address, &a.credentialID, &a.deviceID, &a.agentSigner, &a.ownerKey, &a.frozenAt)
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
			err := tx.QueryRow(ctx, `SELECT public_key, sign_count FROM workspace_passkeys WHERE credential_id = $1 AND workspace_id = $2 FOR UPDATE`,
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
// the permission it pays under is the one its account's owner signed, and pay is inside it, counting what it already
// paid in the token this period. A payment it allows is kept, and counts against the allowance from then on.
func (s *DualTokenStore) AuthoriseAgentCryptoPayment(ctx context.Context, workspaceID, agentID string, pay smartaccount.Payment) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		acct, err := s.spendAccount(ctx, tx, workspaceID, agentID) // locked: two payments are counted one after the other
		if err != nil {
			return err
		}
		if acct.frozenAt != nil {
			return smartaccount.ErrFrozen
		}
		var canonical string
		var a passkey.Assertion
		var spki, deviceSPKI []byte
		err = tx.QueryRow(ctx, `SELECT p.canonical, p.credential_id, p.device_id, p.client_data_json, p.authenticator_data, p.signature,
			k.public_key, d.public_key FROM agent_spend_permissions p
			LEFT JOIN workspace_passkeys k ON k.credential_id = p.credential_id AND k.workspace_id = p.workspace_id
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
		if _, err := tx.Exec(ctx, `INSERT INTO agent_spend_permission_uses (agent_id, workspace_id, permission_number, token, counterparty, amount, used_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, agentID, workspaceID, t.Number, token, to, pay.Amount.Int64(), now); err != nil {
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
}

// FreezeAgent freezes agentID at once: the gate refuses everything for it from now on. Freezing again answers the
// same freeze.
func (s *DualTokenStore) FreezeAgent(ctx context.Context, workspaceID, agentID, by string) (AgentFreeze, error) {
	var f AgentFreeze
	var fresh bool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		acct, err := s.spendAccount(ctx, tx, workspaceID, agentID)
		if err != nil {
			return err
		}
		fresh = acct.frozenAt == nil
		if err := tx.QueryRow(ctx, `UPDATE agent_smart_accounts SET frozen_at = COALESCE(frozen_at, now()),
			frozen_by = CASE WHEN frozen_at IS NULL THEN $3 ELSE frozen_by END
			WHERE agent_id = $1 AND workspace_id = $2 RETURNING frozen_at`, agentID, workspaceID, by).Scan(&f.FrozenAt); err != nil {
			return fmt.Errorf("economy: freeze: %w", err)
		}
		agentOwner, err := smartaccount.AddressOwner(acct.agentSigner)
		if err != nil {
			return err
		}
		call, err := smartaccount.FreezeCall(smartaccount.Account{Address: acct.address, Owners: [][]byte{acct.ownerKey, agentOwner}})
		if err != nil {
			return err
		}
		f.AgentID, f.Account, f.RevokeCall = agentID, acct.address, fmt.Sprintf("0x%x", call)
		return nil
	})
	if err != nil {
		return AgentFreeze{}, err
	}
	if fresh {
		if err := s.AppendAudit(ctx, workspaceID, "agent:"+agentID, "agent.frozen", map[string]any{"agent_id": agentID,
			"account": f.Account, "by": by}); err != nil {
			return AgentFreeze{}, err
		}
	}
	return f, nil
}
