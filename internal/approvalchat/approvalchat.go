// Package approvalchat is Slack and Teams approvals (B56.16): a workspace on a plan with slack_teams_approvals saves
// one incoming webhook, Slack's or Teams', and each approval filed is posted there — the agent, the payee or model,
// the amount, the reason, the expiry and one button that opens the approval in Talyvor. The decision is made in
// Talyvor with the passkey; the post carries no action Slack or Teams could send back, and no route takes one.
//
// The webhook's URL is its credential, so it is kept as a provider key is (internal/byok): sealed by internal/envelope
// with the workspace and kind as aad, accepted only while LENS_PROVIDER_SECRET_KEK is armed, and never returned —
// an answer names its kind and host. Errors from a delivery are stripped of the URL before anyone logs them.
package approvalchat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/envelope"
	"github.com/talyvor/lens/internal/plans"
)

// The kinds of webhook.
const (
	Slack = "slack"
	Teams = "teams"
)

// ErrInvalidHook is a webhook that is not Slack's or Teams'.
var ErrInvalidHook = errors.New("approvalchat: the webhook must be a Slack incoming webhook (https://hooks.slack.com/...) " +
	"or a Teams one (https://...webhook.office.com, ...logic.azure.com or ...api.powerplatform.com)")

// Hook is what is ever shown of a saved webhook.
type Hook struct {
	Kind      string    `json:"kind"`
	Host      string    `json:"host"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Approval is one approval as it is posted; the amount is already in words (economy.LXCMoney).
type Approval struct {
	ID, Agent, To, Amount, Reason, URL string
	ExpiresAt                          time.Time
}

// Store keeps workspaces' webhooks, sealed, and posts approvals to them.
type Store struct {
	pool   *pgxpool.Pool
	ring   *envelope.Keyring
	client *http.Client
	tries  int
	pause  time.Duration
}

// New returns a Store sealing under ring and posting through client; a nil client is one that waits 10 seconds and
// follows no redirect, since a redirect could send an approval anywhere.
func New(pool *pgxpool.Pool, ring *envelope.Keyring, client *http.Client) *Store {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &Store{pool: pool, ring: ring, client: client, tries: 3, pause: time.Second}
}

func aad(workspaceID, kind string) []byte { return []byte(workspaceID + "|approval_chat|" + kind) }

// Allowed reports whether u can be kind's incoming webhook: https on the default port, on Slack's or Microsoft's own
// webhook hosts, so a saved URL cannot point Lens at anything else.
func Allowed(kind string, u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	switch kind {
	case Slack:
		return host == "hooks.slack.com"
	case Teams:
		for _, s := range []string{".webhook.office.com", ".logic.azure.com", ".api.powerplatform.com"} {
			if strings.HasSuffix(host, s) {
				return true
			}
		}
	}
	return false
}

// On reports whether workspaceID's plan has Slack and Teams approvals.
func (s *Store) On(ctx context.Context, workspaceID string) (bool, error) {
	w, err := plans.Of(ctx, s.pool, workspaceID)
	return w.SlackTeamsApprovals, err
}

// Put saves (or replaces) the workspace's webhook. It refuses, with a *plans.Refusal, a plan without it.
func (s *Store) Put(ctx context.Context, workspaceID, kind, rawURL string) (Hook, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || len(rawURL) > 2048 || !Allowed(kind, u) {
		return Hook{}, ErrInvalidHook
	}
	w, err := plans.Of(ctx, s.pool, workspaceID)
	if err != nil {
		return Hook{}, fmt.Errorf("approvalchat: %w", err)
	}
	if !w.SlackTeamsApprovals {
		return Hook{}, w.RefuseSlackTeams(plans.Current())
	}
	sealed, err := s.ring.Seal([]byte(u.String()), aad(workspaceID, kind))
	if err != nil {
		return Hook{}, fmt.Errorf("approvalchat: seal: %w", err)
	}
	h := Hook{Kind: kind, Host: strings.ToLower(u.Hostname())}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO workspace_approval_chat (workspace_id, kind, host, key_id, wrapped_dek, dek_nonce, ciphertext, ct_nonce)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (workspace_id) DO UPDATE SET kind = EXCLUDED.kind, host = EXCLUDED.host, key_id = EXCLUDED.key_id,
			wrapped_dek = EXCLUDED.wrapped_dek, dek_nonce = EXCLUDED.dek_nonce, ciphertext = EXCLUDED.ciphertext,
			ct_nonce = EXCLUDED.ct_nonce, updated_at = NOW()
		RETURNING updated_at`,
		workspaceID, kind, h.Host, sealed.KeyID, sealed.WrappedDEK, sealed.DEKNonce, sealed.Ciphertext, sealed.CTNonce).Scan(&h.UpdatedAt)
	if err != nil {
		return Hook{}, fmt.Errorf("approvalchat: save the webhook: %w", err)
	}
	return h, nil
}

