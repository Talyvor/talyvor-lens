package economy

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
)

// money_mandates.go — B30.19: MANDATES: A PAYEE MAY PULL MONEY WITHIN LIMITS THE OWNER SET.
//
// An agent — or its owner for it — grants a mandate on one of the agent's currency accounts to one payee: another
// Talyvor company, or an outside business by its id. It sets a maximum per pull, a maximum per calendar month (UTC)
// and an expiry. The payee pulls within it; the owner or the agent revokes it at any time, and a revoked or expired
// mandate pulls nothing (migration 0246).
//
// B55.24: an agent's own key only asks for a mandate. It is requested, and pulls nothing until the owner approves it —
// signed, once the workspace has a passkey or a device key, as an approval is — or declines it. The owner's own grant is
// signed the same way, over exactly that grant, and is active at once. Each pull is judged by the agent's rules at its
// LXC value (enforceAgentRules): its limits, payee lists, hours and unusual spend, with the owner's signed mandate
// standing in for the approval a pull over the approval amount would need.
//
// A pull is one money entry. To a Talyvor company it moves the money from the agent's account straight into the
// company's open account in the same currency, so the one entry is on both statements; it moves for
// currency_accounts. An outside business pulls with the mandate's key, shown once when the mandate is granted, and
// what it pulls moves for payments_out into the mandate's hold account and from there, as a payment out (B30.17,
// B30.129) to the saved payee the mandate names, through the account partner: screened first, with the partner's
// reference on the entry, and back to the agent's account if it fails or comes back. Both are RED: test money only
// until a clearance, asked by PostMoney. A pull never takes the agent's account below zero, and the payee's idempotency
// key makes a retried pull the same pull.

var (
	// ErrMandate: a mandate asked for with a payee, limits or an expiry it cannot have, or a pull of nothing.
	ErrMandate = errors.New("economy: invalid mandate")
	// ErrMandateNotFound: no such mandate, or not one this caller may use.
	ErrMandateNotFound = errors.New("economy: no such mandate")
	// ErrMandateRevoked: the mandate was revoked, so it pulls nothing.
	ErrMandateRevoked = errors.New("economy: the mandate was revoked")
	// ErrMandateExpired: the mandate is past its expiry, so it pulls nothing.
	ErrMandateExpired = errors.New("economy: the mandate has expired")
	// ErrMandateOverLimit: a pull over the mandate's maximum per pull or per month.
	ErrMandateOverLimit = errors.New("economy: the pull is over the mandate's limit")
	// ErrMandateShort: the agent's account does not hold the money pulled.
	ErrMandateShort = errors.New("economy: the account does not hold enough to pay this pull")
	// ErrMandatePayerPaused: the agent is paused or archived, so its mandates pull nothing.
	ErrMandatePayerPaused = errors.New("economy: the agent is paused or archived, so its mandates pull nothing")
	// ErrMandateWorkspacePaused: the payer's workspace has every agent paused (B55.23).
	ErrMandateWorkspacePaused = errors.New("economy: the payer has paused its agents, so this mandate pulls nothing until they are started again")
	// ErrMandatePayeeAccount: the payee company has no open account in the mandate's currency to be paid into.
	ErrMandatePayeeAccount = errors.New("economy: the payee has no open account in this currency to be paid into")
	// ErrMandateRequested: an agent's key asked for the mandate and its owner has not approved it (B55.24).
	ErrMandateRequested = errors.New("economy: the owner has not approved this mandate yet, so it pulls nothing")
	// ErrMandateDeclined: the owner declined the mandate an agent's key asked for.
	ErrMandateDeclined = errors.New("economy: the owner declined this mandate, so it pulls nothing")
	// ErrMandateUnsigned: the workspace signs its mandates and this grant or approval carried no signature.
	ErrMandateUnsigned = errors.New("economy: this workspace signs its mandates with a passkey or the phone app — sign this mandate's challenge (POST /v1/money/mandates/challenge)")
)

// The states of a mandate.
const (
	MandateActive    = "active"
	MandateRevoked   = "revoked"
	MandateExpired   = "expired"
	MandateRequested = "requested" // an agent's key asked; its owner has not decided (B55.24)
	MandateDeclined  = "declined"
)

