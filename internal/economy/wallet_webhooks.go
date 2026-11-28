package economy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/safehttp"
)

// wallet_webhooks.go — B28.344: SIGNED, RETRIED WEBHOOKS FOR WALLET EVENTS.
//
// The owner adds an endpoint (an https url of theirs) and the events it takes. Every minute SweepWalletWebhooks queues
// one event per endpoint for each transfer in or out of the workspace, each approval asked for or decided, each limit or
// unusual-spend alert and each low balance since the endpoint was added, and POSTs the due ones. Nothing here writes to
// the ledger: the events are read from the rows the money paths already wrote, so a webhook can never hold up a payment.
//
// Each POST is signed the Standard Webhooks way, the format Track and Docs send too, so a customer verifies one format:
// webhook-id (the event's id, the same on every retry), webhook-timestamp (Unix seconds) and webhook-signature
// "v1,<base64 HMAC-SHA256 of id.timestamp.body>", keyed with the base64 part of the endpoint's whsec_ secret. For a day
// after a rotation the previous secret signs too, space-separated. An event answered 2xx is delivered and never sent
// again; any other answer is tried again after walletWebhookBackoff, and marked failed after its last attempt. Every
// attempt is a wallet_webhook_attempts row.

// Wallet webhook event types.
const (
	WalletEventTransfer         = "transfer.created"
	WalletEventApprovalRequest  = "approval.requested"
	WalletEventApprovalDecided  = "approval.decided"
	WalletEventAlertLimit       = "alert.limit"
	WalletEventAlertUnusual     = "alert.unusual_spend"
	WalletEventLowBalance       = "balance.low"
	walletWebhookRotationGrace  = 24 * time.Hour
	walletWebhookLease          = 2 * time.Minute // a sending sweep's hold on an event, so two replicas never POST it twice
	walletWebhookSecretPrefix   = "whsec_"
	walletWebhookBatch          = 200
	walletWebhookResponseLimit  = 1 << 16
	walletWebhookRequestTimeout = 10 * time.Second
)

// WalletEventTypes is every event an endpoint can take.
var WalletEventTypes = []string{WalletEventTransfer, WalletEventApprovalRequest, WalletEventApprovalDecided, WalletEventAlertLimit,
	WalletEventAlertUnusual, WalletEventLowBalance}

// walletWebhookBackoff is the wait after each failed attempt; after the last one the event is failed.
var walletWebhookBackoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 5 * time.Hour, 10 * time.Hour, 10 * time.Hour}

var (
	ErrWalletWebhook         = errors.New("economy: webhook")
	ErrWalletWebhookNotFound = errors.New("economy: no such webhook")
)

