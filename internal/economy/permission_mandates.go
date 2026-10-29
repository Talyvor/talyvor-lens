package economy

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/agentsig"
	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/passkey"
)

// permission_mandates.go — B40.2: WHAT A PERSON ALLOWED AN ASSISTANT OR AN AGENT TO PREPARE AND PAY, SIGNED BY THEM.
//
// A permission mandate is shaped as an AP2 intent mandate: who it is for (the principal, a person or a company),
// optionally which agent, which clients may act on it (an MCP connector's client id, "chat", "voice", an agent's key),
// what it may pay (payees, payee categories, marketplace sellers, rooms, in which currencies), its limits, the amount
// above which it still asks, what happens with a new payee, and when it is valid.
//
// PreparePermissionMandate checks the terms and returns them as canonical JSON; the person signs it with one of the
// workspace's passkeys, the WebAuthn challenge being the SHA-256 of that JSON, and CreatePermissionMandate keeps it
// only if it is byte for byte the canonical form and the assertion verifies over it. The assertion is kept: every
// evaluation checks it again, so a mandate altered after it was signed is blocked. Revocation is at once and not
// signed. EvaluatePermissionMandate is deterministic: allow, needs_approval or block, with the reason and the rule.
// Nothing here moves money; B40.3 judges each prepared payment with it. Class: rules_approvals_statements_pots, GREEN.

var (
	// ErrPermissionMandate: terms a mandate cannot have, or a signature that does not sign them.
	ErrPermissionMandate = errors.New("economy: invalid permission mandate")
	// ErrPermissionMandateNotFound: no such mandate in this workspace.
	ErrPermissionMandateNotFound = errors.New("economy: no such permission mandate")
)

// PermissionMandateVersion names the canonical form signed.
const PermissionMandateVersion = "talyvor.permission_mandate.v1"

// The clients a mandate may name besides "mcp:<client id>" and "key:<api key id>".
const (
	ClientChat  = "chat"
	ClientVoice = "voice"
)

// The new-payee policies.
const (
	NewPayeeAlwaysAsk           = "always_ask"
	NewPayeeConfirmationOfPayee = "confirmation_of_payee"
	NewPayeeCoolingOff          = "cooling_off"
)

// The decisions.
const (
	MandateAllow         = "allow"
	MandateNeedsApproval = "needs_approval"
	MandateBlock         = "block"
)

// MandatePrincipal is who the mandate speaks for.
type MandatePrincipal struct {
	Type string `json:"type"` // person or company
	ID   string `json:"id"`
	Name string `json:"name"`
}

// MandateScope is what the mandate may pay: a payment is in scope when its payee, its payee's category, its
// marketplace seller or its room is named, in one of the currencies.
type MandateScope struct {
	Payees          []string `json:"payees"`
	PayeeCategories []string `json:"payee_categories"`
	Sellers         []string `json:"sellers"`
	Rooms           []string `json:"rooms"`
	Currencies      []string `json:"currencies"`
}

// MandateLimits are in each currency's minor units; a currency with no entry has no limit of that kind.
type MandateLimits struct {
	PerPayment      map[string]int64 `json:"per_payment"`
	PerDay          map[string]int64 `json:"per_day"`
	PerMonth        map[string]int64 `json:"per_month"`
	BatchTotal      map[string]int64 `json:"batch_total"`
	PaymentsPerHour int              `json:"payments_per_hour"` // 0: no limit
}

// MandateNewPayee is what happens the first time a payee in scope is paid.
type MandateNewPayee struct {
	Policy          string `json:"policy"`
	CoolingOffHours int    `json:"cooling_off_hours"`
}

// counterpartyTrust ranks the trust levels a mandate may ask for; an unverified agent, or an unknown level, is 0.
var counterpartyTrust = map[agentsig.Level]int{agentsig.Signed: 1, agentsig.Verified: 2}

// MandateCounterparty is who the mandate pays (B45.30): an agent at least this trusted, by the trust levels of
// docs/agent-signatures.md, and/or one whose signing domain — its operator — is named. A payment whose counterparty
// is not known is an unverified agent's, with no operator.
type MandateCounterparty struct {
	MinTrust  string   `json:"min_trust,omitempty"` // "signed" or "verified"; empty: any
	Operators []string `json:"operators,omitempty"` // signing domains, lower case; empty: any
}

