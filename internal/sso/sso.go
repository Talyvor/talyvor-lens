// Package sso keeps a workspace's own identity provider (B56.18): an OIDC issuer, a client id, the client secret
// sealed by internal/envelope like a BYOK key, and the email domains the connection covers. A domain counts only
// once proven, by a DNS TXT record Lens reads, and only one workspace can prove a domain.
//
// The owner saves, reads back and removes the connection, and never sees the secret again. The one place the secret
// is opened is For, which the suite BFF calls on its provision secret when someone signs in with an email.
package sso

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/envelope"
	"github.com/talyvor/lens/internal/plans"
)

// RecordPrefix is the label in front of a domain whose TXT record proves it, and ValuePrefix what the record says
// before the workspace's token.
const (
	RecordPrefix = "_talyvor-sso."
	ValuePrefix  = "talyvor-sso="
)

var (
	ErrInvalid      = errors.New("sso: the connection needs an https issuer, a client id, a client secret and at least one email domain")
	ErrNoConnection = errors.New("sso: this workspace has no single sign-on connection")
	ErrNoDomain     = errors.New("sso: that domain is not on this workspace's connection")
	ErrDomainTaken  = errors.New("sso: another workspace has already proven that domain")
	ErrUnproven     = errors.New("sso: the TXT record for that domain does not answer with this workspace's value yet")
)

// Domain is an email domain on a connection, with the TXT record that proves it.
type Domain struct {
	Domain   string     `json:"domain"`
	Record   string     `json:"record"` // the TXT record's name
	Value    string     `json:"value"`  // what it must answer
	ProvenAt *time.Time `json:"proven_at"`
}