// Get is the workspace's webhook as it is shown; nil when it has none.
func (s *Store) Get(ctx context.Context, workspaceID string) (*Hook, error) {
	var h Hook
	err := s.pool.QueryRow(ctx, `SELECT kind, host, updated_at FROM workspace_approval_chat WHERE workspace_id = $1`, workspaceID).
		Scan(&h.Kind, &h.Host, &h.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("approvalchat: read the webhook: %w", err)
	}
	return &h, nil
}

// Delete forgets the workspace's webhook; false when it had none.
func (s *Store) Delete(ctx context.Context, workspaceID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM workspace_approval_chat WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return false, fmt.Errorf("approvalchat: delete the webhook: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Post sends a to the workspace's webhook, while its plan has Slack and Teams approvals; with no webhook, or on a plan
// without them, it sends nothing. A 429, a 5xx or a failed connection is tried again, up to three times in all; any
// other answer is final, so a webhook that took the post is never sent it twice.
func (s *Store) Post(ctx context.Context, workspaceID string, a Approval) error {
	var kind string
	var sealed envelope.Sealed
	err := s.pool.QueryRow(ctx, `SELECT kind, key_id, wrapped_dek, dek_nonce, ciphertext, ct_nonce FROM workspace_approval_chat
		WHERE workspace_id = $1`, workspaceID).Scan(&kind, &sealed.KeyID, &sealed.WrappedDEK, &sealed.DEKNonce, &sealed.Ciphertext, &sealed.CTNonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("approvalchat: read the webhook: %w", err)
	}
	if on, err := s.On(ctx, workspaceID); err != nil || !on {
		return err
	}
	body, err := json.Marshal(message(kind, a))
	if err != nil {
		return err
	}
	var hook string
	if err := s.ring.Use(sealed, aad(workspaceID, kind), func(pt []byte) error { hook = string(pt); return nil }); err != nil {
		return fmt.Errorf("approvalchat: the %s webhook of %s cannot be opened — it must be saved again: %w", kind, workspaceID, err)
	}
	for try := 1; ; try++ {
		again, err := s.send(ctx, hook, body)
		if err == nil || !again || try == s.tries {
			if err != nil {
				return fmt.Errorf("approvalchat: posting approval %s to %s: %w", a.ID, kind, err)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("approvalchat: posting approval %s to %s: %w", a.ID, kind, ctx.Err())
		case <-time.After(time.Duration(try) * s.pause):
		}
	}
}

// send posts body once; again says whether another try is safe and may succeed. Its error never holds the URL.
func (s *Store) send(ctx context.Context, hook string, body []byte) (again bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook, bytes.NewReader(body))
	if err != nil {
		return false, errors.New("the webhook cannot be posted to")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // a *url.Error quotes the URL
		}
		return true, err
	}
	_ = resp.Body.Close()
	switch c := resp.StatusCode; {
	case c >= 200 && c <= 299:
		return false, nil
	case c == http.StatusTooManyRequests || c >= 500:
		return true, fmt.Errorf("the webhook answered HTTP %d", c)
	default:
		return false, fmt.Errorf("the webhook answered HTTP %d", c)
	}
}

// message is a as kind's webhook takes it: a Slack message with a link button, or a Teams Adaptive Card with one
// Action.OpenUrl. The agent named itself and wrote the reason, so neither can mention anyone or hide a link behind words.
func message(kind string, a Approval) any {
	line := a.Agent + " wants to spend " + a.Amount
	if a.To != "" {
		line = a.Agent + " wants to pay " + a.To + " " + a.Amount
	}
	expires := "Expires " + a.ExpiresAt.UTC().Format("2 Jan 2006, 15:04 UTC")
	if kind == Teams {
		safe := strings.NewReplacer("[", "(", "]", ")").Replace
		body := []any{
			map[string]any{"type": "TextBlock", "text": "Approval needed", "weight": "Bolder", "size": "Medium"},
			map[string]any{"type": "TextBlock", "text": safe(line), "wrap": true},
		}
		if a.Reason != "" {
			body = append(body, map[string]any{"type": "TextBlock", "text": safe(a.Reason), "wrap": true, "isSubtle": true})
		}
		body = append(body, map[string]any{"type": "TextBlock", "text": expires, "isSubtle": true})
		return map[string]any{"type": "message", "attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content": map[string]any{"$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "type": "AdaptiveCard", "version": "1.4",
				"body": body, "actions": []any{map[string]any{"type": "Action.OpenUrl", "title": "Open in Talyvor", "url": a.URL}}},
		}}}
	}
	safe := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace
	text := "*Approval needed*\n" + safe(line)
	if a.Reason != "" {
		text += "\n" + safe(a.Reason)
	}
	return map[string]any{
		"text": "Approval needed: " + safe(line),
		"blocks": []any{
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text + "\n" + expires}},
			map[string]any{"type": "actions", "elements": []any{map[string]any{"type": "button", "style": "primary",
				"text": map[string]any{"type": "plain_text", "text": "Open in Talyvor"}, "url": a.URL}}},
		},
	}
}
