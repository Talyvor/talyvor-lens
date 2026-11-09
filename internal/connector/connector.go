// Package connector is B41.178's Talyvor connector: the OAuth 2.1 authorisation server an assistant (Claude, ChatGPT,
// Cursor) uses to add a marketplace listing as its own tool. The assistant registers itself, sends its user to
// authorize, the owner approves the grant for one of their agents on Talyvor's connect page, and the assistant swaps
// the single-use code, with its PKCE verifier, for that agent's own key: every use is then the agent's, under its
// rules, on its owner's marketplace bill.
package connector

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GrantTTL is how long a grant waits for the owner, and then how long its code waits for the assistant.
const GrantTTL = 10 * time.Minute

var (
	ErrInvalidClient   = errors.New("connector: unknown client or a redirect_uri it did not register")
	ErrInvalidRequest  = errors.New("connector: invalid request")
	ErrGrantNotFound   = errors.New("connector: no pending request with this id; it may have expired or been decided")
	ErrInvalidGrant    = errors.New("connector: the code is unknown, used, expired or not this client's")
	ErrRedirectRefused = errors.New("connector: a redirect_uri must be https, http on localhost, or cursor:// or vscode://")
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

type Client struct {
	ID           string   `json:"client_id"`
	Name         string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
}

// Request is what the connect page shows the owner before they decide.
type Request struct {
	ID           string    `json:"id"`
	ClientName   string    `json:"client_name"`
	RedirectHost string    `json:"redirect_host"` // who the code goes to: the page shows it so a lookalike name can't hide it
	ListingID    string    `json:"listing_id,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Grant is a redeemed code: the agent whose key the assistant gets.
type Grant struct {
	ID, WorkspaceID, AgentID, ListingID, ClientName string
}

func token(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func allowedRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || len(raw) > 512 {
		return false
	}
	switch u.Scheme {
	case "https":
		return u.Host != ""
	case "http":
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	case "cursor", "vscode":
		return true
	}
	return false
}

// Register is OAuth dynamic client registration (RFC 7591) for a public client.
func (s *Store) Register(ctx context.Context, name string, redirectURIs []string) (Client, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 || len(redirectURIs) == 0 || len(redirectURIs) > 5 {
		return Client{}, fmt.Errorf("%w: client_name (1-100 characters) and 1-5 redirect_uris are required", ErrInvalidRequest)
	}
	for _, u := range redirectURIs {
		if !allowedRedirect(u) {
			return Client{}, ErrRedirectRefused
		}
	}
	c := Client{ID: "tc_" + token(18), Name: name, RedirectURIs: redirectURIs}
	_, err := s.pool.Exec(ctx, `INSERT INTO connector_clients (client_id, client_name, redirect_uris) VALUES ($1, $2, $3)`, c.ID, c.Name, c.RedirectURIs)
	return c, err
}

// CheckClient says whether redirectURI is one clientID registered: only then may an error be redirected to it.
func (s *Store) CheckClient(ctx context.Context, clientID, redirectURI string) error {
	var uris []string
	err := s.pool.QueryRow(ctx, `SELECT redirect_uris FROM connector_clients WHERE client_id = $1`, clientID).Scan(&uris)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !slices.Contains(uris, redirectURI) {
		return ErrInvalidClient
	}
	return err
}

// Begin opens a grant for the owner to decide; the caller has already checked the client (CheckClient).
func (s *Store) Begin(ctx context.Context, clientID, redirectURI, codeChallenge, state, resource, listingID string) (string, error) {
	id := "cg_" + token(18)
	_, err := s.pool.Exec(ctx, `INSERT INTO connector_grants (id, client_id, redirect_uri, code_challenge, state, resource, listing_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now() + $8 * interval '1 second')`, id, clientID, redirectURI, codeChallenge, state, resource, listingID, int64(GrantTTL.Seconds()))
	return id, err
}

// Pending reads an undecided, unexpired grant for the connect page.
func (s *Store) Pending(ctx context.Context, id string) (Request, error) {
	r := Request{ID: id}
	var redirect string
	err := s.pool.QueryRow(ctx, `SELECT c.client_name, g.redirect_uri, g.listing_id, g.expires_at FROM connector_grants g
		JOIN connector_clients c USING (client_id) WHERE g.id = $1 AND g.status = 'pending' AND g.expires_at > now()`, id).
		Scan(&r.ClientName, &redirect, &r.ListingID, &r.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrGrantNotFound
	}
	if u, perr := url.Parse(redirect); perr == nil {
		r.RedirectHost = u.Scheme + "://" + u.Host
	}
	return r, err
}

// Decide records the owner's answer once and returns where to send their browser: the assistant's redirect_uri with a
// code, or with access_denied.
func (s *Store) Decide(ctx context.Context, id, workspaceID, agentID string, approve bool) (string, error) {
	status, code, codeHash := "denied", "", any(nil)
	if approve {
		status, code = "approved", token(32)
		codeHash = hash(code)
	}
	var redirect, state string
	err := s.pool.QueryRow(ctx, `UPDATE connector_grants SET status = $2, workspace_id = $3, agent_id = $4, code_hash = $5,
			expires_at = now() + $6 * interval '1 second'
		WHERE id = $1 AND status = 'pending' AND expires_at > now() RETURNING redirect_uri, state`,
		id, status, workspaceID, agentID, codeHash, int64(GrantTTL.Seconds())).Scan(&redirect, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrGrantNotFound
	}
	if err != nil {
		return "", err
	}
	u, err := url.Parse(redirect)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if approve {
		q.Set("code", code)
	} else {
		q.Set("error", "access_denied")
	}
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Redeem spends a code once. A wrong client, redirect_uri or verifier still spends it, as OAuth 2.1 asks.
func (s *Store) Redeem(ctx context.Context, code, clientID, redirectURI, verifier string) (Grant, error) {
	var g Grant
	var gotClient, gotRedirect, challenge string
	err := s.pool.QueryRow(ctx, `UPDATE connector_grants g SET status = 'used' FROM connector_clients c
		WHERE g.code_hash = $1 AND g.status = 'approved' AND g.expires_at > now() AND c.client_id = g.client_id
		RETURNING g.id, g.workspace_id, g.agent_id, g.listing_id, c.client_name, g.client_id, g.redirect_uri, g.code_challenge`, hash(code)).
		Scan(&g.ID, &g.WorkspaceID, &g.AgentID, &g.ListingID, &g.ClientName, &gotClient, &gotRedirect, &challenge)
	if errors.Is(err, pgx.ErrNoRows) {
		return g, ErrInvalidGrant
	}
	if err != nil {
		return g, err
	}
	sum := sha256.Sum256([]byte(verifier))
	pkce := base64.RawURLEncoding.EncodeToString(sum[:])
	if gotClient != clientID || gotRedirect != redirectURI || len(verifier) < 43 || len(verifier) > 128 ||
		subtle.ConstantTimeCompare([]byte(pkce), []byte(challenge)) != 1 {
		return Grant{}, ErrInvalidGrant
	}
	return g, nil
}

// Issue gives a redeemed grant its access token, behind keyID: the agent's key minted for it, whose plaintext nobody
// holds. Only the grant's listing URL accepts the token (Holder), so the assistant can use that listing and nothing
// else the agent may do.
func (s *Store) Issue(ctx context.Context, grantID, keyID string) (string, error) {
	tok := "tlc_" + token(32)
	tag, err := s.pool.Exec(ctx, `UPDATE connector_grants SET key_id = $2, access_hash = $3 WHERE id = $1 AND status = 'used' AND key_id IS NULL`,
		grantID, keyID, hash(tok))
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrInvalidGrant
	}
	return tok, err
}

// Holder is who an access token acts as: the workspace and the agent's key behind it.
type Holder struct {
	WorkspaceID, KeyID string
	Scopes             []string
}

// Holder reads the token's grant for listingID; ErrInvalidGrant for a token of another listing, or whose key the owner
// revoked or that has expired.
func (s *Store) Holder(ctx context.Context, accessToken, listingID string) (Holder, error) {
	var h Holder
	err := s.pool.QueryRow(ctx, `SELECT g.workspace_id, g.key_id, k.scopes FROM connector_grants g JOIN workspace_api_keys k ON k.id::text = g.key_id
		WHERE g.access_hash = $1 AND g.listing_id = $2 AND g.status = 'used' AND (k.expires_at IS NULL OR k.expires_at > now())`,
		hash(accessToken), listingID).Scan(&h.WorkspaceID, &h.KeyID, &h.Scopes)
	if errors.Is(err, pgx.ErrNoRows) {
		return h, ErrInvalidGrant
	}
	return h, err
}
