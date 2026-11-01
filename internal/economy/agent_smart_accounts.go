package economy

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/passkey"
)

// agent_smart_accounts.go — B48.2: EACH CRYPTO AGENT'S WALLET IS ITS OWN SMART ACCOUNT, OWNED BY THE PERSON, SIGNED FOR
// THE AGENT INSIDE THE PROVIDER'S ENCLAVE.
//
// The owner names the passkey that owns the agent's account — one of the workspace's passkeys or a phone's device key —
// and may add a recovery signer. The wallet provider makes the agent's key inside its enclave and answers its address;
// the account's address is then worked out from the owners, the same on every chain, and the account is deployed by
// its first operation there (internal/money/smartaccount). On Solana, the agent's wallet is a Solana account the same
// provider makes and holds for the same agent. Lens keeps addresses and public keys only.

var (
	// ErrSmartAccountsOff: no wallet provider holds agents' keys here.
	ErrSmartAccountsOff = errors.New("economy: agents' smart accounts are not switched on")
	// ErrOwnerSigner: the owner signer named is not one of the workspace's passkeys or live device keys.
	ErrOwnerSigner = errors.New("economy: the account's owner is one of this workspace's passkeys or device keys: name exactly one")
	// ErrSmartAccountNotFound: the agent has no smart account yet.
	ErrSmartAccountNotFound = errors.New("economy: this agent has no smart account yet")
	// ErrSmartAccountExists: the agent has a smart account with other owners; they change on chain, by the person.
	ErrSmartAccountExists = errors.New("economy: this agent already has a smart account with other owners")
)

type smartAccounts struct {
	signers  money.Signers
	provider string
	factory  smartaccount.Factory
}

// SetSmartAccounts makes each crypto agent's wallet its own smart account from factory, its key held by provider's
// signers. Nothing in cmd/lens calls it until the provider's keys are on the server: until then no agent has one.
func (s *DualTokenStore) SetSmartAccounts(signers money.Signers, provider string, factory smartaccount.Factory) {
	s.smartAccounts = &smartAccounts{signers: signers, provider: provider, factory: factory}
}

// SmartAccountOwner is one of a smart account's owner signers, at its place in the account.
type SmartAccountOwner struct {
	Index        int    `json:"index"`
	Role         string `json:"role"`                    // person, agent or recovery
	PublicKey    string `json:"public_key,omitempty"`    // the person's passkey: 0x x‖y
	CredentialID string `json:"credential_id,omitempty"` // which passkey, or
	DeviceID     string `json:"device_id,omitempty"`     // which phone's device key
	Address      string `json:"address,omitempty"`       // the agent's key, or the recovery signer
	Provider     string `json:"provider,omitempty"`      // who holds the agent's key
}

// AgentSmartAccount is an agent's smart account.
type AgentSmartAccount struct {
	AgentID        string              `json:"agent_id"`
	Address        string              `json:"address"`
	Factory        string              `json:"factory"`
	Implementation string              `json:"implementation"`
	EntryPoint     string              `json:"entry_point"`
	Owners         []SmartAccountOwner `json:"owners"`
	SolanaAddress  string              `json:"solana_address"` // the agent's Solana wallet, held by the same provider
	CreatedAt      time.Time           `json:"created_at"`
}

// SmartAccountOwnerRequest names the account's owner signers: exactly one of CredentialID and DeviceID, and a recovery
// address or "". Assertion is that same passkey's or device key's signature over SmartAccountChallenge's challenge.
type SmartAccountOwnerRequest struct {
	CredentialID string             `json:"credential_id"`
	DeviceID     string             `json:"device_id"`
	Recovery     string             `json:"recovery_address"`
	Assertion    *passkey.Assertion `json:"assertion,omitempty"`
}

// B50.25 — a session alone never names who controls an agent's wallet: the owner key it names signs a single-use
// challenge naming the agent, that key and the recovery address, so a stolen session cannot pick its own recovery owner.

// smartAccountPurpose names the challenge making agentID's smart account with r's owners is signed over.
func smartAccountPurpose(agentID string, r SmartAccountOwnerRequest) string {
	return "smart-account:" + agentID + ":" + r.CredentialID + ":" + r.DeviceID + ":" + r.Recovery
}

