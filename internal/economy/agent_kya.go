package economy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/partners"
)

// agent_kya.go — B30.5: KNOW YOUR AGENT. Every agent can show a credential other platforms check: what Talyvor
// knows of it now — its name, its owner's verified name and level, what it may do with live money and what only
// with test money, and a summary of its limits. internal/kya signs it; this file is what it says and the record of
// every one issued (kya_credentials, migration 0231).
//
// A credential is revoked when what it says stops being true: freezing an agent (on its own, by its spend, or with
// every agent in its workspace), archiving it, or changing its rules — a new version of them, or a boost — revokes
// it in the same transaction, so the published revocation list has it at once. Anything else that changes what it
// says (a rename, the owner's level, a capability cleared) is found when the credential is next asked for or
// verified, and it is revoked then as superseded.

// The reasons a credential is revoked, as the revocation list gives them; one deleted with its workspace is listed as
// "deleted".
const (
	KYARevokedFrozen     = "frozen"
	KYARevokedArchived   = "archived"
	KYARevokedRules      = "rules changed"
	KYARevokedSuperseded = "superseded"
)

// KYAAgent is the agent a credential is for.
type KYAAgent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// KYAOwner is who answers for the agent: its workspace, the name its verification checks confirmed, the level they
// reach, and the level a real provider's checks reach — the Test provider's count for test money only.
type KYAOwner struct {
	WorkspaceID string            `json:"workspace_id"`
	Name        string            `json:"name,omitempty"`
	Level       VerificationLevel `json:"level"`
	LiveLevel   VerificationLevel `json:"live_level"`
}

// KYACapability is one capability and the money it takes for this agent now: "live", or "test" (test money only).
type KYACapability struct {
	Key   string `json:"capability"`
	Money string `json:"money"`
}

// KYALimits summarises the agent's rules in force, boosts included, in µLXC (1 LXC = 1,000,000 µLXC). A limit that
// is absent is not set.
type KYALimits struct {
	MaxPerRequestULXC int64  `json:"max_per_request_ulxc,omitempty"`
	HourlyLimitULXC   int64  `json:"hourly_limit_ulxc,omitempty"`
	DailyLimitULXC    int64  `json:"daily_limit_ulxc,omitempty"`
	WeeklyLimitULXC   int64  `json:"weekly_limit_ulxc,omitempty"`
	MonthlyLimitULXC  int64  `json:"monthly_limit_ulxc,omitempty"`
	ApprovalAboveULXC int64  `json:"approval_above_ulxc,omitempty"` // a person approves anything above it
	MaxCommitmentULXC int64  `json:"max_commitment_ulxc,omitempty"`
	RequestsPerMinute int64  `json:"requests_per_minute,omitempty"`
	ActiveHours       string `json:"active_hours,omitempty"` // "09:00-17:00 Europe/London"
	ModelsListed      bool   `json:"models_listed,omitempty"`
	PayeesListed      bool   `json:"payees_listed,omitempty"`
}

// AgentKYAFacts is what an agent's credential says about it, read as it stands now. Standing is why it may have
// none — "frozen" or "archived" — and "" when it may.
type AgentKYAFacts struct {
	Agent        KYAAgent        `json:"agent"`
	Owner        KYAOwner        `json:"owner"`
	Capabilities []KYACapability `json:"capabilities"`
	Limits       KYALimits       `json:"limits"`
	Standing     string          `json:"-"`
}