// Mandate is what an agent's account allows one payee to pull, and what it has pulled this month.
type Mandate struct {
	ID                   string     `json:"id"`
	WorkspaceID          string     `json:"workspace_id"`
	AgentID              string     `json:"agent_id"`
	AccountID            string     `json:"account_id"`
	Currency             string     `json:"currency"`
	PayeeWorkspaceID     string     `json:"payee_workspace_id,omitempty"`
	PayeeBusinessID      string     `json:"payee_business_id,omitempty"`
	PayeeName            string     `json:"payee_name"`
	PayeeID              string     `json:"payee_id,omitempty"`        // an outside business's saved payee (B30.16), paid what it pulls
	HoldAccountID        string     `json:"hold_account_id,omitempty"` // an outside business's: where its pulls wait to be paid out
	MaxPerPullMinor      int64      `json:"max_per_pull_minor"`
	MaxPerMonthMinor     int64      `json:"max_per_month_minor"`
	PulledThisMonthMinor int64      `json:"pulled_this_month_minor"`
	ExpiresAt            time.Time  `json:"expires_at"`
	Status               string     `json:"status"`
	RevokedAt            *time.Time `json:"revoked_at,omitempty"`
	RevokedBy            string     `json:"revoked_by,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	ApprovedAt           *time.Time `json:"approved_at,omitempty"`
	DeclinedAt           *time.Time `json:"declined_at,omitempty"`
	DecidedBy            string     `json:"decided_by,omitempty"` // who approved or declined it
	PullKey              string     `json:"pull_key,omitempty"`   // an outside business's key, answered once, when granted
}

// MandateGrant is a mandate asked for: on AccountID, to PayeeWorkspaceID or else PayeeBusinessID.
type MandateGrant struct {
	AccountID        string    `json:"account_id"`
	PayeeWorkspaceID string    `json:"payee_workspace_id"`
	PayeeBusinessID  string    `json:"payee_business_id"`
	PayeeID          string    `json:"payee_id"` // for an outside business: the saved payee its pulls are paid out to
	PayeeName        string    `json:"payee_name"`
	MaxPerPullMinor  int64     `json:"max_per_pull_minor"`
	MaxPerMonthMinor int64     `json:"max_per_month_minor"`
	ExpiresAt        time.Time `json:"expires_at"`
}

// MandatePayee is who pulls: a Talyvor company's workspace, or an outside business by its mandate's key.
type MandatePayee struct {
	WorkspaceID string
	Key         string
}

// MandatePull is one pull: the entry that moved the money, and what the mandate has pulled this month with it.
type MandatePull struct {
	MandateID            string     `json:"mandate_id"`
	AmountMinor          int64      `json:"amount_minor"`
	Currency             string     `json:"currency"`
	PulledThisMonthMinor int64      `json:"pulled_this_month_minor"`
	Entry                MoneyEntry `json:"entry"`
	// Payment is an outside business's payment out of what it pulled, and where it stands.
	Payment *OutsidePayment `json:"payment,omitempty"`
}

// mandateMonth is the start of this calendar month, UTC: the monthly maximum counts pulls since then.
const mandateMonth = `date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`

const mandateColumns = `m.id, m.workspace_id, m.agent_id, m.account_id, m.currency, COALESCE(m.payee_workspace_id, ''),
	COALESCE(m.payee_business_id, ''), m.payee_name, COALESCE(m.payee_id, ''), COALESCE(m.hold_account_id, ''), m.max_per_pull_minor, m.max_per_month_minor,
	m.expires_at, m.revoked_at, m.revoked_by, m.created_at, m.approved_at, m.declined_at, m.decided_by,
	COALESCE((SELECT sum(p.amount_minor) FROM money_mandate_pulls p WHERE p.mandate_id = m.id AND p.pulled_at >= ` + mandateMonth + `), 0)::bigint`

func scanMandate(row pgx.Row) (Mandate, error) {
	var m Mandate
	err := row.Scan(&m.ID, &m.WorkspaceID, &m.AgentID, &m.AccountID, &m.Currency, &m.PayeeWorkspaceID, &m.PayeeBusinessID, &m.PayeeName,
		&m.PayeeID, &m.HoldAccountID, &m.MaxPerPullMinor, &m.MaxPerMonthMinor, &m.ExpiresAt, &m.RevokedAt, &m.RevokedBy, &m.CreatedAt, &m.ApprovedAt, &m.DeclinedAt, &m.DecidedBy, &m.PulledThisMonthMinor)
	switch {
	case m.RevokedAt != nil:
		m.Status = MandateRevoked
	case m.DeclinedAt != nil:
		m.Status = MandateDeclined
	case !m.ExpiresAt.After(time.Now()):
		m.Status = MandateExpired
	case m.ApprovedAt == nil:
		m.Status = MandateRequested
	default:
		m.Status = MandateActive
	}
	return m, err
}

// normalise trims what a grant is asked for and checks it.
func (g *MandateGrant) normalise(workspaceID string) error {
	g.AccountID = strings.TrimSpace(g.AccountID)
	g.PayeeWorkspaceID, g.PayeeBusinessID = strings.TrimSpace(g.PayeeWorkspaceID), strings.TrimSpace(g.PayeeBusinessID)
	g.PayeeName, g.PayeeID = strings.TrimSpace(g.PayeeName), strings.TrimSpace(g.PayeeID)
	switch {
	case (g.PayeeWorkspaceID == "") == (g.PayeeBusinessID == ""):
		return fmt.Errorf("%w: name one payee: payee_workspace_id for a Talyvor company, or payee_business_id for an outside business", ErrMandate)
	case g.PayeeBusinessID != "" && g.PayeeID == "":
		return fmt.Errorf("%w: a mandate to an outside business names the saved payee (payee_id) its pulls are paid out to — save one at /v1/money/payees first", ErrMandate)
	case g.PayeeWorkspaceID == workspaceID:
		return fmt.Errorf("%w: a mandate is for another company to pull, not this one", ErrMandate)
	case len(g.PayeeBusinessID) > 200 || len(g.PayeeName) > 200:
		return fmt.Errorf("%w: a payee's id and name are at most 200 characters", ErrMandate)
	case g.MaxPerPullMinor <= 0 || g.MaxPerMonthMinor < g.MaxPerPullMinor:
		return fmt.Errorf("%w: max_per_pull_minor is above zero and max_per_month_minor at least that", ErrMandate)
	case !g.ExpiresAt.After(time.Now()):
		return fmt.Errorf("%w: expires_at is in the future", ErrMandate)
	}
	return nil
}

// mandateGrantPurpose binds a challenge to one grant exactly as asked, so a signature for it grants no other.
func mandateGrantPurpose(g MandateGrant) string {
	h := sha256.Sum256([]byte(strings.Join([]string{g.AccountID, g.PayeeWorkspaceID, g.PayeeBusinessID, g.PayeeID, g.PayeeName,
		strconv.FormatInt(g.MaxPerPullMinor, 10), strconv.FormatInt(g.MaxPerMonthMinor, 10), g.ExpiresAt.UTC().Format(time.RFC3339Nano)}, "\x00")))
	return "mandate_grant:" + hex.EncodeToString(h[:])
}

func mandateApprovalPurpose(mandateID string) string { return "mandate_approve:" + mandateID }

// mandateChallenge issues the challenge purpose is signed over, and the workspace's passkeys that may sign it; its
// device keys sign it too.
func (s *DualTokenStore) mandateChallenge(ctx context.Context, workspaceID, purpose string) (string, []string, error) {
	keys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return "", nil, err
	}
	credentialIDs := []string{}
	for _, k := range keys {
		credentialIDs = append(credentialIDs, k.CredentialID)
	}
	c, err := s.newChallenge(ctx, workspaceID, purpose)
	return c, credentialIDs, err
}

// MandateGrantChallenge issues the challenge the owner's grant of g is signed over.
func (s *DualTokenStore) MandateGrantChallenge(ctx context.Context, workspaceID string, g MandateGrant) (string, []string, error) {
	if err := g.normalise(workspaceID); err != nil {
		return "", nil, err
	}
	return s.mandateChallenge(ctx, workspaceID, mandateGrantPurpose(g))
}

// MandateApprovalChallenge issues the challenge the owner's approval of the requested mandate mandateID is signed over.
func (s *DualTokenStore) MandateApprovalChallenge(ctx context.Context, workspaceID, mandateID string) (string, []string, error) {
	m, err := scanMandate(s.pool.QueryRow(ctx, `SELECT `+mandateColumns+` FROM money_mandates m WHERE m.id = $1 AND m.workspace_id = $2`, mandateID, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrMandateNotFound
	}
	if err != nil {
		return "", nil, fmt.Errorf("economy: mandate challenge: %w", err)
	}
	if err := m.undecided(); err != nil {
		return "", nil, err
	}
	return s.mandateChallenge(ctx, workspaceID, mandateApprovalPurpose(mandateID))
}

// undecided refuses a decision on a mandate that is not requested.
func (m Mandate) undecided() error {
	switch m.Status {
	case MandateRequested:
		return nil
	case MandateRevoked:
		return ErrMandateRevoked
	case MandateExpired:
		return ErrMandateExpired
	case MandateDeclined:
		return ErrMandateDeclined
	}
	return fmt.Errorf("%w: the mandate is already approved", ErrMandate)
}

// GrantMandate grants g on one of workspaceID's agent accounts — with agentID, the agent's own key asking for one on
// its own account: requested, it pulls nothing until the owner approves it (B55.24). Without, it is the owner's
// unsigned grant, which only a workspace with no passkey or device key makes.
func (s *DualTokenStore) GrantMandate(ctx context.Context, workspaceID, agentID string, g MandateGrant) (Mandate, error) {
	return s.grantMandate(ctx, workspaceID, agentID, "", g, nil)
}

// GrantMandateSigned is the owner's or an admin's grant, by, with a the signature over MandateGrantChallenge's
// challenge for g: active at once.
func (s *DualTokenStore) GrantMandateSigned(ctx context.Context, workspaceID, by string, g MandateGrant, a *passkey.Assertion) (Mandate, error) {
	return s.grantMandate(ctx, workspaceID, "", by, g, a)
}

func (s *DualTokenStore) grantMandate(ctx context.Context, workspaceID, agentID, by string, g MandateGrant, a *passkey.Assertion) (Mandate, error) {
	if err := g.normalise(workspaceID); err != nil {
		return Mandate{}, err
	}
	m := Mandate{ID: "mdt_" + uuid.NewString(), WorkspaceID: workspaceID, AccountID: g.AccountID, PayeeWorkspaceID: g.PayeeWorkspaceID,
		PayeeBusinessID: g.PayeeBusinessID, PayeeName: g.PayeeName, MaxPerPullMinor: g.MaxPerPullMinor, MaxPerMonthMinor: g.MaxPerMonthMinor,
		ExpiresAt: g.ExpiresAt, Status: MandateRequested}
	var status string
	err := s.pool.QueryRow(ctx, `SELECT agent_id, currency, status FROM money_accounts
		WHERE id = $1 AND workspace_id = $2 AND purpose = 'agent' AND ($3 = '' OR agent_id = $3)`,
		g.AccountID, workspaceID, agentID).Scan(&m.AgentID, &m.Currency, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Mandate{}, fmt.Errorf("%w: no agent account %q", ErrMoneyAccountNotFound, g.AccountID)
	}
	if err != nil {
		return Mandate{}, fmt.Errorf("economy: grant mandate: %w", err)
	}
	if status != MoneyOpen {
		return Mandate{}, fmt.Errorf("%w: %s is %s", ErrMoneyAccountNotOpen, g.AccountID, status)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Mandate{}, fmt.Errorf("economy: grant mandate: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var approved any // when the owner approved it: now, for the owner's own grant; never, for an agent's request
	if agentID == "" {
		if err := s.authorizeSignedTx(ctx, tx, workspaceID, mandateGrantPurpose(g), a, ErrMandateUnsigned); err != nil {
			return Mandate{}, err
		}
		approved, m.DecidedBy, m.Status = time.Now(), by, MandateActive
	}
	var keySHA, hold any
	if m.PayeeWorkspaceID != "" {
		var name string
		var open bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT name FROM workspaces WHERE id = $1), ''),
			EXISTS (SELECT 1 FROM money_accounts WHERE workspace_id = $1 AND currency = $2 AND purpose = 'company' AND status = 'open')`,
			m.PayeeWorkspaceID, m.Currency).Scan(&name, &open); err != nil {
			return Mandate{}, fmt.Errorf("economy: grant mandate: %w", err)
		}
		if !open {
			return Mandate{}, fmt.Errorf("%w (%s)", ErrMandatePayeeAccount, m.Currency)
		}
		if m.PayeeName == "" {
			m.PayeeName = cmp.Or(name, m.PayeeWorkspaceID)
		}
	} else {
		payee, err := s.PayableOutsidePayee(ctx, workspaceID, g.PayeeID)
		if err != nil {
			return Mandate{}, err
		}
		m.PayeeID = payee.ID
		if m.PayeeName == "" {
			m.PayeeName = payee.Name
		}
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return Mandate{}, fmt.Errorf("economy: grant mandate: %w", err)
		}
		m.PullKey = "mdk_" + hex.EncodeToString(raw)
		keySHA = mandateKeySHA(m.PullKey)
		m.HoldAccountID = "macc_" + uuid.NewString()
		hold = m.HoldAccountID
		if _, err := tx.Exec(ctx, `INSERT INTO money_accounts (id, workspace_id, agent_id, currency, purpose, name) VALUES ($1, $2, $3, $4, 'hold', $5)`,
			m.HoldAccountID, workspaceID, m.AgentID, m.Currency, "Pulled by "+m.PayeeName+" under mandate "+m.ID); err != nil {
			return Mandate{}, fmt.Errorf("economy: grant mandate: %w", err)
		}
	}
	if err := tx.QueryRow(ctx, `INSERT INTO money_mandates (id, workspace_id, agent_id, account_id, currency, payee_workspace_id, payee_business_id,
		payee_name, pull_key_sha256, hold_account_id, max_per_pull_minor, max_per_month_minor, expires_at, payee_id, approved_at, decided_by)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), $8, COALESCE($9::text, ''), $10, $11, $12, $13, NULLIF($14, ''), $15, $16)
		RETURNING created_at, approved_at`,
		m.ID, workspaceID, m.AgentID, m.AccountID, m.Currency, m.PayeeWorkspaceID, m.PayeeBusinessID, m.PayeeName, keySHA, hold,
		m.MaxPerPullMinor, m.MaxPerMonthMinor, m.ExpiresAt, m.PayeeID, approved, m.DecidedBy).Scan(&m.CreatedAt, &m.ApprovedAt); err != nil {
		return Mandate{}, fmt.Errorf("economy: grant mandate: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Mandate{}, fmt.Errorf("economy: grant mandate: %w", err)
	}
	return m, nil
}

