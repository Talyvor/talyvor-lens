package screening

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/operatoraudit"
	"github.com/talyvor/lens/internal/partners"
)

// The statuses of a compliance case.
const (
	CaseBlocked  = "blocked"  // an exact match: the money does not move
	CaseHeld     = "held"     // a close match: the money waits for an operator
	CaseReleased = "released" // an operator found the close match is not them: the money may move
	CaseRefused  = "refused"  // an operator found it is: the money does not move
)

// OutcomeStale is a payment's case opened because the sanctions lists were stale when it was screened (B37.4): held
// as a close match is, for an operator to release or refuse.
const OutcomeStale = "stale"

// What a case is about.
const (
	SubjectPayee   = "payee"
	SubjectPayment = "payment"
)

var (
	// ErrBlocked: an exact match on a sanctions list.
	ErrBlocked = errors.New("screening: refused — the name is on a sanctions list")
	// ErrHeld: a close match, waiting for an operator.
	ErrHeld = errors.New("screening: held — the name is close to one on a sanctions list, and waits for an operator to release it")
	// ErrListsStale: a payment held because the sanctions lists were stale, waiting for an operator.
	ErrListsStale = errors.New("screening: held — the sanctions lists are out of date, and the payment waits for an operator to release it")
	// ErrRefused: a close match an operator refused.
	ErrRefused = errors.New("screening: refused — an operator found the name is the one on the sanctions list")
	// ErrNotWhatWasReleased: a payment retried under a released case's id, but not the payment the operator released.
	ErrNotWhatWasReleased = errors.New("screening: refused — the operator released a different payment under this id; " +
		"a new payment needs a new idempotency key, and is screened again")
	// ErrCaseNotFound: no compliance case by that id.
	ErrCaseNotFound = errors.New("screening: no such compliance case")
	// ErrCaseDecided: a case that is not held is not released or refused.
	ErrCaseDecided = errors.New("screening: only a held case is released or refused")
)

// Refusal is a screening that stops the money: the case that says why. It is ErrBlocked, ErrHeld, ErrListsStale,
// ErrRefused or ErrNotWhatWasReleased.
type Refusal struct {
	Case Case
	err  error
}

func (r *Refusal) Error() string { return fmt.Sprintf("%v (compliance case %s)", r.err, r.Case.ID) }
func (r *Refusal) Unwrap() error { return r.err }

// Case is one compliance case a screening opened.
type Case struct {
	ID           string                    `json:"id"`
	WorkspaceID  string                    `json:"workspace_id"`
	Kind         string                    `json:"kind"`
	SubjectKind  string                    `json:"subject_kind"`
	SubjectID    string                    `json:"subject_id"`
	Name         string                    `json:"name"`
	Outcome      string                    `json:"outcome"`
	Status       string                    `json:"status"`
	Matches      []partners.ScreeningMatch `json:"matches"`
	Provider     string                    `json:"provider"`
	Capability   string                    `json:"capability,omitempty"`
	Direction    string                    `json:"direction,omitempty"`
	AmountMinor  *int64                    `json:"amount_minor,omitempty"`
	Currency     string                    `json:"currency,omitempty"`
	Funding      string                    `json:"funding,omitempty"`
	OpenedAt     time.Time                 `json:"opened_at"`
	DecidedBy    string                    `json:"decided_by,omitempty"`
	DecidedAt    *time.Time                `json:"decided_at,omitempty"`
	DecisionNote string                    `json:"decision_note,omitempty"`
}

const caseCols = `id, workspace_id, kind, subject_kind, subject_id, name, outcome, status, matches, provider, capability, direction,
	amount_minor, currency, funding, opened_at, decided_by, decided_at, decision_note`

func scanCase(row pgx.Row) (Case, error) {
	var c Case
	var matches []byte
	err := row.Scan(&c.ID, &c.WorkspaceID, &c.Kind, &c.SubjectKind, &c.SubjectID, &c.Name, &c.Outcome, &c.Status, &matches, &c.Provider,
		&c.Capability, &c.Direction, &c.AmountMinor, &c.Currency, &c.Funding, &c.OpenedAt, &c.DecidedBy, &c.DecidedAt, &c.DecisionNote)
	if err != nil {
		return Case{}, err
	}
	if err := json.Unmarshal(matches, &c.Matches); err != nil {
		return Case{}, err
	}
	return c, nil
}

// Providers hands out the screening provider for a wallet capability: *partners.Registry.
type Providers interface {
	Screening(ctx context.Context, capability string) (partners.ScreeningProvider, error)
}

// Screener screens payees and outside payments through the provider for their capability, and opens a compliance case
// for each match.
type Screener struct {
	pool      *pgxpool.Pool
	providers Providers
	lists     *Store // nil: a payment that clears is never held for stale lists
}

// NewScreener keeps cases in pool and asks providers.
func NewScreener(pool *pgxpool.Pool, providers Providers) *Screener {
	return &Screener{pool: pool, providers: providers}
}

// HoldWhenListsStale holds every payment in or out that would clear while lists says it is stale (B37.4).
func (s *Screener) HoldWhenListsStale(lists *Store) { s.lists = lists }