// checkRecovery refuses a recovery address that is not one and checksums one that is.
func checkRecovery(r *SmartAccountOwnerRequest) error {
	if r.Recovery = strings.TrimSpace(r.Recovery); r.Recovery == "" {
		return nil
	}
	if _, err := smartaccount.AddressOwner(r.Recovery); err != nil {
		return err
	}
	r.Recovery = smartaccount.Checksum(r.Recovery)
	return nil
}

// SmartAccountChallenge issues the challenge the owner key r names signs to make agentID's smart account with r's
// owners, and the passkey that may sign it.
func (s *DualTokenStore) SmartAccountChallenge(ctx context.Context, workspaceID, agentID string, r SmartAccountOwnerRequest) (challenge string, credentialIDs []string, err error) {
	if _, err := s.ownerKey(ctx, workspaceID, r); err != nil {
		return "", nil, err
	}
	if err := checkRecovery(&r); err != nil {
		return "", nil, err
	}
	credentialIDs = []string{}
	if r.CredentialID != "" {
		credentialIDs = append(credentialIDs, r.CredentialID)
	}
	c, err := s.newChallenge(ctx, workspaceID, smartAccountPurpose(agentID, r))
	return c, credentialIDs, err
}

// ownerKey is the P-256 public key of the workspace's passkey or live device key r names.
func (s *DualTokenStore) ownerKey(ctx context.Context, workspaceID string, r SmartAccountOwnerRequest) (*ecdsa.PublicKey, error) {
	var spki []byte
	var err error
	switch {
	case (r.CredentialID == "") == (r.DeviceID == ""):
		return nil, ErrOwnerSigner
	case r.CredentialID != "":
		err = s.pool.QueryRow(ctx, `SELECT public_key FROM workspace_passkeys WHERE credential_id = $1 AND workspace_id = $2 AND revoked_at IS NULL`,
			r.CredentialID, workspaceID).Scan(&spki)
	default:
		err = s.pool.QueryRow(ctx, `SELECT public_key FROM workspace_device_keys WHERE id = $1 AND workspace_id = $2 AND revoked_at IS NULL`,
			r.DeviceID, workspaceID).Scan(&spki)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOwnerSigner
	}
	if err != nil {
		return nil, fmt.Errorf("economy: smart account owner: %w", err)
	}
	pub, err := x509.ParsePKIXPublicKey(spki)
	key, ok := pub.(*ecdsa.PublicKey)
	if err != nil || !ok {
		return nil, fmt.Errorf("%w: its public key is not P-256", ErrOwnerSigner)
	}
	return key, nil
}