func mandateKeySHA(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// PullMandate pulls amountMinor under mandate mandateID for payee, as funding (test, or "" for test; or live), once
// per idempotencyKey: a retry answers the first pull, and moves no money twice.
func (s *DualTokenStore) PullMandate(ctx context.Context, mandateID string, payee MandatePayee, amountMinor int64, funding, idempotencyKey, memo string) (MandatePull, error) {
	if funding == "" {
		funding = FundingTest
	}
	switch {
	case amountMinor <= 0:
		return MandatePull{}, fmt.Errorf("%w: a pull takes some money, not %d", ErrMandate, amountMinor)
	case strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 200:
		return MandatePull{}, fmt.Errorf("%w: a pull needs an idempotency key of up to 200 characters", ErrMandate)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The mandate's row is held until the pull commits, so two pulls at once cannot both fit under one maximum. It is
	// read after the lock, in a statement of its own: one that waited for the lock counts the pulls committed meanwhile.
	var keySHA string
	var m Mandate
	err = tx.QueryRow(ctx, `SELECT pull_key_sha256 FROM money_mandates WHERE id = $1 FOR UPDATE`, mandateID).Scan(&keySHA)
	if err == nil {
		m, err = scanMandate(tx.QueryRow(ctx, `SELECT `+mandateColumns+` FROM money_mandates m WHERE m.id = $1`, mandateID))
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return MandatePull{}, ErrMandateNotFound
	}
	if err != nil {
		return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
	}
	// Who is not the payee finds no mandate.
	if payee.Key != "" {
		if m.PayeeBusinessID == "" || subtle.ConstantTimeCompare([]byte(mandateKeySHA(payee.Key)), []byte(keySHA)) != 1 {
			return MandatePull{}, ErrMandateNotFound
		}
	} else if payee.WorkspaceID == "" || payee.WorkspaceID != m.PayeeWorkspaceID {
		return MandatePull{}, ErrMandateNotFound
	}
	pull := MandatePull{MandateID: m.ID, AmountMinor: amountMinor, Currency: m.Currency}
	entryKey := "mandate_pull:" + m.ID + ":" + idempotencyKey

	var entryID string
	var first int64
	err = tx.QueryRow(ctx, `SELECT entry_id, amount_minor FROM money_mandate_pulls WHERE mandate_id = $1 AND idempotency_key = $2`,
		m.ID, idempotencyKey).Scan(&entryID, &first)
	switch {
	case err == nil && first != amountMinor:
		return MandatePull{}, ErrIdempotencyKeyReused
	case err == nil:
		if pull.Entry, err = readMoneyEntry(ctx, tx, `e.id = $1`, entryID); err != nil {
			return MandatePull{}, err
		}
		if m.PayeeBusinessID != "" {
			p, err := scanOutsidePayment(tx.QueryRow(ctx, `SELECT `+outsidePaymentCols+` FROM money_payments WHERE workspace_id = $1 AND idempotency_key = $2`,
				m.WorkspaceID, entryKey))
			if err != nil && !errors.Is(err, ErrOutsidePaymentNotFound) {
				return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
			} else if err == nil {
				pull.Payment = &p
			}
		}
		pull.PulledThisMonthMinor = m.PulledThisMonthMinor
		return pull, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
	}

	switch {
	case m.Status == MandateRevoked:
		return MandatePull{}, ErrMandateRevoked
	case m.Status == MandateExpired:
		return MandatePull{}, ErrMandateExpired
	case m.Status == MandateDeclined:
		return MandatePull{}, ErrMandateDeclined
	case m.Status == MandateRequested:
		return MandatePull{}, ErrMandateRequested
	case amountMinor > m.MaxPerPullMinor:
		return MandatePull{}, fmt.Errorf("%w: %d is over the maximum per pull of %d %s", ErrMandateOverLimit, amountMinor, m.MaxPerPullMinor, m.Currency)
	case m.PulledThisMonthMinor+amountMinor > m.MaxPerMonthMinor:
		return MandatePull{}, fmt.Errorf("%w: %d more would take this month's pulls to %d, over the maximum per month of %d %s",
			ErrMandateOverLimit, amountMinor, m.PulledThisMonthMinor+amountMinor, m.MaxPerMonthMinor, m.Currency)
	}
	// The agent's row is held too, as every movement of its money holds it, and Pause every agent is read after the lock.
	var paused, allPaused bool
	if err := tx.QueryRow(ctx, `SELECT paused_at IS NOT NULL OR archived_at IS NOT NULL FROM agent_accounts WHERE id = $1 FOR UPDATE`,
		m.AgentID).Scan(&paused); err != nil {
		return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
	}
	if paused {
		return MandatePull{}, ErrMandatePayerPaused
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_workspace_pauses WHERE workspace_id = $1)`, m.WorkspaceID).Scan(&allPaused); err != nil {
		return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
	}
	if allPaused {
		return MandatePull{}, ErrMandateWorkspacePaused
	}
	// The account is held too, so no other movement spends the same money while this one is judged, and its balance is
	// read after the lock, as the mandate is.
	var held int64
	if _, err := tx.Exec(ctx, `SELECT 1 FROM money_accounts WHERE id = $1 FOR UPDATE`, m.AccountID); err != nil {
		return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT balance_minor FROM money_account_balances WHERE account_id = $1 AND funding = $2), 0)`,
		m.AccountID, funding).Scan(&held); err != nil {
		return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
	}
	if held < amountMinor {
		return MandatePull{}, fmt.Errorf("%w: it holds %d %s %s", ErrMandateShort, held, funding, m.Currency)
	}
	// B55.24: the agent's rules judge the pull at its LXC value, as they judge its payments out; the owner signed the
	// mandate, so a pull within it needs no approval of its own. What they let through counts towards its limits.
	if s.accountPartners == nil && m.Currency != CurrencyUSD && m.Currency != CurrencyUSDC {
		return MandatePull{}, fmt.Errorf("%w: no account partner is set to value the pull", ErrAccountNotOpened)
	}
	value, err := s.paymentValue(ctx, OutsidePayment{WorkspaceID: m.WorkspaceID, IdempotencyKey: entryKey, AmountMinor: amountMinor, Currency: m.Currency})
	if err != nil {
		return MandatePull{}, err
	}
	paid := Payee{Kind: "company", ID: m.PayeeWorkspaceID, Name: m.PayeeName}
	if m.PayeeBusinessID != "" {
		paid = Payee{Kind: "outside", ID: m.PayeeID, Name: m.PayeeName}
	}
	rules := WithAgentRequest(ctx, AgentRequest{Payment: true, Approved: true, Payee: paid,
		Memo: Money(amountMinor, m.Currency) + " pulled by " + m.PayeeName + " under mandate " + m.ID})
	if err := enforceAgentRules(rules, tx, m.WorkspaceID, m.AgentID, value, entryKey); err != nil {
		return MandatePull{}, err
	}

	e := MoneyEntry{WorkspaceID: m.WorkspaceID, Kind: "mandate_pull", IdempotencyKey: entryKey, Funding: funding, Memo: strings.TrimSpace(memo)}
	if e.Memo == "" {
		e.Memo = "Pulled by " + m.PayeeName + " under mandate " + m.ID
	}
	to := m.HoldAccountID
	var payout mandatePayout
	if m.PayeeWorkspaceID != "" {
		e.Capability, e.payeeWorkspaceID = CapabilityCurrencyAccounts, m.PayeeWorkspaceID
		err := tx.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND currency = $2 AND purpose = 'company' AND status = 'open'`,
			m.PayeeWorkspaceID, m.Currency).Scan(&to)
		if errors.Is(err, pgx.ErrNoRows) {
			return MandatePull{}, fmt.Errorf("%w (%s)", ErrMandatePayeeAccount, m.Currency)
		}
		if err != nil {
			return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
		}
	} else {
		// An outside business's pull waits in the mandate's hold account only until the partner has it: a payment out
		// of the hold account to the business's saved payee, written pending with the pull and sent once it commits.
		e.Capability = CapabilityPaymentsOut
		if payout, err = s.mandatePayout(ctx, tx, m, e, amountMinor, memo); err != nil {
			return MandatePull{}, err
		}
	}
	e.Postings = []MoneyPosting{{AccountID: m.AccountID, AmountMinor: -amountMinor}, {AccountID: to, AmountMinor: amountMinor}}
	if pull.Entry, err = postMoneyTx(ctx, tx, e, s.screener); err != nil {
		return MandatePull{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO money_mandate_pulls (mandate_id, idempotency_key, entry_id, amount_minor) VALUES ($1, $2, $3, $4)`,
		m.ID, idempotencyKey, pull.Entry.ID, amountMinor); err != nil {
		return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" {
			return MandatePull{}, fmt.Errorf("%w: %s", ErrMoneyUnbalanced, pgErr.Message)
		}
		return MandatePull{}, fmt.Errorf("economy: pull mandate: %w", err)
	}
	pull.PulledThisMonthMinor = m.PulledThisMonthMinor + amountMinor
	if payout.payment == nil {
		return pull, nil
	}
	p, dest := *payout.payment, payout.payee
	res, err := payout.partner.SendPayment(partners.WithWorkspace(ctx, m.WorkspaceID), partners.PaymentRequest{ID: p.ID, AccountRef: payout.from.partnerRef,
		Amount: partners.Money{Minor: p.AmountMinor, Currency: p.Currency}, Reference: p.Reference, Payee: partners.Payee{Name: dest.Name,
			SortCode: dest.SortCode, AccountNumber: dest.AccountNumber, IBAN: dest.IBAN, BIC: dest.BIC, RoutingNumber: dest.RoutingNumber}})
	switch {
	case errors.Is(err, partners.ErrInvalid), errors.Is(err, partners.ErrNotFound), errors.Is(err, partners.ErrIDReused):
		res = partners.Result{Status: partners.StatusFailed, Detail: err.Error()}
	case err != nil: // the partner did not answer: the money stays held, and the payment is sent again when the owner reads it
		pull.Payment = &p
		return pull, nil
	}
	if p, err = s.applyPartnerResult(ctx, p, payout.from.partnerID, res); err != nil {
		return MandatePull{}, err
	}
	pull.Payment = &p
	return pull, nil
}