// Payment is an outside payment to screen: money in from Counterparty, or out to it.
type Payment struct {
	WorkspaceID  string
	ID           string // the payment's own id: a retry is the same payment, and reads the same case
	Capability   string // the wallet capability it moves money for
	Direction    string // in or out
	Counterparty string // the payer of money in, the payee of money out
	AmountMinor  int64
	Currency     string
	Funding      string // test or live
}

// ScreenPayment screens p's counterparty before any money moves. It answers nil when the money may move, a *Refusal
// when a case stops it — blocked, held or refused — and partners.ErrScreeningUnavailable when it could not screen,
// on which nothing may move either.
func (s *Screener) ScreenPayment(ctx context.Context, p Payment) error {
	if p.Direction != "in" && p.Direction != "out" {
		return fmt.Errorf("screening: a payment is in or out, not %q", p.Direction)
	}
	amount := p.AmountMinor
	c := Case{WorkspaceID: p.WorkspaceID, SubjectKind: SubjectPayment, SubjectID: p.ID, Name: strings.TrimSpace(p.Counterparty),
		Capability: p.Capability, Direction: p.Direction, AmountMinor: &amount, Currency: p.Currency, Funding: p.Funding}
	return s.screen(ctx, c, func(prov partners.ScreeningProvider) (partners.Screening, error) {
		req := partners.PaymentScreen{ID: p.ID, Amount: partners.Money{Minor: p.AmountMinor, Currency: p.Currency}}
		if p.Direction == "in" {
			req.Payer = c.Name
		} else {
			req.Payee = c.Name
		}
		return prov.ScreenPayment(partners.WithWorkspace(ctx, c.WorkspaceID), req)
	})
}

// ScreenPayee screens a payee when it is created, by its name, for the capability it will be paid through. It answers
// as ScreenPayment does.
func (s *Screener) ScreenPayee(ctx context.Context, workspaceID, payeeID, name, capability string) error {
	c := Case{WorkspaceID: workspaceID, SubjectKind: SubjectPayee, SubjectID: payeeID, Name: strings.TrimSpace(name), Capability: capability}
	return s.screen(ctx, c, func(prov partners.ScreeningProvider) (partners.Screening, error) {
		return prov.ScreenName(partners.WithWorkspace(ctx, c.WorkspaceID), partners.NameScreen{Name: c.Name})
	})
}

func (s *Screener) screen(ctx context.Context, c Case, ask func(partners.ScreeningProvider) (partners.Screening, error)) error {
	switch {
	case c.WorkspaceID == "" || c.SubjectID == "":
		return fmt.Errorf("screening: a screening names its workspace and what it screens")
	case c.Name == "":
		return fmt.Errorf("screening: a %s is screened by name, and this one has none", c.SubjectKind)
	}
	// A subject already screened under this name reads its case: a held one stays held until it is decided, and a
	// released one moves — when it is the very payment the operator released.
	prior, err := s.caseFor(ctx, c)
	switch {
	case err == nil:
		if prior.Status == CaseReleased && !sameMoney(prior, c) {
			return &Refusal{Case: prior, err: ErrNotWhatWasReleased}
		}
		return refusalFor(prior)
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: %v", partners.ErrScreeningUnavailable, err)
	}
	prov, err := s.providers.Screening(ctx, c.Capability)
	if err != nil {
		return fmt.Errorf("%w: %v", partners.ErrScreeningUnavailable, err)
	}
	res, err := ask(prov)
	if err != nil {
		if errors.Is(err, partners.ErrScreeningUnavailable) {
			return err
		}
		return fmt.Errorf("%w: %v", partners.ErrScreeningUnavailable, err)
	}
	switch res.Outcome {
	case partners.ScreenClear:
		if c.SubjectKind != SubjectPayment || s.lists == nil {
			return nil
		}
		_, stale, err := s.lists.ListsAge(ctx)
		if err != nil {
			return fmt.Errorf("%w: %v", partners.ErrScreeningUnavailable, err)
		}
		if !stale {
			return nil
		}
		c.Outcome, c.Status = OutcomeStale, CaseHeld
	case partners.ScreenHit:
		c.Outcome, c.Status = partners.ScreenHit, CaseBlocked
	case partners.ScreenReview:
		c.Outcome, c.Status = partners.ScreenReview, CaseHeld
	default:
		return fmt.Errorf("%w: the provider answered %q", partners.ErrScreeningUnavailable, res.Outcome)
	}
	matches, err := json.Marshal(res.Matches)
	if err != nil {
		return err
	}
	c.ID, c.Kind, c.Provider = "cc_"+uuid.NewString(), "screening", prov.Name()
	// Two attempts at once open one case: the second reads the first's.
	opened, err := scanCase(s.pool.QueryRow(ctx, `WITH ins AS (
			INSERT INTO compliance_cases (id, workspace_id, kind, subject_kind, subject_id, name, outcome, status, matches, provider,
				capability, direction, amount_minor, currency, funding)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			ON CONFLICT (workspace_id, subject_kind, subject_id, name, direction, currency) WHERE kind = 'screening' DO NOTHING RETURNING `+caseCols+`)
		SELECT `+caseCols+` FROM ins
		UNION ALL
		SELECT `+caseCols+` FROM compliance_cases WHERE workspace_id = $2 AND subject_kind = $4 AND subject_id = $5 AND name = $6
			AND direction = $12 AND currency = $14 AND NOT EXISTS (SELECT 1 FROM ins)`,
		c.ID, c.WorkspaceID, c.Kind, c.SubjectKind, c.SubjectID, c.Name, c.Outcome, c.Status, string(matches), c.Provider,
		c.Capability, c.Direction, c.AmountMinor, c.Currency, c.Funding))
	if errors.Is(err, pgx.ErrNoRows) { // the other attempt committed after this statement's snapshot
		opened, err = s.caseFor(ctx, c)
	}
	if err != nil {
		return fmt.Errorf("%w: open the compliance case: %v", partners.ErrScreeningUnavailable, err)
	}
	return refusalFor(opened)
}