// KYACredentialRecord is one credential issued.
type KYACredentialRecord struct {
	ID            string     `json:"id"`
	WorkspaceID   string     `json:"workspace_id"`
	AgentID       string     `json:"agent_id"`
	Kid           string     `json:"kid"`
	PublicKey     string     `json:"-"` // base64url Ed25519 public key
	ClaimsDigest  string     `json:"-"`
	Token         string     `json:"credential"`
	IssuedAt      time.Time  `json:"issued_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	RevokedReason string     `json:"revoked_reason,omitempty"`
}

// KYARevocation is one entry on the revocation list.
type KYARevocation struct {
	ID        string    `json:"jti"`
	Reason    string    `json:"reason"`
	RevokedAt time.Time `json:"revoked_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// KYAKey is a public key a credential not yet expired was signed with.
type KYAKey struct {
	Kid       string
	PublicKey string // base64url
}

// ErrKYACredentialNotFound: no credential with that id was issued.
var ErrKYACredentialNotFound = errors.New("economy: no such Know Your Agent credential")

// AgentKYAFacts reads what agentID's credential would say now.
func (s *DualTokenStore) AgentKYAFacts(ctx context.Context, workspaceID, agentID string) (AgentKYAFacts, error) {
	return agentKYAFacts(ctx, s.pool, workspaceID, agentID)
}

func agentKYAFacts(ctx context.Context, q pgxDB, workspaceID, agentID string) (AgentKYAFacts, error) {
	f := AgentKYAFacts{Agent: KYAAgent{ID: agentID}, Owner: KYAOwner{WorkspaceID: workspaceID}, Capabilities: []KYACapability{}}
	var paused, allPaused, archived bool
	err := q.QueryRow(ctx, `SELECT a.name, a.paused_at IS NOT NULL, w.paused_at IS NOT NULL, a.archived_at IS NOT NULL
		FROM agent_accounts a LEFT JOIN agent_workspace_pauses w ON w.workspace_id = a.workspace_id
		WHERE a.id = $1 AND a.workspace_id = $2`, agentID, workspaceID).Scan(&f.Agent.Name, &paused, &allPaused, &archived)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, ErrAgentNotFound
	}
	if err != nil {
		return f, fmt.Errorf("economy: agent credential: %w", err)
	}
	switch {
	case archived:
		f.Standing = KYARevokedArchived
	case paused || allPaused:
		f.Standing = KYARevokedFrozen
	}
	if f.Owner, err = kyaOwner(ctx, q, workspaceID); err != nil {
		return f, err
	}
	for _, c := range Capabilities {
		refusal, err := capabilityLive(ctx, q, workspaceID, c)
		if err != nil {
			return f, err
		}
		money := "live"
		if refusal != nil {
			money = "test"
		}
		f.Capabilities = append(f.Capabilities, KYACapability{Key: c.Key, Money: money})
	}
	r, err := scanAgentRules(q.QueryRow(ctx, `SELECT `+agentRulesColumns+` FROM agent_rules WHERE agent_id = $1`, agentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return f, nil // no rules, so no limits
	}
	if err != nil {
		return f, fmt.Errorf("economy: agent credential: %w", err)
	}
	boosts, err := agentBoosts(ctx, q, agentID, time.Now())
	if err != nil {
		return f, err
	}
	r.applyBoosts(boosts)
	f.Limits = KYALimits{MaxPerRequestULXC: r.MaxPerRequestULXC, HourlyLimitULXC: limitOf(r.HourlyLimitULXC),
		DailyLimitULXC: r.DailyLimitULXC, WeeklyLimitULXC: limitOf(r.WeeklyLimitULXC), MonthlyLimitULXC: r.MonthlyLimitULXC,
		ApprovalAboveULXC: r.ApprovalAboveULXC, MaxCommitmentULXC: limitOf(r.MaxCommitmentULXC),
		RequestsPerMinute: limitOf(r.RequestsPerMinute), ModelsListed: len(r.AllowedModels) > 0 || len(r.AllowedProviders) > 0,
		PayeesListed: len(r.AllowedPayees) > 0}
	if r.ActiveFrom != "" {
		f.Limits.ActiveHours = r.ActiveFrom + "-" + r.ActiveUntil + " " + r.Timezone
	}
	return f, nil
}

// kyaOwner is who answers for workspaceID's agents: the levels its checks reach and the name the highest check within
// them confirmed.
func kyaOwner(ctx context.Context, q pgxDB, workspaceID string) (KYAOwner, error) {
	o := KYAOwner{WorkspaceID: workspaceID}
	checks, err := readVerificationChecks(ctx, q, workspaceID)
	if err != nil {
		return o, err
	}
	o.Level, o.LiveLevel = reachedLevels(checks)
	named := VerificationLevel(0)
	for _, c := range checks {
		if c.Status == string(partners.StatusCompleted) && c.VerifiedName != "" && c.Level <= o.Level && c.Level > named {
			o.Name, named = c.VerifiedName, c.Level
		}
	}
	return o, nil
}

const kyaColumns = `id, workspace_id, agent_id, kid, public_key, claims_digest, token, issued_at, expires_at, revoked_at, revoked_reason`

func scanKYACredential(row pgx.Row) (KYACredentialRecord, error) {
	var c KYACredentialRecord
	err := row.Scan(&c.ID, &c.WorkspaceID, &c.AgentID, &c.Kid, &c.PublicKey, &c.ClaimsDigest, &c.Token, &c.IssuedAt, &c.ExpiresAt,
		&c.RevokedAt, &c.RevokedReason)
	return c, err
}

// LatestKYACredential is agentID's newest credential that is neither revoked nor expired at `at`; ok is false when it
// has none.
func (s *DualTokenStore) LatestKYACredential(ctx context.Context, workspaceID, agentID string, at time.Time) (KYACredentialRecord, bool, error) {
	c, err := scanKYACredential(s.pool.QueryRow(ctx, `SELECT `+kyaColumns+` FROM kya_credentials
		WHERE workspace_id = $1 AND agent_id = $2 AND revoked_at IS NULL AND expires_at > $3 ORDER BY issued_at DESC LIMIT 1`,
		workspaceID, agentID, at))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, fmt.Errorf("economy: agent credential: %w", err)
	}
	return c, true, nil
}