// mandatePayout is the payment out of what a pull takes under m: to the business's saved payee, from the mandate's
// hold account, through the account partner.
type mandatePayout struct {
	payment *OutsidePayment
	payee   OutsidePayee
	from    payingAccount
	partner partners.AccountPartner
}

// mandatePayout prepares the payment out of a pull of amountMinor under m, whose entry is e: the payee is payable, the
// payment is screened (B30.6), and it is written pending on tx — under the pull's key, so a retried pull finds it —
// with the mandate's hold account as its hold. The business's memo is the bank reference it sees, or the mandate's id
// without one. The caller sends it once the pull commits.
func (s *DualTokenStore) mandatePayout(ctx context.Context, tx pgx.Tx, m Mandate, e MoneyEntry, amountMinor int64, memo string) (mandatePayout, error) {
	var out mandatePayout
	ref := cmp.Or(strings.Join(strings.Fields(memo), " "), "Mandate "+m.ID)
	switch {
	case len(ref) > 140:
		return out, fmt.Errorf("%w: a pull's memo is its bank reference, up to 140 characters", ErrMandate)
	case m.PayeeID == "":
		return out, fmt.Errorf("%w: the mandate names no saved payee to pay out to — grant a new one with payee_id", ErrMandate)
	case s.accountPartners == nil:
		return out, fmt.Errorf("%w: no account partner is set", ErrAccountNotOpened)
	case s.screener == nil:
		return out, fmt.Errorf("%w: no screener is set, so no money moves out", partners.ErrScreeningUnavailable)
	}
	var err error
	if out.partner, err = s.accountPartners.Account(ctx, CapabilityPaymentsOut); err != nil {
		return out, fmt.Errorf("economy: pull mandate: %w", err)
	}
	if out.from, err = s.payingAccount(ctx, m.WorkspaceID, OutsidePayment{AccountID: m.AccountID, AgentID: m.AgentID, Currency: m.Currency}); err != nil {
		return out, err
	}
	if out.payee, err = s.PayableOutsidePayee(ctx, m.WorkspaceID, m.PayeeID); err != nil {
		return out, err
	}
	if err := s.coolingOffTx(ctx, tx, out.payee, m.Currency, amountMinor); err != nil { // B41.111, B50.21, as PayOutside
		return out, err
	}
	p := OutsidePayment{ID: "mpay_" + uuid.NewString(), WorkspaceID: m.WorkspaceID, IdempotencyKey: e.IdempotencyKey, AccountID: m.AccountID,
		holdAccountID: m.HoldAccountID, AgentID: m.AgentID, PayeeID: out.payee.ID, PayeeName: out.payee.Name, AmountMinor: amountMinor,
		Currency: m.Currency, Funding: e.Funding, Reference: ref, Status: PaymentPending, Partner: out.partner.Name()}
	if err := s.screener.ScreenPayment(ctx, screening.Payment{WorkspaceID: p.WorkspaceID, ID: p.screenKey(), Capability: CapabilityPaymentsOut,
		Direction: "out", Counterparty: p.PayeeName, AmountMinor: p.AmountMinor, Currency: p.Currency, Funding: p.Funding}); err != nil {
		return out, err
	}
	if p, err = scanOutsidePayment(tx.QueryRow(ctx, `INSERT INTO money_payments (id, workspace_id, idempotency_key, account_id, hold_account_id,
		agent_id, payee_id, payee_name, amount_minor, currency, funding, reference, value_ulxc, status, partner)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 0, $13, $14) RETURNING `+outsidePaymentCols,
		p.ID, p.WorkspaceID, p.IdempotencyKey, p.AccountID, p.holdAccountID, p.AgentID, p.PayeeID, p.PayeeName, p.AmountMinor, p.Currency,
		p.Funding, p.Reference, p.Status, p.Partner)); err != nil {
		return out, fmt.Errorf("economy: pull mandate: %w", err)
	}
	out.payment = &p
	return out, nil
}

