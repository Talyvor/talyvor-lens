package economy

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	tokens "github.com/talyvor/lens/internal/money/holdings"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/passkey"
)

// agent_recovery.go — B48.15: RECOVERY, ROTATION, LOCKDOWN AND SWEEP FOR A CRYPTO AGENT'S SMART ACCOUNT.
//
// The person adds a recovery signer — an address whose key only they hold, a second device or a recovery key — with
// the passkey that owns the account. When that passkey is lost, the recovery signer signs a challenge naming the
// workspace's passkey or device key that takes its place, and Lens records the new owner: from then on only the new
// passkey signs the agent's spend permission, so one the lost passkey signed allows nothing. The agent's key is
// rotated on demand: the provider makes the next key, and from then on Talyvor has only the new key sign. A lockdown
// freezes every agent of the workspace at once (FreezeAgent). Each change answers the account's call that makes it on
// chain (internal/money/smartaccount): the person's passkey signs adding a recovery signer and a sweep, the recovery
// signer signs the recovery, and the agent's own key signs its rotation and a lockdown's freeze, so neither needs a
// device the person may have lost. "Sweep to me" answers the call that pays everything an account holds on a network
// to the person's own wallet, and an agent whose account still holds anything is not archived. Class:
// rules_approvals_statements_pots, GREEN — nothing here moves money; the person's signature on chain does.

// ErrRecoverySigner: no recovery signer, already one, or a signature that is not the recovery signer's.
var ErrRecoverySigner = errors.New("economy: recovery signer")

// OwnerChange is a change Lens recorded to the owners of an agent's smart account, and the account's call that makes
// it on each chain the account is on.
type OwnerChange struct {
	AgentID     string            `json:"agent_id"`
	Account     string            `json:"account"`
	Change      string            `json:"change"`    // recovery_signer, recovered or rotated
	Call        string            `json:"call"`      // 0x calldata the account runs
	SignedBy    string            `json:"signed_by"` // who signs its operation: person, recovery or agent
	RotatedFrom string            `json:"rotated_from,omitempty"`
	Smart       AgentSmartAccount `json:"smart_account"`
}

// ownerState is the account's owners now, locked in tx.
type ownerState struct {
	address, credentialID, deviceID, agentSigner, recovery string
	ownerKey                                               []byte
	ownerIndex, agentIndex, recoveryIndex, next            uint64
	frozen                                                 bool
}

func ownersNow(ctx context.Context, tx pgx.Tx, workspaceID, agentID string) (ownerState, error) {
	var o ownerState
	err := tx.QueryRow(ctx, `SELECT address, owner_credential_id, owner_device_id, agent_signer, recovery_address, owner_public_key,
		owner_index, agent_index, recovery_index, COALESCE(next_owner_index, CASE WHEN recovery_address = '' THEN 2 ELSE 3 END), frozen_at IS NOT NULL
		FROM agent_smart_accounts WHERE agent_id = $1 AND workspace_id = $2 FOR UPDATE`, agentID, workspaceID).
		Scan(&o.address, &o.credentialID, &o.deviceID, &o.agentSigner, &o.recovery, &o.ownerKey, &o.ownerIndex, &o.agentIndex,
			&o.recoveryIndex, &o.next, &o.frozen)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, ErrSmartAccountNotFound
	}
	if err != nil {
		return o, fmt.Errorf("economy: smart account owners: %w", err)
	}
	return o, nil
}

func (o ownerState) account() smartaccount.Account {
	return smartaccount.Account{Address: o.address, Passkey: o.ownerKey, PasskeyIndex: o.ownerIndex, AgentKey: o.agentSigner, AgentIndex: o.agentIndex}
}

// keepMadeWith keeps the owners the account was made with, before the first change overwrites them.
const keepMadeWith = `made_with_public_key = COALESCE(made_with_public_key, owner_public_key),
	made_with_agent_signer = COALESCE(made_with_agent_signer, agent_signer),
	made_with_recovery_address = COALESCE(made_with_recovery_address, recovery_address)`

func (s *DualTokenStore) ownerChange(ctx context.Context, workspaceID, agentID, change, signedBy string, call []byte, audit map[string]any) (OwnerChange, error) {
	audit["agent_id"] = agentID
	if err := s.AppendAudit(ctx, workspaceID, "agent:"+agentID, "agent.smart_account."+change, audit); err != nil {
		return OwnerChange{}, err
	}
	smart, err := s.AgentSmartAccount(ctx, workspaceID, agentID)
	if err != nil {
		return OwnerChange{}, err
	}
	return OwnerChange{AgentID: agentID, Account: smart.Address, Change: change, Call: fmt.Sprintf("0x%x", call), SignedBy: signedBy, Smart: smart}, nil
}