// PermissionMandateTerms is what the person signs, in this field order.
type PermissionMandateTerms struct {
	Version               string           `json:"version"`
	ID                    string           `json:"id"`
	WorkspaceID           string           `json:"workspace_id"`
	Principal             MandatePrincipal `json:"principal"`
	AgentID               string           `json:"agent_id"`
	Clients               []string         `json:"clients"`
	Scope                 MandateScope     `json:"scope"`
	Limits                MandateLimits    `json:"limits"`
	RequiresApprovalAbove map[string]int64 `json:"requires_approval_above"`
	NewPayee              MandateNewPayee  `json:"new_payee"`
	ValidFrom             time.Time        `json:"valid_from"`
	ExpiresAt             time.Time        `json:"expires_at"`
	// Counterparty is left out of the signed form when there is none, so mandates signed before B45.30 stay canonical.
	Counterparty *MandateCounterparty `json:"counterparty,omitempty"`
}

// PermissionMandate is a signed mandate as kept.
type PermissionMandate struct {
	PermissionMandateTerms
	Status       string     `json:"status"` // active, not_yet_valid, expired or revoked
	Canonical    string     `json:"canonical"`
	CredentialID string     `json:"credential_id"`
	DeviceID     string     `json:"device_id,omitempty"` // B40.4: set when a device key signed it, instead of a passkey
	CreatedBy    string     `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	RevokedBy    string     `json:"revoked_by,omitempty"`
}

// PreparedPayment is a payment an assistant or an agent prepared, as the evaluator sees it. The spent and count
// figures are what already went under this mandate today, this calendar month (UTC) and in the last hour.
type PreparedPayment struct {
	Client           string     `json:"client"`
	AgentID          string     `json:"agent_id"`
	PayeeID          string     `json:"payee_id"`
	PayeeCategory    string     `json:"payee_category"`
	SellerID         string     `json:"seller_id"`
	RoomID           string     `json:"room_id"`
	Currency         string     `json:"currency"`
	AmountMinor      int64      `json:"amount_minor"`
	BatchTotalMinor  int64      `json:"batch_total_minor"`
	NewPayee         bool       `json:"new_payee"`
	PayeeConfirmed   bool       `json:"payee_confirmed"`
	PayeeAddedAt     *time.Time `json:"payee_added_at,omitempty"`
	SpentTodayMinor  int64      `json:"spent_today_minor"`
	SpentMonthMinor  int64      `json:"spent_month_minor"`
	PaymentsLastHour int        `json:"payments_last_hour"`
	// Counterparty is who Talyvor takes the agent being paid to be, from its signature (B45.24); nil when unknown.
	Counterparty *agentsig.Result `json:"counterparty,omitempty"`
}

// MandateDecision is the evaluator's answer: the decision, why, and the rule of the mandate that decided it.
type MandateDecision struct {
	Decision  string `json:"decision"`
	Reason    string `json:"reason"`
	Rule      string `json:"rule"`
	MandateID string `json:"mandate_id"`
}

func sortedSet(in []string) []string {
	out := []string{}
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

func amounts(in map[string]int64) map[string]int64 {
	out := map[string]int64{}
	for c, v := range in {
		out[strings.ToUpper(strings.TrimSpace(c))] = v
	}
	return out
}

// normalise puts the terms in the one form that is signed: lists trimmed, deduplicated and sorted, currencies upper
// case, times whole seconds in UTC, empty maps and lists present.
func (t *PermissionMandateTerms) normalise() {
	t.Clients = sortedSet(t.Clients)
	t.Scope.Payees, t.Scope.PayeeCategories = sortedSet(t.Scope.Payees), sortedSet(t.Scope.PayeeCategories)
	t.Scope.Sellers, t.Scope.Rooms = sortedSet(t.Scope.Sellers), sortedSet(t.Scope.Rooms)
	cur := []string{}
	for _, c := range t.Scope.Currencies {
		cur = append(cur, strings.ToUpper(c))
	}
	t.Scope.Currencies = sortedSet(cur)
	t.Limits.PerPayment, t.Limits.PerDay = amounts(t.Limits.PerPayment), amounts(t.Limits.PerDay)
	t.Limits.PerMonth, t.Limits.BatchTotal = amounts(t.Limits.PerMonth), amounts(t.Limits.BatchTotal)
	t.RequiresApprovalAbove = amounts(t.RequiresApprovalAbove)
	t.ValidFrom, t.ExpiresAt = t.ValidFrom.UTC().Truncate(time.Second), t.ExpiresAt.UTC().Truncate(time.Second)
	if c := t.Counterparty; c != nil {
		ops := []string{}
		for _, o := range c.Operators {
			ops = append(ops, strings.ToLower(o))
		}
		c.MinTrust, c.Operators = strings.ToLower(strings.TrimSpace(c.MinTrust)), sortedSet(ops)
		if len(c.Operators) == 0 {
			c.Operators = nil
		}
		if c.MinTrust == "" && c.Operators == nil {
			t.Counterparty = nil
		}
	}
}

func (t PermissionMandateTerms) validate() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrPermissionMandate, fmt.Sprintf(format, args...))
	}
	if t.Version != PermissionMandateVersion {
		return bad("version must be %q", PermissionMandateVersion)
	}
	if t.Principal.Type != "person" && t.Principal.Type != "company" {
		return bad(`principal.type must be "person" or "company"`)
	}
	if t.Principal.ID == "" || t.Principal.Name == "" {
		return bad("principal needs an id and a name")
	}
	if len(t.Clients) == 0 {
		return bad(`name at least one client: "chat", "voice", "mcp:<client id>" or "key:<api key id>"`)
	}
	for _, c := range t.Clients {
		named := (strings.HasPrefix(c, "mcp:") || strings.HasPrefix(c, "key:")) && len(c) > 4
		if c != ClientChat && c != ClientVoice && !named {
			return bad("%q is not a client: use \"chat\", \"voice\", \"mcp:<client id>\" or \"key:<api key id>\"", c)
		}
	}
	s := t.Scope
	if len(s.Payees)+len(s.PayeeCategories)+len(s.Sellers)+len(s.Rooms) == 0 {
		return bad("the scope names no payee, payee category, seller or room")
	}
	if len(s.Currencies) == 0 {
		return bad("the scope names no currency")
	}
	for _, c := range s.Currencies {
		if _, ok := MoneyCurrencies[c]; !ok {
			return bad("%q is not a currency Talyvor holds", c)
		}
	}
	for name, m := range map[string]map[string]int64{"limits.per_payment": t.Limits.PerPayment, "limits.per_day": t.Limits.PerDay,
		"limits.per_month": t.Limits.PerMonth, "limits.batch_total": t.Limits.BatchTotal, "requires_approval_above": t.RequiresApprovalAbove} {
		for c, v := range m {
			if !slices.Contains(s.Currencies, c) {
				return bad("%s names %s, which is not in the scope's currencies", name, c)
			}
			if v <= 0 {
				return bad("%s for %s must be more than 0 (leave it out for no limit)", name, c)
			}
		}
	}
	if t.Limits.PaymentsPerHour < 0 {
		return bad("limits.payments_per_hour cannot be negative")
	}
	switch t.NewPayee.Policy {
	case NewPayeeAlwaysAsk, NewPayeeConfirmationOfPayee:
		if t.NewPayee.CoolingOffHours != 0 {
			return bad("cooling_off_hours is only for the cooling_off policy")
		}
	case NewPayeeCoolingOff:
		if t.NewPayee.CoolingOffHours <= 0 {
			return bad("the cooling_off policy needs cooling_off_hours above 0")
		}
	default:
		return bad("new_payee.policy must be always_ask, confirmation_of_payee or cooling_off")
	}
	if t.ExpiresAt.IsZero() || !t.ExpiresAt.After(t.ValidFrom) {
		return bad("expires_at must be after valid_from")
	}
	if c := t.Counterparty; c != nil {
		if _, ok := counterpartyTrust[agentsig.Level(c.MinTrust)]; c.MinTrust != "" && !ok {
			return bad(`counterparty.min_trust must be "signed" or "verified"`)
		}
		for _, o := range c.Operators {
			if strings.ContainsAny(o, "/:@ \t") || !strings.Contains(o, ".") {
				return bad("counterparty.operators names %q, which is not a domain: name the domain the agents sign with, such as agents.example.com", o)
			}
		}
	}
	return nil
}

// canonical is the JSON the person signs.
func (t PermissionMandateTerms) canonical() string {
	b, _ := json.Marshal(t)
	return string(b)
}

// MandateChallenge is the WebAuthn challenge a mandate is signed over: the SHA-256 of its canonical JSON, base64url.
func MandateChallenge(canonical string) string {
	h := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// PreparePermissionMandate checks terms for workspaceID, gives them a new id, and returns the canonical JSON to sign,
// its challenge, and the workspace's passkeys that may sign it. Nothing is kept until it is signed.
func (s *DualTokenStore) PreparePermissionMandate(ctx context.Context, workspaceID string, t PermissionMandateTerms) (canonical, challenge string, credentialIDs []string, err error) {
	t.Version, t.ID, t.WorkspaceID = PermissionMandateVersion, uuid.NewString(), workspaceID
	if t.ValidFrom.IsZero() {
		t.ValidFrom = time.Now()
	}
	t.normalise()
	if err := t.validate(); err != nil {
		return "", "", nil, err
	}
	keys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return "", "", nil, err
	}
	var devices int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_device_keys WHERE workspace_id = $1 AND revoked_at IS NULL`,
		workspaceID).Scan(&devices); err != nil {
		return "", "", nil, fmt.Errorf("economy: device keys: %w", err)
	}
	if len(keys) == 0 && devices == 0 {
		return "", "", nil, fmt.Errorf("%w: a mandate is signed with a passkey or the phone app's device key, and this workspace has neither: register one first", ErrPermissionMandate)
	}
	credentialIDs = []string{}
	for _, k := range keys {
		credentialIDs = append(credentialIDs, k.CredentialID)
	}
	c := t.canonical()
	return c, MandateChallenge(c), credentialIDs, nil
}