// RevokeMandate revokes workspaceID's mandate mandateID — with agentID, only that agent's — for good. Revoking it again
// answers it as it was revoked.
func (s *DualTokenStore) RevokeMandate(ctx context.Context, workspaceID, agentID, mandateID, by string) (Mandate, error) {
	m, err := scanMandate(s.pool.QueryRow(ctx, `WITH r AS (UPDATE money_mandates SET revoked_at = COALESCE(revoked_at, now()),
		revoked_by = CASE WHEN revoked_at IS NULL THEN $4 ELSE revoked_by END
		WHERE id = $1 AND workspace_id = $2 AND ($3 = '' OR agent_id = $3) RETURNING *)
		SELECT `+mandateColumns+` FROM r m`, mandateID, workspaceID, agentID, by))
	if errors.Is(err, pgx.ErrNoRows) {
		return Mandate{}, ErrMandateNotFound
	}
	if err != nil {
		return Mandate{}, fmt.Errorf("economy: revoke mandate: %w", err)
	}
	return m, nil
}

// ApproveMandate is the owner's or an admin's approval, by, of the mandate mandateID an agent's key asked for, with a
// the signature over MandateApprovalChallenge's challenge for it: from then on it pulls. Approving it again answers it.
func (s *DualTokenStore) ApproveMandate(ctx context.Context, workspaceID, mandateID, by string, a *passkey.Assertion) (Mandate, error) {
	return s.decideMandate(ctx, workspaceID, mandateID, by, true, a)
}

