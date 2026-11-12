// Package support is B59.27: what a workspace asks Talyvor for — support, a complaint, closing the account, an export
// of its data, or deleting it — kept per workspace in workspace_requests (migration 0376).
//
// Each request is received, in progress or done, with when and by whom it was filed, and reaches the operator's inbox.
// A complaint has its reference from the moment it is filed. Closing the account is refused while any of its accounts
// holds money, naming what each currency holds. A data export is built from the tenant data manifest
// (internal/tenantdata) for the owner to download once Talyvor has marked it done. A person at Talyvor answers every
// request and moves its state, recorded in the operator audit trail: nothing else is automatic, a deletion included
// (B52.37 is the deletion itself).
package support

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/operatoraudit"
	"github.com/talyvor/lens/internal/tenantdata"
)

// Kind is what a request asks for.
type Kind string

// The five kinds of request.
const (
	Support        Kind = "support"
	Complaint      Kind = "complaint"
	AccountClosure Kind = "account_closure"
	DataExport     Kind = "data_export"
	DataDeletion   Kind = "data_deletion"
)

// State is how far Talyvor has got with a request.
type State string

// The three states, in order.
const (
	Received   State = "received"
	InProgress State = "in_progress"
	Done       State = "done"
)

// MaxText is the longest message or answer, in characters.
const MaxText = 4000

var (
	// ErrInvalid is a request or answer the store will not take; the message says why.
	ErrInvalid = errors.New("support: invalid")
	// ErrNotFound is a request that is not there, or not the workspace's.
	ErrNotFound = errors.New("support: no such request")
	// ErrNotReady is an export asked for before Talyvor has marked it done.
	ErrNotReady = errors.New("support: the export is not ready yet")
)

// ParseKind returns the kind s names.
func ParseKind(s string) (Kind, error) {
	switch k := Kind(s); k {
	case Support, Complaint, AccountClosure, DataExport, DataDeletion:
		return k, nil
	}
	return "", fmt.Errorf("%w: a request is support, complaint, account_closure, data_export or data_deletion, not %q", ErrInvalid, s)
}

// ParseState returns the state s names.
func ParseState(s string) (State, error) {
	switch st := State(s); st {
	case Received, InProgress, Done:
		return st, nil
	}
	return "", fmt.Errorf("%w: a request is received, in_progress or done, not %q", ErrInvalid, s)
}

// OwnerOnly is whether only the workspace's owner may file a request of kind: closing the account and its data are
// the owner's to ask for, support and complaints anyone's who signs in to it.
func (k Kind) OwnerOnly() bool {
	return k == AccountClosure || k == DataExport || k == DataDeletion
}