func recoverySignerPurpose(agentID, recovery string) string {
	return "recovery-signer:" + agentID + ":" + recovery
}

// RecoverySignerChallenge issues the challenge the passkey that owns agentID's account signs to add recovery as its
// recovery signer, and that passkey.
func (s *DualTokenStore) RecoverySignerChallenge(ctx context.Context, workspaceID, agentID, recovery string) (challenge string, credentialIDs []string, err error) {
	r := SmartAccountOwnerRequest{Recovery: recovery}
	if err := checkRecovery(&r); err != nil || r.Recovery == "" {
		return "", nil, fmt.Errorf("%w: a recovery signer is an address", smartaccount.ErrInvalid)
	}
	smart, err := s.AgentSmartAccount(ctx, workspaceID, agentID)
	if err != nil {
		return "", nil, err
	}
	if recoveryOf(smart) != "" {
		return "", nil, fmt.Errorf("%w: the account has one already", ErrRecoverySigner)
	}
	credentialIDs = []string{}
	if id := smart.Owners[0].CredentialID; id != "" {
		credentialIDs = append(credentialIDs, id)
	}
	c, err := s.newChallenge(ctx, workspaceID, recoverySignerPurpose(agentID, r.Recovery))
	return c, credentialIDs, err
}

// AddRecoverySigner adds recovery as the recovery signer of agentID's account, only with a, the assertion by the
// passkey or device key that owns the account over RecoverySignerChallenge's challenge.
func (s *DualTokenStore) AddRecoverySigner(ctx context.Context, workspaceID, agentID, by, recovery string, a *passkey.Assertion) (OwnerChange, error) {
	r := SmartAccountOwnerRequest{Recovery: recovery}
	if err := checkRecovery(&r); err != nil || r.Recovery == "" {
		return OwnerChange{}, fmt.Errorf("%w: a recovery signer is an address", smartaccount.ErrInvalid)
	}
	smart, err := s.AgentSmartAccount(ctx, workspaceID, agentID)
	if err != nil {
		return OwnerChange{}, err
	}
	if a == nil {
		return OwnerChange{}, ErrPasskeyRequired
	}
	if a.CredentialID != smart.Owners[0].CredentialID || a.DeviceID != smart.Owners[0].DeviceID {
		return OwnerChange{}, fmt.Errorf("%w: only the passkey that owns the agent's account adds its recovery signer", passkey.ErrInvalid)
	}
	if err := s.authorizeSigned(ctx, workspaceID, recoverySignerPurpose(agentID, r.Recovery), a); err != nil {
		return OwnerChange{}, err
	}
	var call []byte
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		o, err := ownersNow(ctx, tx, workspaceID, agentID)
		if err != nil {
			return err
		}
		if o.recovery != "" {
			return fmt.Errorf("%w: the account has one already", ErrRecoverySigner)
		}
		if call, err = smartaccount.AddRecoveryCall(o.account(), r.Recovery); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE agent_smart_accounts SET `+keepMadeWith+`, recovery_address = $3, recovery_index = $4,
			next_owner_index = $4 + 1 WHERE agent_id = $1 AND workspace_id = $2`, agentID, workspaceID, r.Recovery, o.next)
		return err
	})
	if err != nil {
		return OwnerChange{}, err
	}
	return s.ownerChange(ctx, workspaceID, agentID, "recovery_signer", "person", call, map[string]any{"recovery_address": r.Recovery, "by": by})
}

func recoverPurpose(agentID string, r SmartAccountOwnerRequest) string {
	return "recover:" + agentID + ":" + r.CredentialID + ":" + r.DeviceID
}

// RecoveryChallenge issues the challenge the recovery signer of agentID's account signs (personal_sign, EIP-191) to
// put the workspace's passkey or device key r names in the place of the one the person lost.
func (s *DualTokenStore) RecoveryChallenge(ctx context.Context, workspaceID, agentID string, r SmartAccountOwnerRequest) (string, error) {
	if _, err := s.ownerKey(ctx, workspaceID, r); err != nil {
		return "", err
	}
	smart, err := s.AgentSmartAccount(ctx, workspaceID, agentID)
	if err != nil {
		return "", err
	}
	if recoveryOf(smart) == "" {
		return "", fmt.Errorf("%w: the account has none: it is added with the passkey that owns it", ErrRecoverySigner)
	}
	return s.newChallenge(ctx, workspaceID, recoverPurpose(agentID, r))
}

// RecoverAgentOwner puts the passkey or device key r names in the place of the one that owns agentID's account, only
// with signature (65 bytes, hex), the account's recovery signer's personal_sign of RecoveryChallenge's challenge.
func (s *DualTokenStore) RecoverAgentOwner(ctx context.Context, workspaceID, agentID, by string, r SmartAccountOwnerRequest, challenge, signature string) (OwnerChange, error) {
	pub, err := s.ownerKey(ctx, workspaceID, r)
	if err != nil {
		return OwnerChange{}, err
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(signature, "0x"))
	if err != nil {
		return OwnerChange{}, fmt.Errorf("%w: the signature is 0x hex", ErrRecoverySigner)
	}
	var call []byte
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		o, err := ownersNow(ctx, tx, workspaceID, agentID)
		if err != nil {
			return err
		}
		if o.recovery == "" || !strings.EqualFold(smartaccount.PersonalSigner(challenge, sig), o.recovery) {
			return fmt.Errorf("%w: the challenge is not signed by the account's recovery signer", passkey.ErrInvalid)
		}
		if err := useChallenge(ctx, tx, workspaceID, recoverPurpose(agentID, r), challenge); err != nil {
			return err
		}
		if call, err = smartaccount.RecoverCall(o.account(), pub); err != nil {
			return err
		}
		next, _ := smartaccount.PasskeyOwner(pub)
		_, err = tx.Exec(ctx, `UPDATE agent_smart_accounts SET `+keepMadeWith+`, owner_credential_id = $3, owner_device_id = $4,
			owner_public_key = $5, owner_index = $6, next_owner_index = $6 + 1, recovered_at = now()
			WHERE agent_id = $1 AND workspace_id = $2`, agentID, workspaceID, r.CredentialID, r.DeviceID, next, o.next)
		return err
	})
	if err != nil {
		return OwnerChange{}, err
	}
	return s.ownerChange(ctx, workspaceID, agentID, "recovered", "recovery", call, map[string]any{"credential_id": r.CredentialID,
		"device_id": r.DeviceID, "by": by})
}

// RotateAgentSigner has the provider make agentID's next key and puts it in the place of the one signing for the
// agent now: from then on Talyvor has only the new key sign. A frozen agent is not rotated.
func (s *DualTokenStore) RotateAgentSigner(ctx context.Context, workspaceID, agentID, by string) (OwnerChange, error) {
	if s.smartAccounts == nil {
		return OwnerChange{}, ErrSmartAccountsOff
	}
	var o ownerState
	if err := s.inTx(ctx, func(tx pgx.Tx) (err error) { o, err = ownersNow(ctx, tx, workspaceID, agentID); return err }); err != nil {
		return OwnerChange{}, err
	}
	if o.frozen {
		return OwnerChange{}, smartaccount.ErrFrozen
	}
	// Named by the place it takes, so a retry is the same key.
	next, err := s.smartAccounts.signers.NewSigner(ctx, fmt.Sprintf("agent_signer:%s:%d", agentID, o.next), agentID)
	if err != nil {
		return OwnerChange{}, fmt.Errorf("economy: the agent's next key: %w", err)
	}
	next = smartaccount.Checksum(next)
	var call []byte
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		now, err := ownersNow(ctx, tx, workspaceID, agentID)
		if err != nil {
			return err
		}
		if now.frozen {
			return smartaccount.ErrFrozen
		}
		if now.agentSigner != o.agentSigner || now.next != o.next {
			return fmt.Errorf("%w: it changed while the next key was made: rotate it again", ErrSmartAccountExists)
		}
		if call, err = smartaccount.RetireCall(o.account(), next); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE agent_smart_accounts SET `+keepMadeWith+`, agent_signer = $3, agent_index = $4,
			next_owner_index = $4 + 1, rotated_at = $5 WHERE agent_id = $1 AND workspace_id = $2`, agentID, workspaceID, next, o.next, time.Now())
		return err
	})
	if err != nil {
		return OwnerChange{}, err
	}
	out, err := s.ownerChange(ctx, workspaceID, agentID, "rotated", "agent", call, map[string]any{"from": o.agentSigner, "to": next, "by": by})
	out.RotatedFrom = o.agentSigner
	return out, err
}