// parseCanonical reads a mandate's canonical JSON and refuses anything that is not exactly the canonical form of
// terms that may be signed.
func parseCanonical(canonical string) (PermissionMandateTerms, error) {
	var t PermissionMandateTerms
	dec := json.NewDecoder(strings.NewReader(canonical))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return t, fmt.Errorf("%w: the mandate is not JSON of the mandate's form: %v", ErrPermissionMandate, err)
	}
	n := t
	n.normalise()
	if n.canonical() != canonical {
		return n, fmt.Errorf("%w: the mandate is not in its canonical form, as prepare returned it", ErrPermissionMandate)
	}
	return n, n.validate()
}

// verifyMandateSignature checks a over canonical with the passkey spki: the challenge it answers must be the
// mandate's own. storedCount is the passkey's counter, or 0 when checking again an assertion already accepted.
func (s *DualTokenStore) verifyMandateSignature(canonical string, a passkey.Assertion, spki []byte, storedCount uint32) (uint32, error) {
	if s.approvalRP == nil {
		return 0, fmt.Errorf("%w: passkeys are not configured on this server", passkey.ErrInvalid)
	}
	return s.approvalRP.VerifyAssertion(a, MandateChallenge(canonical), spki, storedCount)
}

// CreatePermissionMandate keeps a mandate the person signed: canonical as PreparePermissionMandate returned it, and
// an assertion by one of the workspace's passkeys over its challenge. An altered mandate, another workspace's, one
// already kept (even if revoked) or an unsigned one is refused.
func (s *DualTokenStore) CreatePermissionMandate(ctx context.Context, workspaceID, by, canonical string, a *passkey.Assertion) (PermissionMandate, error) {
	t, err := parseCanonical(canonical)
	if err != nil {
		return PermissionMandate{}, err
	}
	if t.WorkspaceID != workspaceID {
		return PermissionMandate{}, fmt.Errorf("%w: the mandate is another workspace's", ErrPermissionMandate)
	}
	if a == nil {
		return PermissionMandate{}, fmt.Errorf("%w: the mandate is not signed: sign its challenge with a passkey", ErrPermissionMandate)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PermissionMandate{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if a.DeviceID != "" {
		// B40.4: the phone app's device key signs the mandate's own challenge; the WebAuthn fields stay empty.
		if err := signDevice(ctx, tx, workspaceID, MandateChallenge(canonical), *a); err != nil {
			return PermissionMandate{}, err
		}
		a = &passkey.Assertion{DeviceID: a.DeviceID, Signature: a.Signature}
	} else {
		var spki []byte
		var count int64
		err = tx.QueryRow(ctx, `SELECT public_key, sign_count FROM workspace_passkeys WHERE credential_id = $1 AND workspace_id = $2 FOR UPDATE`,
			a.CredentialID, workspaceID).Scan(&spki, &count)
		if errors.Is(err, pgx.ErrNoRows) {
			return PermissionMandate{}, fmt.Errorf("%w: not a passkey of this workspace", passkey.ErrInvalid)
		}
		if err != nil {
			return PermissionMandate{}, fmt.Errorf("economy: passkey: %w", err)
		}
		next, err := s.verifyMandateSignature(canonical, *a, spki, uint32(count))
		if err != nil {
			return PermissionMandate{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE workspace_passkeys SET sign_count = $2, last_used_at = now() WHERE credential_id = $1`,
			a.CredentialID, int64(next)); err != nil {
			return PermissionMandate{}, fmt.Errorf("economy: passkey: %w", err)
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO permission_mandates (id, workspace_id, agent_id, canonical, credential_id, client_data_json,
		authenticator_data, signature, created_by, device_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		t.ID, workspaceID, t.AgentID, canonical, a.CredentialID, a.ClientDataJSON, a.AuthenticatorData, a.Signature, by, a.DeviceID)
	if pe := (*pgconn.PgError)(nil); errors.As(err, &pe) && pe.Code == "23505" {
		return PermissionMandate{}, fmt.Errorf("%w: this mandate was already signed once; prepare a new one", ErrPermissionMandate)
	}
	if err != nil {
		return PermissionMandate{}, fmt.Errorf("economy: permission mandate: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PermissionMandate{}, err
	}
	return s.PermissionMandate(ctx, workspaceID, t.ID)
}

const permissionMandateCols = `id, canonical, credential_id, device_id, created_by, created_at, revoked_at, revoked_by`

func scanPermissionMandate(row pgx.Row, now time.Time) (PermissionMandate, error) {
	var m PermissionMandate
	var id string
	if err := row.Scan(&id, &m.Canonical, &m.CredentialID, &m.DeviceID, &m.CreatedBy, &m.CreatedAt, &m.RevokedAt, &m.RevokedBy); err != nil {
		return m, err
	}
	// The terms shown are the signed JSON's; a row whose JSON does not read is shown by id alone and evaluates to block.
	if err := json.Unmarshal([]byte(m.Canonical), &m.PermissionMandateTerms); err != nil {
		m.ID = id
	}
	m.Status = mandateStatus(m.PermissionMandateTerms, m.RevokedAt, now)
	return m, nil
}

func mandateStatus(t PermissionMandateTerms, revokedAt *time.Time, now time.Time) string {
	switch {
	case revokedAt != nil:
		return "revoked"
	case now.Before(t.ValidFrom):
		return "not_yet_valid"
	case !now.Before(t.ExpiresAt):
		return "expired"
	}
	return "active"
}

// PermissionMandate reads one of workspaceID's mandates.
func (s *DualTokenStore) PermissionMandate(ctx context.Context, workspaceID, id string) (PermissionMandate, error) {
	m, err := scanPermissionMandate(s.pool.QueryRow(ctx, `SELECT `+permissionMandateCols+` FROM permission_mandates
		WHERE id = $1 AND workspace_id = $2`, id, workspaceID), time.Now())
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrPermissionMandateNotFound
	}
	return m, err
}

// PermissionMandates reads workspaceID's mandates, newest first; agentID, when not "", keeps those for that agent or
// for no agent in particular.
func (s *DualTokenStore) PermissionMandates(ctx context.Context, workspaceID, agentID string) ([]PermissionMandate, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+permissionMandateCols+` FROM permission_mandates
		WHERE workspace_id = $1 AND ($2 = '' OR agent_id IN ('', $2)) ORDER BY created_at DESC, id`, workspaceID, agentID)
	if err != nil {
		return nil, fmt.Errorf("economy: permission mandates: %w", err)
	}
	defer rows.Close()
	out, now := []PermissionMandate{}, time.Now()
	for rows.Next() {
		m, err := scanPermissionMandate(rows, now)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RevokePermissionMandate revokes a mandate at once; revoking it again changes nothing.
func (s *DualTokenStore) RevokePermissionMandate(ctx context.Context, workspaceID, id, by string) (PermissionMandate, error) {
	if _, err := s.pool.Exec(ctx, `UPDATE permission_mandates SET revoked_at = now(), revoked_by = $3
		WHERE id = $1 AND workspace_id = $2 AND revoked_at IS NULL`, id, workspaceID, by); err != nil {
		return PermissionMandate{}, fmt.Errorf("economy: revoke permission mandate: %w", err)
	}
	return s.PermissionMandate(ctx, workspaceID, id)
}

// EvaluateMandatePayment judges p against the mandate id of workspaceID, checking its signature again first.
func (s *DualTokenStore) EvaluateMandatePayment(ctx context.Context, workspaceID, id string, p PreparedPayment) (MandateDecision, error) {
	var canonical, rowWS string
	var a passkey.Assertion
	var spki, deviceSPKI []byte
	var revokedAt *time.Time
	err := s.pool.QueryRow(ctx, `SELECT m.canonical, m.workspace_id, m.credential_id, m.client_data_json, m.authenticator_data, m.signature,
		m.device_id, m.revoked_at, k.public_key, d.public_key FROM permission_mandates m
		LEFT JOIN workspace_passkeys k ON k.credential_id = m.credential_id AND k.workspace_id = m.workspace_id
		LEFT JOIN workspace_device_keys d ON d.id = m.device_id AND d.workspace_id = m.workspace_id AND d.revoked_at IS NULL
		WHERE m.id = $1 AND m.workspace_id = $2`, id, workspaceID).
		Scan(&canonical, &rowWS, &a.CredentialID, &a.ClientDataJSON, &a.AuthenticatorData, &a.Signature, &a.DeviceID, &revokedAt, &spki, &deviceSPKI)
	if errors.Is(err, pgx.ErrNoRows) {
		return MandateDecision{}, ErrPermissionMandateNotFound
	}
	if err != nil {
		return MandateDecision{}, fmt.Errorf("economy: permission mandate: %w", err)
	}
	block := func(reason string) MandateDecision {
		return MandateDecision{Decision: MandateBlock, Reason: reason, Rule: "signature", MandateID: id}
	}
	t, err := parseCanonical(canonical)
	if err != nil || t.ID != id || t.WorkspaceID != rowWS {
		return block("the mandate is not what the person signed"), nil
	}
	switch {
	case a.DeviceID != "" && deviceSPKI == nil:
		return block("the device key that signed the mandate was revoked or is no longer the workspace's"), nil
	case a.DeviceID != "":
		if err := devicekey.Verify(deviceSPKI, MandateChallenge(canonical), a.Signature); err != nil {
			return block("the mandate's signature does not verify: " + err.Error()), nil
		}
	case spki == nil:
		return block("the passkey that signed the mandate is no longer the workspace's"), nil
	default:
		if _, err := s.verifyMandateSignature(canonical, a, spki, 0); err != nil {
			return block("the mandate's signature does not verify: " + err.Error()), nil
		}
	}
	return EvaluatePermissionMandate(t, revokedAt, p, time.Now()), nil
}

// EvaluatePermissionMandate is the deterministic evaluator: the same mandate, payment and time give the same answer.
// What is outside the mandate is blocked; what is inside it but over a limit, above its approval amount, to a new
// payee the policy asks about, or asked for by voice needs approval; the rest is allowed.
func EvaluatePermissionMandate(t PermissionMandateTerms, revokedAt *time.Time, p PreparedPayment, now time.Time) MandateDecision {
	d := func(decision, rule, format string, args ...any) MandateDecision {
		return MandateDecision{Decision: decision, Reason: fmt.Sprintf(format, args...), Rule: rule, MandateID: t.ID}
	}
	cur := strings.ToUpper(p.Currency)
	over := func(limits map[string]int64, v int64) (int64, bool) {
		l, ok := limits[cur]
		return l, ok && v > l
	}
	switch {
	case revokedAt != nil:
		return d(MandateBlock, "revoked_at", "the mandate was revoked at %s", revokedAt.UTC().Format(time.RFC3339))
	case now.Before(t.ValidFrom):
		return d(MandateBlock, "valid_from", "the mandate is valid only from %s", t.ValidFrom.Format(time.RFC3339))
	case !now.Before(t.ExpiresAt):
		return d(MandateBlock, "expires_at", "the mandate expired at %s", t.ExpiresAt.Format(time.RFC3339))
	case !slices.Contains(t.Clients, p.Client):
		return d(MandateBlock, "clients", "the mandate does not let %q act on it", p.Client)
	case t.AgentID != "" && p.AgentID != t.AgentID:
		return d(MandateBlock, "agent_id", "the mandate is for agent %s only", t.AgentID)
	case !slices.Contains(t.Scope.Currencies, cur):
		return d(MandateBlock, "scope.currencies", "the mandate does not pay in %q", p.Currency)
	case p.AmountMinor <= 0:
		return d(MandateBlock, "amount", "a payment must be of more than 0")
	}
	switch {
	case p.PayeeID != "" && slices.Contains(t.Scope.Payees, p.PayeeID):
	case p.PayeeCategory != "" && slices.Contains(t.Scope.PayeeCategories, p.PayeeCategory):
	case p.SellerID != "" && slices.Contains(t.Scope.Sellers, p.SellerID):
	case p.RoomID != "" && slices.Contains(t.Scope.Rooms, p.RoomID):
	default:
		return d(MandateBlock, "scope", "the payee, its category, the seller or the room is not one the mandate pays")
	}
	if c := t.Counterparty; c != nil {
		who := agentsig.Result{Level: agentsig.Unverified}
		if p.Counterparty != nil {
			who = *p.Counterparty
		}
		if c.MinTrust != "" && counterpartyTrust[who.Level] < counterpartyTrust[agentsig.Level(c.MinTrust)] {
			return d(MandateBlock, "counterparty.min_trust", "the mandate pays only %s agents, and this one is %s", c.MinTrust, cmp.Or(string(who.Level), string(agentsig.Unverified)))
		}
		if len(c.Operators) > 0 && !slices.Contains(c.Operators, strings.ToLower(who.Domain)) {
			return d(MandateBlock, "counterparty.operators", "the mandate pays only agents signed by %s, and this one is %s", strings.Join(c.Operators, ", "),
				cmp.Or(who.Domain, "not signed by any"))
		}
	}
	if p.NewPayee && t.NewPayee.Policy == NewPayeeCoolingOff {
		if p.PayeeAddedAt == nil {
			return d(MandateBlock, "new_payee.cooling_off_hours", "a new payee is paid only %d hours after it was added, and this one's time is not known", t.NewPayee.CoolingOffHours)
		}
		if until := p.PayeeAddedAt.Add(time.Duration(t.NewPayee.CoolingOffHours) * time.Hour); now.Before(until) {
			return d(MandateBlock, "new_payee.cooling_off_hours", "a new payee is paid only from %s, %d hours after it was added", until.UTC().Format(time.RFC3339), t.NewPayee.CoolingOffHours)
		}
	}
	if l, ok := over(t.Limits.PerPayment, p.AmountMinor); ok {
		return d(MandateNeedsApproval, "limits.per_payment", "%d is over the mandate's %d %s a payment", p.AmountMinor, l, cur)
	}
	if l, ok := over(t.Limits.PerDay, p.SpentTodayMinor+p.AmountMinor); ok {
		return d(MandateNeedsApproval, "limits.per_day", "with %d already today it is over the mandate's %d %s a day", p.SpentTodayMinor, l, cur)
	}
	if l, ok := over(t.Limits.PerMonth, p.SpentMonthMinor+p.AmountMinor); ok {
		return d(MandateNeedsApproval, "limits.per_month", "with %d already this month it is over the mandate's %d %s a month", p.SpentMonthMinor, l, cur)
	}
	if l, ok := over(t.Limits.BatchTotal, p.BatchTotalMinor); ok {
		return d(MandateNeedsApproval, "limits.batch_total", "the batch's %d is over the mandate's %d %s a batch", p.BatchTotalMinor, l, cur)
	}
	if n := t.Limits.PaymentsPerHour; n > 0 && p.PaymentsLastHour+1 > n {
		return d(MandateNeedsApproval, "limits.payments_per_hour", "it is over the mandate's %d payments an hour", n)
	}
	if l, ok := over(t.RequiresApprovalAbove, p.AmountMinor); ok {
		return d(MandateNeedsApproval, "requires_approval_above", "the mandate asks for approval above %d %s", l, cur)
	}
	if p.NewPayee && t.NewPayee.Policy == NewPayeeAlwaysAsk {
		return d(MandateNeedsApproval, "new_payee.policy", "the mandate asks before paying a new payee")
	}
	if p.NewPayee && t.NewPayee.Policy == NewPayeeConfirmationOfPayee && !p.PayeeConfirmed {
		return d(MandateNeedsApproval, "new_payee.policy", "a new payee whose name its bank has not confirmed needs approval")
	}
	// Until the lawyer's answer to Q66, a payment asked for by voice is never allowed without approval.
	if p.Client == ClientVoice {
		return d(MandateNeedsApproval, "clients.voice", "a payment asked for by voice always needs approval")
	}
	return d(MandateAllow, "", "inside the mandate")
}