// CreateAgentSmartAccount makes agentID's smart account, owned by the passkey r names and signed for the agent by a key
// the provider makes in its enclave, only with that passkey's r.Assertion (B50.25). Asked again with the same owners it
// answers the account it made.
func (s *DualTokenStore) CreateAgentSmartAccount(ctx context.Context, workspaceID, agentID string, r SmartAccountOwnerRequest) (AgentSmartAccount, error) {
	if s.smartAccounts == nil {
		return AgentSmartAccount{}, ErrSmartAccountsOff
	}
	kind, _, err := s.agentWalletKind(ctx, workspaceID, agentID)
	if err != nil {
		return AgentSmartAccount{}, err
	}
	if kind == WalletKindFiat {
		return AgentSmartAccount{}, fmt.Errorf("%w: this agent is fiat; make it crypto or both first", ErrWalletKind)
	}
	var archived bool
	if err := s.pool.QueryRow(ctx, `SELECT archived_at IS NOT NULL FROM agent_accounts WHERE id = $1`, agentID).Scan(&archived); err != nil {
		return AgentSmartAccount{}, fmt.Errorf("economy: smart account: %w", err)
	}
	if archived {
		return AgentSmartAccount{}, ErrAgentArchived
	}
	r.Recovery = strings.TrimSpace(r.Recovery)
	if had, err := s.AgentSmartAccount(ctx, workspaceID, agentID); err == nil {
		if had.Owners[0].CredentialID != r.CredentialID || had.Owners[0].DeviceID != r.DeviceID || !strings.EqualFold(recoveryOf(had), r.Recovery) {
			return AgentSmartAccount{}, ErrSmartAccountExists
		}
		return had, nil
	} else if !errors.Is(err, ErrSmartAccountNotFound) {
		return AgentSmartAccount{}, err
	}
	pub, err := s.ownerKey(ctx, workspaceID, r)
	if err != nil {
		return AgentSmartAccount{}, err
	}
	sa := s.smartAccounts
	if err := checkRecovery(&r); err != nil { // refused before the provider makes a key
		return AgentSmartAccount{}, err
	}
	if a := r.Assertion; a == nil {
		return AgentSmartAccount{}, ErrPasskeyRequired
	} else if a.CredentialID != r.CredentialID || a.DeviceID != r.DeviceID {
		return AgentSmartAccount{}, fmt.Errorf("%w: sign with the passkey or device key you name as the owner", passkey.ErrInvalid)
	}
	if err := s.authorizeSigned(ctx, workspaceID, smartAccountPurpose(agentID, r), r.Assertion); err != nil {
		return AgentSmartAccount{}, err
	}
	signer, err := sa.signers.NewSigner(ctx, "agent_signer:"+agentID, agentID)
	if err != nil {
		return AgentSmartAccount{}, fmt.Errorf("economy: the agent's key: %w", err)
	}
	acct, err := smartaccount.New(sa.factory, pub, signer, r.Recovery)
	if err != nil {
		return AgentSmartAccount{}, err
	}
	solana, err := sa.signers.NewSolanaWallet(ctx, "agent_solana:"+agentID, agentID)
	if err != nil {
		return AgentSmartAccount{}, fmt.Errorf("economy: the agent's Solana wallet: %w", err)
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO agent_smart_accounts (agent_id, workspace_id, address, factory, implementation,
		owner_credential_id, owner_device_id, owner_public_key, agent_signer, provider, recovery_address, solana_address)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) ON CONFLICT (agent_id) DO NOTHING`,
		agentID, workspaceID, acct.Address, sa.factory.Address, sa.factory.Implementation, r.CredentialID, r.DeviceID,
		acct.Owners[smartaccount.OwnerPasskey], smartaccount.Checksum(signer), sa.provider, r.Recovery, solana)
	if err != nil {
		return AgentSmartAccount{}, fmt.Errorf("economy: smart account: %w", err)
	}
	if tag.RowsAffected() == 1 {
		if err := s.AppendAudit(ctx, workspaceID, "agent:"+agentID, "agent.smart_account", map[string]any{"agent_id": agentID,
			"address": acct.Address, "factory": sa.factory.Address, "credential_id": r.CredentialID, "device_id": r.DeviceID,
			"agent_signer": smartaccount.Checksum(signer), "provider": sa.provider, "recovery_address": r.Recovery,
			"solana_address": solana}); err != nil {
			return AgentSmartAccount{}, err
		}
	}
	return s.CreateAgentSmartAccount(ctx, workspaceID, agentID, r) // one made at the same time is the agent's
}

func recoveryOf(a AgentSmartAccount) string {
	if len(a.Owners) > smartaccount.OwnerRecovery {
		return a.Owners[smartaccount.OwnerRecovery].Address
	}
	return ""
}

// AgentSmartAccount is agentID's smart account.
func (s *DualTokenStore) AgentSmartAccount(ctx context.Context, workspaceID, agentID string) (AgentSmartAccount, error) {
	a := AgentSmartAccount{AgentID: agentID, EntryPoint: smartaccount.EntryPoint}
	var person, agent, recovery SmartAccountOwner
	var pub []byte
	err := s.pool.QueryRow(ctx, `SELECT address, factory, implementation, owner_credential_id, owner_device_id, owner_public_key,
		agent_signer, provider, recovery_address, solana_address, created_at FROM agent_smart_accounts WHERE agent_id = $1 AND workspace_id = $2`,
		agentID, workspaceID).Scan(&a.Address, &a.Factory, &a.Implementation, &person.CredentialID, &person.DeviceID, &pub,
		&agent.Address, &agent.Provider, &recovery.Address, &a.SolanaAddress, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, _, kerr := s.agentWalletKind(ctx, workspaceID, agentID); kerr != nil {
			return AgentSmartAccount{}, kerr
		}
		return AgentSmartAccount{}, ErrSmartAccountNotFound
	}
	if err != nil {
		return AgentSmartAccount{}, fmt.Errorf("economy: smart account: %w", err)
	}
	person.Index, person.Role, person.PublicKey = smartaccount.OwnerPasskey, "person", fmt.Sprintf("0x%x", pub)
	agent.Index, agent.Role = smartaccount.OwnerAgent, "agent"
	a.Owners = []SmartAccountOwner{person, agent}
	if recovery.Address != "" {
		recovery.Index, recovery.Role = smartaccount.OwnerRecovery, "recovery"
		a.Owners = append(a.Owners, recovery)
	}
	return a, nil
}