// LockdownAgents freezes every agent of the workspace that has a smart account at once (FreezeAgent), and answers
// each freeze with its call, which the agent's own key signs on chain (smartaccount.SignRetirementAsAgent).
func (s *DualTokenStore) LockdownAgents(ctx context.Context, workspaceID, by string) ([]AgentFreeze, error) {
	rows, err := s.pool.Query(ctx, `SELECT agent_id FROM agent_smart_accounts WHERE workspace_id = $1 ORDER BY agent_id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: lockdown: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("economy: lockdown: %w", err)
	}
	out := []AgentFreeze{}
	for _, id := range ids {
		f, err := s.FreezeAgent(ctx, workspaceID, id, by)
		if err != nil {
			return out, err
		}
		out = append(out, f)
	}
	return out, nil
}

// AgentSweep is the call that pays everything agentID's account holds on a network to the person's own wallet.
type AgentSweep struct {
	AgentID string `json:"agent_id"`
	Account string `json:"account"`
	Network string `json:"network"`
	To      string `json:"to"`
	Call    string `json:"call"` // 0x calldata, signed by the person's passkey in one tap
}

// ErrAgentWalletHeld: the agent's crypto wallet still holds something; sweep it to the person first.
var ErrAgentWalletHeld = errors.New("economy: the agent's crypto wallet still holds funds: sweep them to yourself first")

// heldOn is what agentID's account holds on network, of the tokens Talyvor lists there and the network's coin — a
// token Talyvor does not list could refuse its transfer and with it the whole sweep.
func (s *DualTokenStore) heldOn(ctx context.Context, address, network string) ([]tokens.Held, error) {
	ch := s.cryptoHoldings
	r, ok := ch.readers[network]
	if !ok || network == "solana" {
		return nil, fmt.Errorf("%w: %q is not an EVM network this server reads", smartaccount.ErrInvalid, network)
	}
	hs, err := r.Holdings(ctx, address)
	if err != nil {
		return nil, err
	}
	var out []tokens.Held
	for _, h := range hs {
		listed := h.Address == ""
		for _, t := range ch.list.Tokens {
			listed = listed || (t.Network == network && strings.EqualFold(t.Address, h.Address))
		}
		if listed && h.Amount != nil && h.Amount.Sign() > 0 {
			out = append(out, h)
		}
	}
	return out, nil
}

// AgentSweepCall answers the call that pays everything agentID's account holds on network to to, the person's own
// wallet. Nothing moves until the person's passkey signs it.
func (s *DualTokenStore) AgentSweepCall(ctx context.Context, workspaceID, agentID, network, to string) (AgentSweep, error) {
	if s.cryptoHoldings == nil {
		return AgentSweep{}, ErrSmartAccountsOff
	}
	smart, err := s.AgentSmartAccount(ctx, workspaceID, agentID)
	if err != nil {
		return AgentSweep{}, err
	}
	held, err := s.heldOn(ctx, smart.Address, network)
	if err != nil {
		return AgentSweep{}, err
	}
	call, err := smartaccount.SweepCall(smartaccount.Account{Address: smart.Address}, to, held)
	if err != nil {
		return AgentSweep{}, err
	}
	return AgentSweep{AgentID: agentID, Account: smart.Address, Network: network, To: smartaccount.Checksum(to), Call: fmt.Sprintf("0x%x", call)}, nil
}

// requireSwept refuses to archive agentID while its account holds anything on an EVM network this server reads.
func (s *DualTokenStore) requireSwept(ctx context.Context, workspaceID, agentID string) error {
	if s.cryptoHoldings == nil {
		return nil
	}
	smart, err := s.AgentSmartAccount(ctx, workspaceID, agentID)
	if errors.Is(err, ErrSmartAccountNotFound) || errors.Is(err, ErrAgentNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for network := range s.cryptoHoldings.readers {
		if network == "solana" {
			continue
		}
		held, err := s.heldOn(ctx, smart.Address, network)
		if err != nil {
			return err
		}
		if len(held) > 0 {
			return fmt.Errorf("%w (on %s)", ErrAgentWalletHeld, network)
		}
	}
	return nil
}