// WalletWebhook is one endpoint the workspace's wallet events are sent to.
type WalletWebhook struct {
	ID        string     `json:"id"`
	URL       string     `json:"url"`
	Events    []string   `json:"events"`           // none: every event
	Secret    string     `json:"secret,omitempty"` // only when it is added or rotated
	AddedBy   string     `json:"added_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	RotatedAt *time.Time `json:"rotated_at,omitempty"`
}

// WalletWebhookEvent is one event queued, delivered or failed to one endpoint.
type WalletWebhookEvent struct {
	ID          string          `json:"id"`
	WebhookID   string          `json:"webhook_id"`
	Type        string          `json:"type"`
	Ref         string          `json:"ref"`
	OccurredAt  time.Time       `json:"occurred_at"`
	Data        json.RawMessage `json:"data"`
	Status      string          `json:"status"` // queued, delivered or failed
	Attempts    int             `json:"attempts"`
	NextAttempt *time.Time      `json:"next_attempt_at,omitempty"`
	LastError   string          `json:"last_error,omitempty"`
	DeliveredAt *time.Time      `json:"delivered_at,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// AddWalletWebhook adds an endpoint: an https url on the public internet, and the events it takes (none: every one). It
// comes back with its signing secret, which is never shown again.
func (s *DualTokenStore) AddWalletWebhook(ctx context.Context, workspaceID, rawURL string, events []string, by string) (WalletWebhook, error) {
	h := WalletWebhook{URL: strings.TrimSpace(rawURL), Events: []string{}, AddedBy: by}
	if u, err := url.Parse(h.URL); err != nil || u.Scheme != "https" || u.Host == "" || !safehttp.AllowedURL(h.URL) {
		return WalletWebhook{}, fmt.Errorf("%w: the url must be https, on the public internet", ErrWalletWebhook)
	}
	for _, e := range events {
		if !slices.Contains(WalletEventTypes, e) {
			return WalletWebhook{}, fmt.Errorf("%w: %q is not an event; the events are %s", ErrWalletWebhook, e, strings.Join(WalletEventTypes, ", "))
		}
		if !slices.Contains(h.Events, e) {
			h.Events = append(h.Events, e)
		}
	}
	secret, err := newWalletWebhookSecret()
	if err != nil {
		return WalletWebhook{}, err
	}
	h.ID, h.Secret = "wwh_"+uuid.NewString(), secret
	err = s.pool.QueryRow(ctx, `INSERT INTO wallet_webhooks (id, workspace_id, url, events, secret, added_by) VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at`, h.ID, workspaceID, h.URL, h.Events, h.Secret, by).Scan(&h.CreatedAt)
	if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return WalletWebhook{}, fmt.Errorf("%w: that url is already added", ErrWalletWebhook)
	}
	if err != nil {
		return WalletWebhook{}, fmt.Errorf("economy: webhook: %w", err)
	}
	return h, nil
}

// WalletWebhooks is the workspace's endpoints, oldest first, without their secrets.
func (s *DualTokenStore) WalletWebhooks(ctx context.Context, workspaceID string) ([]WalletWebhook, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, url, events, added_by, created_at, rotated_at FROM wallet_webhooks WHERE workspace_id = $1
		ORDER BY created_at, id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: webhooks: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (WalletWebhook, error) {
		var h WalletWebhook
		err := row.Scan(&h.ID, &h.URL, &h.Events, &h.AddedBy, &h.CreatedAt, &h.RotatedAt)
		return h, err
	})
	if err != nil {
		return nil, fmt.Errorf("economy: webhooks: %w", err)
	}
	return out, nil
}

// RotateWalletWebhook gives an endpoint a new signing secret, shown this once. The old one still signs beside it for a
// day, so the owner's server can move over without refusing an event.
func (s *DualTokenStore) RotateWalletWebhook(ctx context.Context, workspaceID, id string, now time.Time) (WalletWebhook, error) {
	secret, err := newWalletWebhookSecret()
	if err != nil {
		return WalletWebhook{}, err
	}
	h := WalletWebhook{ID: id, Secret: secret}
	err = s.pool.QueryRow(ctx, `UPDATE wallet_webhooks SET previous_secret = secret, previous_until = $3, secret = $4, rotated_at = $5
		WHERE id = $1 AND workspace_id = $2 RETURNING url, events, added_by, created_at, rotated_at`,
		id, workspaceID, now.Add(walletWebhookRotationGrace), secret, now).Scan(&h.URL, &h.Events, &h.AddedBy, &h.CreatedAt, &h.RotatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return WalletWebhook{}, ErrWalletWebhookNotFound
	}
	if err != nil {
		return WalletWebhook{}, fmt.Errorf("economy: webhook: %w", err)
	}
	return h, nil
}