// KYACredential is the credential issued as id.
func (s *DualTokenStore) KYACredential(ctx context.Context, id string) (KYACredentialRecord, error) {
	c, err := scanKYACredential(s.pool.QueryRow(ctx, `SELECT `+kyaColumns+` FROM kya_credentials WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrKYACredentialNotFound
	}
	if err != nil {
		return c, fmt.Errorf("economy: agent credential: %w", err)
	}
	return c, nil
}

// IssueKYACredential gives agentID a new credential: under the agent's Know Your Agent lock, which every revocation
// takes too, it reads what the credential says, has sign make it, revokes as superseded the one the agent held, and
// records it. So a freeze, an archive or a rule change either comes before — and the credential says so, or is not
// issued — or after, and revokes it. A frozen or archived agent is given none: facts.Standing says why.
func (s *DualTokenStore) IssueKYACredential(ctx context.Context, workspaceID, agentID string,
	sign func(AgentKYAFacts) (KYACredentialRecord, error)) (KYACredentialRecord, AgentKYAFacts, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return KYACredentialRecord{}, AgentKYAFacts{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgentKYA(ctx, tx, agentID); err != nil {
		return KYACredentialRecord{}, AgentKYAFacts{}, err
	}
	facts, err := agentKYAFacts(ctx, tx, workspaceID, agentID)
	if err != nil || facts.Standing != "" {
		return KYACredentialRecord{}, facts, err
	}
	c, err := sign(facts)
	if err != nil {
		return c, facts, err
	}
	if err := revokeAgentKYA(ctx, tx, agentID, KYARevokedSuperseded); err != nil {
		return c, facts, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO kya_credentials (id, workspace_id, agent_id, kid, public_key, claims_digest, token,
		issued_at, expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, c.ID, workspaceID, agentID, c.Kid, c.PublicKey,
		c.ClaimsDigest, c.Token, c.IssuedAt, c.ExpiresAt); err != nil {
		return c, facts, fmt.Errorf("economy: record agent credential: %w", err)
	}
	return c, facts, tx.Commit(ctx)
}

// lockAgentKYA takes agentID's Know Your Agent lock until q's transaction ends. Issuing a credential holds it while it
// reads what the credential says and records it, and every revocation takes it, so neither misses the other.
func lockAgentKYA(ctx context.Context, q pgxDB, agentID string) error {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('kya'), hashtext($1))`, agentID); err != nil {
		return fmt.Errorf("economy: agent credential lock: %w", err)
	}
	return nil
}

// RevokeKYACredential revokes credential id for reason, unless it is revoked already.
func (s *DualTokenStore) RevokeKYACredential(ctx context.Context, id, reason string) error {
	if _, err := s.pool.Exec(ctx, `UPDATE kya_credentials SET revoked_at = now(), revoked_reason = $2
		WHERE id = $1 AND revoked_at IS NULL`, id, reason); err != nil {
		return fmt.Errorf("economy: revoke agent credential: %w", err)
	}
	return nil
}

// revokeAgentKYA revokes for reason every credential of agentID in force, in q: the transaction that froze, archived
// or changed the rules of the agent.
func revokeAgentKYA(ctx context.Context, q pgxDB, agentID, reason string) error {
	if err := lockAgentKYA(ctx, q, agentID); err != nil {
		return err
	}
	if _, err := q.Exec(ctx, `UPDATE kya_credentials SET revoked_at = now(), revoked_reason = $2
		WHERE agent_id = $1 AND revoked_at IS NULL AND expires_at > now()`, agentID, reason); err != nil {
		return fmt.Errorf("economy: revoke agent credential: %w", err)
	}
	return nil
}

// revokeWorkspaceKYA revokes for reason the credential in force of every agent in workspaceID, in q.
func revokeWorkspaceKYA(ctx context.Context, q pgxDB, workspaceID, reason string) error {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('kya'), hashtext(id)) FROM agent_accounts WHERE workspace_id = $1`,
		workspaceID); err != nil {
		return fmt.Errorf("economy: agent credential lock: %w", err)
	}
	if _, err := q.Exec(ctx, `UPDATE kya_credentials SET revoked_at = now(), revoked_reason = $2
		WHERE workspace_id = $1 AND revoked_at IS NULL AND expires_at > now()`, workspaceID, reason); err != nil {
		return fmt.Errorf("economy: revoke agent credentials: %w", err)
	}
	return nil
}

// KYARevocations is the revocation list: every credential revoked that has not yet expired at `at` — those deleted with
// their workspace too, as "deleted" — newest first.
func (s *DualTokenStore) KYARevocations(ctx context.Context, at time.Time) ([]KYARevocation, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, revoked_reason, revoked_at, expires_at FROM kya_credentials
		WHERE revoked_at IS NOT NULL AND expires_at > $1
		UNION ALL SELECT id, reason, revoked_at, expires_at FROM kya_revocation_tombstones WHERE expires_at > $1
		ORDER BY revoked_at DESC, id`, at)
	if err != nil {
		return nil, fmt.Errorf("economy: agent credential revocations: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (KYARevocation, error) {
		var r KYARevocation
		return r, row.Scan(&r.ID, &r.Reason, &r.RevokedAt, &r.ExpiresAt)
	})
}

// KYAKeys are the keys credentials not yet expired at `at` were signed with.
func (s *DualTokenStore) KYAKeys(ctx context.Context, at time.Time) ([]KYAKey, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT kid, public_key FROM kya_credentials WHERE expires_at > $1 ORDER BY kid`, at)
	if err != nil {
		return nil, fmt.Errorf("economy: agent credential keys: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (KYAKey, error) {
		var k KYAKey
		return k, row.Scan(&k.Kid, &k.PublicKey)
	})
}