// Request is one request as kept.
type Request struct {
	ID          int64     `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Kind        Kind      `json:"kind"`
	State       State     `json:"state"`
	Reference   string    `json:"reference,omitempty"`
	FiledBy     string    `json:"filed_by"`
	Message     string    `json:"message"`
	Answer      string    `json:"answer"`
	FiledAt     time.Time `json:"filed_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// BalancesRefusal refuses closing an account that still holds money: Balances is what each currency holds, in minor
// units.
type BalancesRefusal struct {
	Balances map[string]int64 `json:"balances"`
}

func (r *BalancesRefusal) Error() string {
	currencies := make([]string, 0, len(r.Balances))
	for c := range r.Balances {
		currencies = append(currencies, c)
	}
	sort.Strings(currencies)
	held := make([]string, 0, len(currencies))
	for _, c := range currencies {
		held = append(held, fmt.Sprintf("%d %s", r.Balances[c], c))
	}
	return "support: the account still holds " + strings.Join(held, ", ") + " (in minor units): move it out before closing the account"
}

// AuditAction is the operator_audit action of an answer; the target is the request.
const AuditAction = "workspace.request"

// Store reads and writes workspace_requests.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a Store over pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const columns = `id, workspace_id, kind, state, COALESCE(reference, ''), filed_by, message, answer, filed_at, updated_at`

func scan(row pgx.Row) (Request, error) {
	var r Request
	err := row.Scan(&r.ID, &r.WorkspaceID, &r.Kind, &r.State, &r.Reference, &r.FiledBy, &r.Message, &r.Answer, &r.FiledAt, &r.UpdatedAt)
	return r, err
}

// File keeps a request of kind for workspaceID, filed by the person by, and returns it received. Support and a
// complaint need a message; a complaint is given its reference here. Closing the account is refused with a
// *BalancesRefusal while any of the workspace's accounts holds money.
func (s *Store) File(ctx context.Context, workspaceID string, kind Kind, by, message string) (Request, error) {
	message = strings.TrimSpace(message)
	switch {
	case strings.TrimSpace(workspaceID) == "":
		return Request{}, fmt.Errorf("%w: a workspace is required", ErrInvalid)
	case strings.TrimSpace(by) == "":
		return Request{}, fmt.Errorf("%w: the person filing is required", ErrInvalid)
	case (kind == Support || kind == Complaint) && message == "":
		return Request{}, fmt.Errorf("%w: say what the %s is about", ErrInvalid, kind)
	case utf8.RuneCountInString(message) > MaxText:
		return Request{}, fmt.Errorf("%w: a message is at most %d characters", ErrInvalid, MaxText)
	}
	if _, err := ParseKind(string(kind)); err != nil {
		return Request{}, err
	}
	if kind == AccountClosure {
		held, err := s.Balances(ctx, workspaceID)
		if err != nil {
			return Request{}, err
		}
		if len(held) > 0 {
			return Request{}, &BalancesRefusal{Balances: held}
		}
	}
	var ref any
	if kind == Complaint {
		r, err := newReference()
		if err != nil {
			return Request{}, err
		}
		ref = r
	}
	out, err := scan(s.pool.QueryRow(ctx, `INSERT INTO workspace_requests (workspace_id, kind, reference, filed_by, message)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+columns, workspaceID, string(kind), ref, strings.TrimSpace(by), message))
	if err != nil {
		return Request{}, fmt.Errorf("support: file: %w", err)
	}
	return out, nil
}

// referenceAlphabet leaves out the letters and digits a person misreads (0 and O, 1 and I).
const referenceAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// newReference is a complaint's reference, C- and eight characters, random so it says nothing about how many others
// there are.
func newReference() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("support: reference: %w", err)
	}
	for i := range b {
		b[i] = referenceAlphabet[int(b[i])%len(referenceAlphabet)]
	}
	return "C-" + string(b[:4]) + "-" + string(b[4:]), nil
}

// Balances is what workspaceID's accounts that are not closed — the company's, its agents', pots and holds — hold, by
// currency, in minor units. Only currencies holding something are named.
func (s *Store) Balances(ctx context.Context, workspaceID string) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT a.currency, sum(b.balance_minor)::bigint FROM money_accounts a
		JOIN money_account_balances b ON b.account_id = a.id
		WHERE a.workspace_id = $1 AND a.status <> 'closed' AND a.purpose IN ('company', 'agent', 'pot', 'hold')
		GROUP BY a.currency HAVING sum(b.balance_minor) <> 0`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("support: balances: %w", err)
	}
	defer rows.Close()
	held := map[string]int64{}
	for rows.Next() {
		var currency string
		var minor int64
		if err := rows.Scan(&currency, &minor); err != nil {
			return nil, fmt.Errorf("support: balances: %w", err)
		}
		held[currency] = minor
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("support: balances: %w", err)
	}
	return held, nil
}

// List returns workspaceID's requests, newest first; with by set, only the ones that person filed.
func (s *Store) List(ctx context.Context, workspaceID, by string) ([]Request, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM workspace_requests
		WHERE workspace_id = $1 AND ($2 = '' OR filed_by = $2) ORDER BY filed_at DESC, id DESC`, workspaceID, by)
	if err != nil {
		return nil, fmt.Errorf("support: list: %w", err)
	}
	return collect(rows)
}

func collect(rows pgx.Rows) ([]Request, error) {
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Request, error) { return scan(row) })
	if err != nil {
		return nil, fmt.Errorf("support: list: %w", err)
	}
	if out == nil {
		out = []Request{}
	}
	return out, nil
}

// Get returns request id of workspaceID; ErrNotFound for another workspace's.
func (s *Store) Get(ctx context.Context, workspaceID string, id int64) (Request, error) {
	r, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM workspace_requests WHERE id = $1 AND workspace_id = $2`, id, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	if err != nil {
		return Request{}, fmt.Errorf("support: get: %w", err)
	}
	return r, nil
}

// InboxFilter narrows the operator's inbox. Empty fields do not filter.
type InboxFilter struct {
	State       State
	Kind        Kind
	WorkspaceID string
}

// inboxLimit caps the inbox: the oldest first, so nothing waiting is pushed off the end.
const inboxLimit = 1000

// Inbox returns every workspace's requests f selects, the oldest first.
func (s *Store) Inbox(ctx context.Context, f InboxFilter) ([]Request, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM workspace_requests
		WHERE ($1 = '' OR state = $1) AND ($2 = '' OR kind = $2) AND ($3 = '' OR workspace_id = $3)
		ORDER BY filed_at, id LIMIT $4`, string(f.State), string(f.Kind), f.WorkspaceID, inboxLimit)
	if err != nil {
		return nil, fmt.Errorf("support: inbox: %w", err)
	}
	return collect(rows)
}

// Answer moves request id to state with the answer the workspace reads, by the operator named actor, and records it
// in the operator audit trail in the same transaction. An empty answer keeps the one it has.
func (s *Store) Answer(ctx context.Context, id int64, state State, answer, actor string) (Request, error) {
	answer = strings.TrimSpace(answer)
	if _, err := ParseState(string(state)); err != nil {
		return Request{}, err
	}
	if utf8.RuneCountInString(answer) > MaxText {
		return Request{}, fmt.Errorf("%w: an answer is at most %d characters", ErrInvalid, MaxText)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Request{}, fmt.Errorf("support: answer: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, err := scan(tx.QueryRow(ctx, `UPDATE workspace_requests SET state = $2,
		answer = CASE WHEN $3 = '' THEN answer ELSE $3 END, updated_at = now() WHERE id = $1 RETURNING `+columns,
		id, string(state), answer))
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	if err != nil {
		return Request{}, fmt.Errorf("support: answer: %w", err)
	}
	if _, err := operatoraudit.RecordIn(ctx, tx, operatoraudit.Entry{Actor: actor, Action: AuditAction,
		Target: fmt.Sprintf("%s/%d", out.WorkspaceID, out.ID), Detail: string(out.Kind) + " " + string(state)}); err != nil {
		return Request{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Request{}, fmt.Errorf("support: answer: %w", err)
	}
	return out, nil
}

// secretColumn names the columns an export leaves out: what a key, a password, a token or a sealed secret is kept as.
// The rest of each row is the workspace's own.
var secretColumn = regexp.MustCompile(`secret|hash|sha(1|256|512)|digest|cipher|encrypt|password|private|_enc$|sealed|` +
	`credential|api_key|signing_key|refresh|access_token|token_ref|(^|_)otp(_|$)|recovery_code|nonce`)

// Export writes request id's export of workspaceID as one JSON object: the workspace, when it was built, and
// "tables", each table of the tenant data manifest that holds the workspace's rows, by name, with those rows. A row is
// the workspace's when its owner column names it (exportColumns). Tables the manifest marks not tenant scoped, any
// other workspace's id in a row, and the columns secretColumn names, are left out. Refused with ErrNotReady until
// Talyvor has marked the request done, and ErrNotFound for anything but this workspace's export request.
func (s *Store) Export(ctx context.Context, w io.Writer, workspaceID string, id int64) error {
	r, err := s.Get(ctx, workspaceID, id)
	if err != nil {
		return err
	}
	if r.Kind != DataExport {
		return ErrNotFound
	}
	if r.State != Done {
		return ErrNotReady
	}
	return s.WriteExport(ctx, w, workspaceID)
}

// WriteExport writes workspaceID's data as Export describes, whatever the state of any request.
func (s *Store) WriteExport(ctx context.Context, w io.Writer, workspaceID string) error {
	tables := make([]string, 0, len(tenantdata.Manifest))
	for t, e := range tenantdata.Manifest {
		if e.Disposition != tenantdata.NotTenantScoped {
			tables = append(tables, t)
		}
	}
	sort.Strings(tables)
	keyedByID := map[string]bool{}
	for _, t := range tenantdata.MappingTables {
		keyedByID[t] = true
	}
	head, err := json.Marshal(workspaceID)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, `{"workspace_id":%s,"built_at":"%s","tables":{`, head, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	first := true
	for _, t := range tables {
		owner, hidden, err := s.exportColumns(ctx, t, keyedByID[t])
		if err != nil {
			return err
		}
		if owner == "" {
			continue
		}
		rows, err := s.pool.Query(ctx, `SELECT (to_jsonb(t) - $2::text[])::text FROM `+pgx.Identifier{t}.Sanitize()+
			` t WHERE `+pgx.Identifier{owner}.Sanitize()+`::text = $1`, workspaceID, hidden)
		if err != nil {
			return fmt.Errorf("support: export %s: %w", t, err)
		}
		n := 0
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				rows.Close()
				return fmt.Errorf("support: export %s: %w", t, err)
			}
			sep := ","
			if n == 0 {
				sep = `"` + t + `":[`
				if !first {
					sep = "," + sep
				}
				first = false
			}
			if _, err := io.WriteString(w, sep+row); err != nil {
				rows.Close()
				return err
			}
			n++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("support: export %s: %w", t, err)
		}
		if n > 0 {
			if _, err := io.WriteString(w, "]"); err != nil {
				return err
			}
		}
	}
	_, err = io.WriteString(w, "}}\n")
	return err
}

// exportColumns is the column naming the workspace that owns each of table's rows, and the columns its export leaves
// out. The owner is workspace_id; id for a table keyed by the workspace; otherwise the table's one column whose name
// holds "workspace". A table with two such columns and no workspace_id — two parties and no owner — is left out, as is
// a table this schema does not have or one reached only through a parent's cascade: owner "". Every other workspace
// column is left out of the rows, so an export never names another workspace.
func (s *Store) exportColumns(ctx context.Context, table string, keyedByID bool) (owner string, hidden []string, err error) {
	rows, err := s.pool.Query(ctx, `SELECT a.attname FROM pg_attribute a WHERE a.attrelid = to_regclass($1)
		AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, pgx.Identifier{table}.Sanitize())
	if err != nil {
		return "", nil, fmt.Errorf("support: export %s: %w", table, err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", nil, fmt.Errorf("support: export %s: %w", table, err)
	}
	var tenant []string
	hidden = []string{}
	for _, n := range names {
		switch {
		case strings.Contains(n, "workspace"):
			tenant = append(tenant, n)
		case secretColumn.MatchString(n):
			hidden = append(hidden, n)
		}
	}
	switch {
	case keyedByID:
		owner = "id"
	case slices.Contains(tenant, "workspace_id"):
		owner = "workspace_id"
	case len(tenant) == 1:
		owner = tenant[0]
	default:
		return "", nil, nil
	}
	for _, n := range tenant {
		if n != owner {
			hidden = append(hidden, n)
		}
	}
	return owner, hidden, nil
}