// DeleteWalletWebhook removes an endpoint, and with it its events.
func (s *DualTokenStore) DeleteWalletWebhook(ctx context.Context, workspaceID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM wallet_webhooks WHERE id = $1 AND workspace_id = $2`, id, workspaceID)
	if err != nil {
		return fmt.Errorf("economy: webhook: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrWalletWebhookNotFound
	}
	return nil
}

// WalletWebhookEvents is the workspace's latest 200 events, newest first.
func (s *DualTokenStore) WalletWebhookEvents(ctx context.Context, workspaceID string) ([]WalletWebhookEvent, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, webhook_id, event_type, ref, occurred_at, data, status, attempts,
		CASE WHEN status = 'queued' THEN next_attempt_at END, last_error, delivered_at, created_at
		FROM wallet_webhook_events WHERE workspace_id = $1 ORDER BY created_at DESC, id LIMIT 200`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: webhook events: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (WalletWebhookEvent, error) {
		var e WalletWebhookEvent
		err := row.Scan(&e.ID, &e.WebhookID, &e.Type, &e.Ref, &e.OccurredAt, &e.Data, &e.Status, &e.Attempts, &e.NextAttempt,
			&e.LastError, &e.DeliveredAt, &e.CreatedAt)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("economy: webhook events: %w", err)
	}
	return out, nil
}

// walletEventSources says, for each event type, the rows it is made of: a query selecting, per endpoint h of the
// workspace that takes it, the row's ref, when it happened and its data. $1 is the workspace, or "" for every one.
var walletEventSources = []struct{ typ, from string }{
	{WalletEventTransfer, `SELECT h.workspace_id, h.id, t.id, t.created_at, jsonb_build_object(
			'transfer_id', t.id, 'entry_id', t.entry_id, 'from_agent_id', t.from_agent_id, 'to_agent_id', t.to_agent_id,
			'direction', CASE WHEN t.from_workspace_id = t.to_workspace_id THEN 'internal'
				WHEN t.from_workspace_id = h.workspace_id THEN 'out' ELSE 'in' END,
			'amount_ulxc', t.amount_ulxc, 'currency', 'LXC', 'memo', t.memo, 'class', t.class, 'refund_of', t.refund_of,
			'postings', (SELECT COALESCE(jsonb_agg(jsonb_build_object('account', p.account, 'amount_ulxc', p.amount_ulxc) ORDER BY p.id), '[]')
				FROM agent_postings p WHERE p.entry_id = t.entry_id AND p.workspace_id = h.workspace_id),
			'created_at', t.created_at)
		FROM wallet_webhooks h JOIN agent_transfers t ON (t.from_workspace_id = h.workspace_id OR t.to_workspace_id = h.workspace_id)
			AND t.created_at >= h.created_at`},
	{WalletEventApprovalRequest, `SELECT h.workspace_id, h.id, a.id, a.created_at, jsonb_build_object(
			'approval_id', a.id, 'agent_id', a.agent_id, 'amount_ulxc', a.amount_ulxc, 'currency', a.currency,
			'amount_minor', a.amount_minor, 'reason', a.reason, 'expires_at', a.expires_at, 'created_at', a.created_at)
		FROM wallet_webhooks h JOIN agent_approvals a ON a.workspace_id = h.workspace_id AND a.created_at >= h.created_at`},
	{WalletEventApprovalDecided, `SELECT h.workspace_id, h.id, a.id, a.decided_at, jsonb_build_object(
			'approval_id', a.id, 'agent_id', a.agent_id, 'decision', CASE WHEN a.status = 'used' THEN 'approved' ELSE a.status END,
			'amount_ulxc', a.amount_ulxc, 'currency', a.currency, 'amount_minor', a.amount_minor, 'deny_reason', a.deny_reason,
			'decided_at', a.decided_at)
		FROM wallet_webhooks h JOIN agent_approvals a ON a.workspace_id = h.workspace_id AND a.decided_at >= h.created_at
			AND a.status IN ('approved', 'denied', 'used')`},
	{WalletEventAlertLimit, `SELECT h.workspace_id, h.id, a.id, a.created_at, jsonb_build_object(
			'alert_id', a.id, 'kind', a.kind, 'subject', a.subject, 'agent_id', a.agent_id, 'period', a.period,
			'percent', a.percent, 'spent_minor', a.spent_minor, 'limit_minor', a.limit_minor, 'currency', a.currency,
			'created_at', a.created_at)
		FROM wallet_webhooks h JOIN spend_alerts a ON a.workspace_id = h.workspace_id AND a.created_at >= h.created_at`},
	{WalletEventAlertUnusual, `SELECT h.workspace_id, h.id, a.id, a.created_at, jsonb_build_object(
			'alert_id', a.id, 'agent_id', a.agent_id, 'last_hour_ulxc', a.last_hour_ulxc,
			'usual_per_hour_ulxc', a.usual_per_hour_ulxc, 'paused', a.paused, 'created_at', a.created_at)
		FROM wallet_webhooks h JOIN agent_spend_alerts a ON a.workspace_id = h.workspace_id AND a.created_at >= h.created_at`},
	{WalletEventLowBalance, `SELECT h.workspace_id, h.id, a.id, a.created_at, jsonb_build_object(
			'alert_id', a.id, 'agent_id', a.agent_id, 'currency', a.currency, 'balance_minor', a.balance_minor,
			'threshold_minor', a.threshold_minor, 'created_at', a.created_at)
		FROM wallet_webhooks h JOIN low_balance_alerts a ON a.workspace_id = h.workspace_id AND a.created_at >= h.created_at`},
}