// DeclineMandate is the owner's or an admin's refusal, by, of the mandate mandateID an agent's key asked for: it never
// pulls. A refusal moves no money, so it is not signed. Declining it again answers it.
func (s *DualTokenStore) DeclineMandate(ctx context.Context, workspaceID, mandateID, by string) (Mandate, error) {
	return s.decideMandate(ctx, workspaceID, mandateID, by, false, nil)
}

func (s *DualTokenStore) decideMandate(ctx context.Context, workspaceID, mandateID, by string, approve bool, a *passkey.Assertion) (Mandate, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Mandate{}, fmt.Errorf("economy: decide mandate: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var locked string
	err = tx.QueryRow(ctx, `SELECT id FROM money_mandates WHERE id = $1 AND workspace_id = $2 FOR UPDATE`, mandateID, workspaceID).Scan(&locked)
	var m Mandate
	if err == nil {
		m, err = scanMandate(tx.QueryRow(ctx, `SELECT `+mandateColumns+` FROM money_mandates m WHERE m.id = $1`, mandateID))
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Mandate{}, ErrMandateNotFound
	}
	if err != nil {
		return Mandate{}, fmt.Errorf("economy: decide mandate: %w", err)
	}
	if approve && m.Status == MandateActive || !approve && m.Status == MandateDeclined {
		return m, nil
	}
	if err := m.undecided(); err != nil {
		return Mandate{}, err
	}
	set := `declined_at`
	if approve {
		if err := s.authorizeSignedTx(ctx, tx, workspaceID, mandateApprovalPurpose(mandateID), a, ErrMandateUnsigned); err != nil {
			return Mandate{}, err
		}
		set = `approved_at`
	}
	if m, err = scanMandate(tx.QueryRow(ctx, `WITH d AS (UPDATE money_mandates SET `+set+` = now(), decided_by = $2 WHERE id = $1 RETURNING *)
		SELECT `+mandateColumns+` FROM d m`, mandateID, by)); err != nil {
		return Mandate{}, fmt.Errorf("economy: decide mandate: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Mandate{}, fmt.Errorf("economy: decide mandate: %w", err)
	}
	return m, nil
}

// Mandates is what workspaceID has granted — with agentID, only that agent's — and, without, what it has been granted
// by other companies, each with what it has pulled this month: the newest first.
func (s *DualTokenStore) Mandates(ctx context.Context, workspaceID, agentID string) (granted, received []Mandate, err error) {
	read := func(where string, args ...any) ([]Mandate, error) {
		rows, err := s.pool.Query(ctx, `SELECT `+mandateColumns+` FROM money_mandates m WHERE `+where+` ORDER BY m.created_at DESC, m.id`, args...)
		if err != nil {
			return nil, fmt.Errorf("economy: mandates: %w", err)
		}
		defer rows.Close()
		out := []Mandate{}
		for rows.Next() {
			m, err := scanMandate(rows)
			if err != nil {
				return nil, fmt.Errorf("economy: mandates: %w", err)
			}
			out = append(out, m)
		}
		return out, rows.Err()
	}
	if granted, err = read(`m.workspace_id = $1 AND ($2 = '' OR m.agent_id = $2)`, workspaceID, agentID); err != nil {
		return nil, nil, err
	}
	received = []Mandate{}
	if agentID == "" {
		if received, err = read(`m.payee_workspace_id = $1`, workspaceID); err != nil {
			return nil, nil, err
		}
	}
	return granted, received, nil
}