// Connection is what the owner sees of the workspace's connection. Never the secret.
type Connection struct {
	Issuer    string    `json:"issuer"`
	ClientID  string    `json:"client_id"`
	Domains   []Domain  `json:"domains"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SignIn is the connection that covers an email, for the BFF alone: it carries the secret.
type SignIn struct {
	WorkspaceID  string `json:"workspace_id"`
	Issuer       string `json:"issuer"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// Store keeps workspaces' connections, their secrets sealed.
type Store struct {
	pool *pgxpool.Pool
	ring *envelope.Keyring
	// LookupTXT answers a domain's TXT records; the system resolver unless a test sets it.
	LookupTXT func(ctx context.Context, name string) ([]string, error)
}

// New returns a Store sealing under ring. A nil ring is custody disabled; the caller must not build one.
func New(pool *pgxpool.Pool, ring *envelope.Keyring) *Store {
	return &Store{pool: pool, ring: ring, LookupTXT: net.DefaultResolver.LookupTXT}
}

// aad binds a sealed secret to its workspace, so a row copied to another workspace fails to open.
func aad(workspaceID string) []byte { return []byte(workspaceID + "|sso") }

// allowed refuses, with a *plans.Refusal, a workspace whose plan does not include single sign-on.
func (s *Store) allowed(ctx context.Context, q plans.Querier, workspaceID string) error {
	plan, err := plans.Of(ctx, q, workspaceID)
	if err != nil {
		return fmt.Errorf("sso: %w", err)
	}
	if !plan.SSO {
		return plan.RefuseSSO(plans.Current())
	}
	return nil
}

// Save stores (or replaces) the workspace's connection. A domain kept from the connection it replaces keeps its token
// and stays proven; a new one starts unproven. A domain another workspace has proven is refused.
func (s *Store) Save(ctx context.Context, workspaceID, issuer, clientID, secret string, domains []string) (Connection, error) {
	issuer, clientID, secret = strings.TrimRight(strings.TrimSpace(issuer), "/"), strings.TrimSpace(clientID), strings.TrimSpace(secret)
	u, err := url.Parse(issuer)
	// The BFF fetches the issuer, so it must name a public host by name on 443: never an address, localhost or a bare
	// name an internal resolver answers.
	if err != nil || u.Scheme != "https" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || !publicHost(u) ||
		clientID == "" || len(clientID) > 512 || secret == "" || len(secret) > 2048 || len(domains) == 0 || len(domains) > 50 {
		return Connection{}, ErrInvalid
	}
	clean := make([]string, 0, len(domains))
	for _, d := range domains {
		d, ok := normalise(d)
		if !ok {
			return Connection{}, fmt.Errorf("%w: %q is not a domain", ErrInvalid, d)
		}
		if !slices.Contains(clean, d) {
			clean = append(clean, d)
		}
	}
	if err := s.allowed(ctx, s.pool, workspaceID); err != nil {
		return Connection{}, err
	}
	sealed, err := s.ring.Seal([]byte(secret), aad(workspaceID))
	if err != nil {
		return Connection{}, fmt.Errorf("sso: seal: %w", err)
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var taken string
		err := tx.QueryRow(ctx, `SELECT domain FROM workspace_sso_domains
			WHERE domain = ANY($1) AND workspace_id <> $2 AND proven_at IS NOT NULL LIMIT 1`, clean, workspaceID).Scan(&taken)
		if err == nil {
			return fmt.Errorf("%w: %s", ErrDomainTaken, taken)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workspace_sso_connections
				(workspace_id, issuer, client_id, key_id, wrapped_dek, dek_nonce, ciphertext, ct_nonce)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (workspace_id) DO UPDATE SET
				issuer = EXCLUDED.issuer, client_id = EXCLUDED.client_id, key_id = EXCLUDED.key_id,
				wrapped_dek = EXCLUDED.wrapped_dek, dek_nonce = EXCLUDED.dek_nonce, ciphertext = EXCLUDED.ciphertext,
				ct_nonce = EXCLUDED.ct_nonce, updated_at = NOW()`,
			workspaceID, issuer, clientID, sealed.KeyID, sealed.WrappedDEK, sealed.DEKNonce, sealed.Ciphertext, sealed.CTNonce); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM workspace_sso_domains WHERE workspace_id = $1 AND NOT (domain = ANY($2))`,
			workspaceID, clean); err != nil {
			return err
		}
		for _, d := range clean {
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_sso_domains (workspace_id, domain, token) VALUES ($1, $2, $3)
				ON CONFLICT (workspace_id, domain) DO NOTHING`, workspaceID, d, token()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrDomainTaken) {
			return Connection{}, err
		}
		return Connection{}, fmt.Errorf("sso: store connection: %w", err)
	}
	return s.Get(ctx, workspaceID)
}

// Get is the workspace's connection without its secret; ErrNoConnection when there is none.
func (s *Store) Get(ctx context.Context, workspaceID string) (Connection, error) {
	var c Connection
	err := s.pool.QueryRow(ctx, `SELECT issuer, client_id, updated_at FROM workspace_sso_connections WHERE workspace_id = $1`,
		workspaceID).Scan(&c.Issuer, &c.ClientID, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, ErrNoConnection
	}
	if err != nil {
		return Connection{}, fmt.Errorf("sso: read connection: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT domain, token, proven_at FROM workspace_sso_domains
		WHERE workspace_id = $1 ORDER BY domain`, workspaceID)
	if err != nil {
		return Connection{}, fmt.Errorf("sso: read domains: %w", err)
	}
	defer rows.Close()
	c.Domains = []Domain{}
	for rows.Next() {
		var d Domain
		var tok string
		if err := rows.Scan(&d.Domain, &tok, &d.ProvenAt); err != nil {
			return Connection{}, err
		}
		d.Record, d.Value = RecordPrefix+d.Domain, ValuePrefix+tok
		c.Domains = append(c.Domains, d)
	}
	return c, rows.Err()
}

// Check reads the domain's TXT record and marks it proven when one answers with the workspace's value. Already
// proven answers as it is; a domain another workspace has proven is ErrDomainTaken.
func (s *Store) Check(ctx context.Context, workspaceID, domain string) (Domain, error) {
	domain, _ = normalise(domain)
	var tok string
	var proven *time.Time
	err := s.pool.QueryRow(ctx, `SELECT token, proven_at FROM workspace_sso_domains WHERE workspace_id = $1 AND domain = $2`,
		workspaceID, domain).Scan(&tok, &proven)
	if errors.Is(err, pgx.ErrNoRows) {
		return Domain{}, ErrNoDomain
	}
	if err != nil {
		return Domain{}, fmt.Errorf("sso: read domain: %w", err)
	}
	d := Domain{Domain: domain, Record: RecordPrefix + domain, Value: ValuePrefix + tok, ProvenAt: proven}
	if proven != nil {
		return d, nil
	}
	if err := s.allowed(ctx, s.pool, workspaceID); err != nil {
		return Domain{}, err
	}
	// A failed lookup (no such record) is the same answer as a record with another value: not proven yet.
	records, _ := s.LookupTXT(ctx, d.Record)
	if !slices.Contains(records, d.Value) {
		return d, ErrUnproven
	}
	err = s.pool.QueryRow(ctx, `UPDATE workspace_sso_domains SET proven_at = NOW()
		WHERE workspace_id = $1 AND domain = $2 RETURNING proven_at`, workspaceID, domain).Scan(&d.ProvenAt)
	if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Domain{}, ErrDomainTaken
	}
	if err != nil {
		return Domain{}, fmt.Errorf("sso: prove domain: %w", err)
	}
	return d, nil
}

// Remove deletes the workspace's connection and its domains; false when there was none.
func (s *Store) Remove(ctx context.Context, workspaceID string) (bool, error) {
	ct, err := s.pool.Exec(ctx, `DELETE FROM workspace_sso_connections WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return false, fmt.Errorf("sso: remove connection: %w", err)
	}
	return ct.RowsAffected() == 1, nil
}

// For is the connection whose proven domain covers email, with its secret opened, while that workspace's plan includes
// single sign-on; ErrNoConnection otherwise.
func (s *Store) For(ctx context.Context, email string) (SignIn, error) {
	_, domain, found := strings.Cut(strings.ToLower(strings.TrimSpace(email)), "@")
	if !found {
		return SignIn{}, ErrNoConnection
	}
	var out SignIn
	var sealed envelope.Sealed
	err := s.pool.QueryRow(ctx, `
		SELECT c.workspace_id, c.issuer, c.client_id, c.key_id, c.wrapped_dek, c.dek_nonce, c.ciphertext, c.ct_nonce
		FROM workspace_sso_domains d JOIN workspace_sso_connections c USING (workspace_id)
		WHERE d.domain = $1 AND d.proven_at IS NOT NULL`, domain).
		Scan(&out.WorkspaceID, &out.Issuer, &out.ClientID, &sealed.KeyID, &sealed.WrappedDEK, &sealed.DEKNonce, &sealed.Ciphertext, &sealed.CTNonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return SignIn{}, ErrNoConnection
	}
	if err != nil {
		return SignIn{}, fmt.Errorf("sso: read connection: %w", err)
	}
	if err := s.allowed(ctx, s.pool, out.WorkspaceID); err != nil {
		if errors.Is(err, plans.ErrRefused) {
			return SignIn{}, ErrNoConnection
		}
		return SignIn{}, err
	}
	if err := s.ring.Use(sealed, aad(out.WorkspaceID), func(pt []byte) error {
		out.ClientSecret = string(pt)
		return nil
	}); err != nil {
		return SignIn{}, fmt.Errorf("sso: the client secret of workspace %s cannot be opened, so it must be entered again: %w", out.WorkspaceID, err)
	}
	return out, nil
}

// publicHost reports whether u names its host as a dotted DNS name on the https port: never an address in any form
// (a top-level label starts with a letter, so 127.1 and 0x7f.1 are refused) nor a private-use name. Input hygiene
// only: the BFF still refuses an issuer that resolves to a private address when it fetches it.
func publicHost(u *url.URL) bool {
	host, ok := normalise(u.Hostname())
	if !ok || net.ParseIP(host) != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	tld := host[strings.LastIndexByte(host, '.')+1:]
	if tld[0] < 'a' || tld[0] > 'z' {
		return false
	}
	for _, private := range []string{".localhost", ".internal", ".local", ".lan", ".home.arpa", ".corp", ".intranet"} {
		if strings.HasSuffix(host, private) {
			return false
		}
	}
	return true
}

// normalise lowercases a domain and drops a trailing dot; ok is false unless it is a hostname with a dot in it.
func normalise(d string) (string, bool) {
	d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
	if len(d) > 253 || !strings.Contains(d, ".") {
		return d, false
	}
	for _, label := range strings.Split(d, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return d, false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return d, false
			}
		}
	}
	return d, true
}

// token is 16 random bytes in hex: the value a domain's TXT record must carry.
func token() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