// QueueWalletWebhookEvents queues one event per endpoint for each wallet event since that endpoint was added and not yet
// queued to it, for workspaceID or every workspace when it is "". It returns how many it queued.
func (s *DualTokenStore) QueueWalletWebhookEvents(ctx context.Context, workspaceID string) (int, error) {
	queued := 0
	for _, src := range walletEventSources {
		tag, err := s.pool.Exec(ctx, `INSERT INTO wallet_webhook_events (id, workspace_id, webhook_id, event_type, ref, occurred_at, data)
			SELECT 'msg_' || gen_random_uuid()::text, e.ws, e.hook, $2, e.ref, e.at, e.data
			FROM (`+src.from+` WHERE ($1 = '' OR h.workspace_id = $1) AND (cardinality(h.events) = 0 OR $2 = ANY (h.events))
			) AS e (ws, hook, ref, at, data)
			ON CONFLICT (webhook_id, event_type, ref) DO NOTHING`, workspaceID, src.typ)
		if err != nil {
			return queued, fmt.Errorf("economy: webhook events (%s): %w", src.typ, err)
		}
		queued += int(tag.RowsAffected())
	}
	return queued, nil
}

// SendWalletWebhookEvents POSTs the due events — for workspaceID, or every workspace when it is "" — through client, and
// returns how many were delivered. Each is held for walletWebhookLease first, so a second sweep never sends it too.
func (s *DualTokenStore) SendWalletWebhookEvents(ctx context.Context, workspaceID string, client *http.Client, now time.Time) (int, error) {
	type due struct {
		id, ws, typ, url, secret, previous string
		previousUntil                      *time.Time
		occurred                           time.Time
		data                               json.RawMessage
		attempts                           int
	}
	rows, err := s.pool.Query(ctx, `WITH claimed AS (
			UPDATE wallet_webhook_events SET next_attempt_at = $3 WHERE id IN (
				SELECT id FROM wallet_webhook_events WHERE status = 'queued' AND next_attempt_at <= $2 AND ($1 = '' OR workspace_id = $1)
				ORDER BY next_attempt_at, id LIMIT $4 FOR UPDATE SKIP LOCKED)
			RETURNING id, workspace_id, webhook_id, event_type, occurred_at, data, attempts)
		SELECT c.id, c.workspace_id, c.event_type, h.url, h.secret, h.previous_secret, h.previous_until, c.occurred_at, c.data, c.attempts
		FROM claimed c JOIN wallet_webhooks h ON h.id = c.webhook_id ORDER BY c.occurred_at, c.id`,
		workspaceID, now, now.Add(walletWebhookLease), walletWebhookBatch)
	if err != nil {
		return 0, fmt.Errorf("economy: webhook events: %w", err)
	}
	todo, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (due, error) {
		var d due
		err := row.Scan(&d.id, &d.ws, &d.typ, &d.url, &d.secret, &d.previous, &d.previousUntil, &d.occurred, &d.data, &d.attempts)
		return d, err
	})
	if err != nil {
		return 0, fmt.Errorf("economy: webhook events: %w", err)
	}
	delivered := 0
	for _, d := range todo {
		secrets := []string{d.secret}
		if d.previous != "" && d.previousUntil != nil && now.Before(*d.previousUntil) {
			secrets = append(secrets, d.previous)
		}
		body, _ := json.Marshal(struct {
			Type      string          `json:"type"`
			Timestamp string          `json:"timestamp"`
			Data      json.RawMessage `json:"data"`
		}{d.typ, d.occurred.UTC().Format(time.RFC3339Nano), d.data})
		code, sendErr := postWalletWebhook(ctx, client, d.url, d.id, now, body, secrets)
		errText := ""
		if sendErr != nil {
			errText = sendErr.Error()
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO wallet_webhook_attempts (event_id, workspace_id, status_code, error, attempted_at)
			VALUES ($1, $2, $3, $4, $5)`, d.id, d.ws, code, errText, now); err != nil {
			return delivered, fmt.Errorf("economy: webhook attempt: %w", err)
		}
		if sendErr == nil {
			if _, err := s.pool.Exec(ctx, `UPDATE wallet_webhook_events SET status = 'delivered', attempts = attempts + 1, last_error = '',
				delivered_at = $2 WHERE id = $1`, d.id, now); err != nil {
				return delivered, fmt.Errorf("economy: webhook events: %w", err)
			}
			delivered++
			continue
		}
		status, next := "queued", now
		if d.attempts < len(walletWebhookBackoff) {
			next = now.Add(walletWebhookBackoff[d.attempts])
		} else {
			status = "failed"
		}
		if _, err := s.pool.Exec(ctx, `UPDATE wallet_webhook_events SET status = $2, attempts = attempts + 1, last_error = $3,
			next_attempt_at = $4 WHERE id = $1`, d.id, status, errText, next); err != nil {
			return delivered, fmt.Errorf("economy: webhook events: %w", err)
		}
	}
	return delivered, nil
}

// SweepWalletWebhooks queues the wallet events since the last sweep to every endpoint and sends the due ones.
func (s *DualTokenStore) SweepWalletWebhooks(ctx context.Context, client *http.Client, now time.Time) (queued, delivered int, err error) {
	if queued, err = s.QueueWalletWebhookEvents(ctx, ""); err != nil {
		return queued, 0, err
	}
	delivered, err = s.SendWalletWebhookEvents(ctx, "", client, now)
	return queued, delivered, err
}

// WalletWebhookSignature is one secret's Standard Webhooks signature of a POST: base64(HMAC-SHA256(key, id.timestamp.body)),
// the key being the secret's base64 part after whsec_.
func WalletWebhookSignature(secret, id, timestamp string, body []byte) (string, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, walletWebhookSecretPrefix))
	if err != nil {
		return "", fmt.Errorf("economy: webhook secret: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// postWalletWebhook POSTs body to to, signed with each of secrets; it returns the status answered (0 for none), and an
// error for anything but a 2xx.
func postWalletWebhook(ctx context.Context, client *http.Client, to, id string, now time.Time, body []byte, secrets []string) (int, error) {
	ts := strconv.FormatInt(now.Unix(), 10)
	var sigs []string
	for _, secret := range secrets {
		sig, err := WalletWebhookSignature(secret, id, ts, body)
		if err != nil {
			return 0, err
		}
		sigs = append(sigs, "v1,"+sig)
	}
	ctx, cancel := context.WithTimeout(ctx, walletWebhookRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, to, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", strings.Join(sigs, " "))
	resp, err := client.Do(req)
	if uerr := (*url.Error)(nil); errors.As(err, &uerr) {
		return 0, uerr.Err
	}
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, walletWebhookResponseLimit))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("answered %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func newWalletWebhookSecret() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("economy: webhook secret: %w", err)
	}
	return walletWebhookSecretPrefix + base64.StdEncoding.EncodeToString(key), nil
}