// caseFor is the case already open for c's subject, name, direction and currency.
func (s *Screener) caseFor(ctx context.Context, c Case) (Case, error) {
	return scanCase(s.pool.QueryRow(ctx, `SELECT `+caseCols+` FROM compliance_cases WHERE workspace_id = $1 AND subject_kind = $2
		AND subject_id = $3 AND name = $4 AND direction = $5 AND currency = $6`, c.WorkspaceID, c.SubjectKind, c.SubjectID, c.Name, c.Direction, c.Currency))
}

// sameMoney says whether a and b move the same money: the amount, funding and capability a release was decided on.
func sameMoney(a, b Case) bool {
	sameAmount := a.AmountMinor == nil && b.AmountMinor == nil || a.AmountMinor != nil && b.AmountMinor != nil && *a.AmountMinor == *b.AmountMinor
	return sameAmount && a.Funding == b.Funding && a.Capability == b.Capability
}

func refusalFor(c Case) error {
	switch c.Status {
	case CaseReleased:
		return nil
	case CaseBlocked:
		return &Refusal{Case: c, err: ErrBlocked}
	case CaseHeld:
		if c.Outcome == OutcomeStale {
			return &Refusal{Case: c, err: ErrListsStale}
		}
		return &Refusal{Case: c, err: ErrHeld}
	default:
		return &Refusal{Case: c, err: ErrRefused}
	}
}

// Cases is the compliance cases in status — every status when it is "" — the newest first, at most limit.
func (s *Screener) Cases(ctx context.Context, status string, limit int) ([]Case, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+caseCols+` FROM compliance_cases WHERE $1 = '' OR status = $1
		ORDER BY opened_at DESC, id LIMIT $2`, status, limit)
	if err != nil {
		return nil, fmt.Errorf("screening: cases: %w", err)
	}
	defer rows.Close()
	out := []Case{}
	for rows.Next() {
		c, err := scanCase(rows)
		if err != nil {
			return nil, fmt.Errorf("screening: cases: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CaseColumns is the columns ScanCase reads, in its order.
const CaseColumns = caseCols

// ScanCase reads one compliance case selected as CaseColumns.
func ScanCase(row pgx.Row) (Case, error) { return scanCase(row) }

// Case is the compliance case id, any kind.
func (s *Screener) Case(ctx context.Context, id string) (Case, error) {
	c, err := scanCase(s.pool.QueryRow(ctx, `SELECT `+caseCols+` FROM compliance_cases WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Case{}, ErrCaseNotFound
	}
	return c, err
}

// AuditTarget is what the operator audit trail names a compliance case by (B30.8).
func AuditTarget(caseID string) string { return "compliance_case:" + caseID }

// Decide releases a held case — the close match is not them, and the money may move when it is tried again — or
// refuses it, and records who decided, when and why: on the case, and in the operator audit trail in the same
// transaction (B30.8).
func (s *Screener) Decide(ctx context.Context, id string, release bool, by, note string) (Case, error) {
	status, action := CaseRefused, "compliance.case.refuse"
	if release {
		status, action = CaseReleased, "compliance.case.release"
	}
	if strings.TrimSpace(by) == "" {
		return Case{}, errors.New("screening: a decision names who made it")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Case{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := scanCase(tx.QueryRow(ctx, `UPDATE compliance_cases SET status = $2, decided_by = $3, decided_at = now(), decision_note = $4
		WHERE id = $1 AND status = 'held' RETURNING `+caseCols, id, status, by, note))
	if err == nil {
		if _, err := operatoraudit.RecordIn(ctx, tx, operatoraudit.Entry{Actor: by, Action: action, Target: AuditTarget(id),
			Detail: note}); err != nil {
			return Case{}, err
		}
		return c, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Case{}, err
	}
	var current string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM compliance_cases WHERE id = $1`, id).Scan(&current); errors.Is(err, pgx.ErrNoRows) {
		return Case{}, ErrCaseNotFound
	} else if err != nil {
		return Case{}, err
	}
	return Case{}, fmt.Errorf("%w; this one is %s", ErrCaseDecided, current)
}
