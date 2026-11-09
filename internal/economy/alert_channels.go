package economy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/nativepush"
	"github.com/talyvor/lens/internal/safehttp"
)

// alert_channels.go — B28.338: ALERTS GO WHERE THE OWNER CHOOSES.
//
// The owner adds channels: push (their phones and browsers), a webhook (a URL of theirs, each POST signed) or Slack
// (an incoming webhook). Every minute SweepAlertChannels queues one delivery per channel for each alert raised since
// the channel was added — a limit alert (B53.4: 50, 80 or 100% of a budget, a card's cap or an agent's limit) or an
// unusual-spend alert (B19.6) — and sends the queued ones. A delivery that fails is tried again on the next sweep, up
// to alertDeliveryAttempts times. Email waits for an email integration: it is refused, never faked.
//
// A webhook's POST carries the alert as JSON, with X-Talyvor-Timestamp (Unix seconds) and X-Talyvor-Signature:
// "v1=" + hex(HMAC-SHA256(secret, timestamp + "." + body)). The secret is shown once, when the channel is added.

// Kinds of alert channel.
const (
	AlertChannelPush    = "push"
	AlertChannelWebhook = "webhook"
	AlertChannelSlack   = "slack"
	AlertChannelEmail   = "email" // refused until an email integration exists
)

// Kinds of alert a channel is sent.
const (
	AlertKindLimit        = "limit"         // a spend_alerts row
	AlertKindUnusualSpend = "unusual_spend" // an agent_spend_alerts row
)

// alertDeliveryAttempts is how many times a delivery is tried before it is marked failed.
const alertDeliveryAttempts = 5

// slackHookPrefix is where every Slack incoming webhook lives.
const slackHookPrefix = "https://hooks.slack.com/"

var (
	ErrAlertChannel         = errors.New("economy: alert channel")
	ErrAlertChannelNotFound = errors.New("economy: no such alert channel")
)

// AlertChannel is one place the workspace's alerts are sent.
type AlertChannel struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	URL       string    `json:"url,omitempty"`    // a webhook's URL; a Slack hook's, all but its end hidden
	Secret    string    `json:"secret,omitempty"` // a webhook's signing secret, only when it is added
	AddedBy   string    `json:"added_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// AlertDelivery is one alert sent, or to be sent, to one channel.
type AlertDelivery struct {
	ID          string     `json:"id"`
	ChannelID   string     `json:"channel_id"`
	ChannelKind string     `json:"channel_kind"`
	AlertKind   string     `json:"alert_kind"`
	AlertID     string     `json:"alert_id"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	Status      string     `json:"status"` // queued, sent or failed
	Attempts    int        `json:"attempts"`
	LastError   string     `json:"last_error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	SentAt      *time.Time `json:"sent_at,omitempty"`
}

// AddAlertChannel adds a channel: kind push, webhook (url: https, on the public internet) or slack (url: a Slack
// incoming webhook). A webhook's channel comes back with its signing secret, which is never shown again.
func (s *DualTokenStore) AddAlertChannel(ctx context.Context, workspaceID string, c AlertChannel, by string) (AlertChannel, error) {
	c.URL = strings.TrimSpace(c.URL)
	switch c.Kind {
	case AlertChannelPush:
		c.URL = ""
	case AlertChannelWebhook:
		if u, err := url.Parse(c.URL); err != nil || u.Scheme != "https" || !safehttp.AllowedURL(c.URL) {
			return AlertChannel{}, fmt.Errorf("%w: a webhook's url must be https, on the public internet", ErrAlertChannel)
		}
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return AlertChannel{}, fmt.Errorf("economy: alert channel: %w", err)
		}
		c.Secret = "whsec_" + hex.EncodeToString(secret)
	case AlertChannelSlack:
		if !strings.HasPrefix(c.URL, slackHookPrefix) || !safehttp.AllowedURL(c.URL) {
			return AlertChannel{}, fmt.Errorf("%w: a Slack channel's url is its incoming webhook, %s…", ErrAlertChannel, slackHookPrefix)
		}
	case AlertChannelEmail:
		return AlertChannel{}, fmt.Errorf("%w: email alerts need an email integration, which is not connected yet", ErrAlertChannel)
	default:
		return AlertChannel{}, fmt.Errorf("%w: kind must be push, webhook or slack", ErrAlertChannel)
	}
	c.ID, c.AddedBy = "ach_"+uuid.NewString(), by
	err := s.pool.QueryRow(ctx, `INSERT INTO alert_channels (id, workspace_id, kind, url, secret, added_by) VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at`, c.ID, workspaceID, c.Kind, c.URL, c.Secret, by).Scan(&c.CreatedAt)
	if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return AlertChannel{}, fmt.Errorf("%w: that channel is already added", ErrAlertChannel)
	}
	if err != nil {
		return AlertChannel{}, fmt.Errorf("economy: alert channel: %w", err)
	}
	c.URL = shownAlertURL(c.Kind, c.URL)
	return c, nil
}

// AlertChannels is the workspace's channels, oldest first, without their secrets.
func (s *DualTokenStore) AlertChannels(ctx context.Context, workspaceID string) ([]AlertChannel, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, kind, url, added_by, created_at FROM alert_channels WHERE workspace_id = $1
		ORDER BY created_at, id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: alert channels: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AlertChannel, error) {
		var c AlertChannel
		err := row.Scan(&c.ID, &c.Kind, &c.URL, &c.AddedBy, &c.CreatedAt)
		c.URL = shownAlertURL(c.Kind, c.URL)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("economy: alert channels: %w", err)
	}
	return out, nil
}

// DeleteAlertChannel removes a channel, and with it its deliveries.
func (s *DualTokenStore) DeleteAlertChannel(ctx context.Context, workspaceID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM alert_channels WHERE id = $1 AND workspace_id = $2`, id, workspaceID)
	if err != nil {
		return fmt.Errorf("economy: alert channel: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAlertChannelNotFound
	}
	return nil
}

// AlertDeliveries is the workspace's latest 200 deliveries, newest first.
func (s *DualTokenStore) AlertDeliveries(ctx context.Context, workspaceID string) ([]AlertDelivery, error) {
	rows, err := s.pool.Query(ctx, `SELECT d.id, d.channel_id, c.kind, d.alert_kind, d.alert_id, d.title, d.body, d.status, d.attempts,
		d.last_error, d.created_at, d.sent_at FROM alert_deliveries d JOIN alert_channels c ON c.id = d.channel_id
		WHERE d.workspace_id = $1 ORDER BY d.created_at DESC, d.id LIMIT 200`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: alert deliveries: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AlertDelivery, error) {
		var d AlertDelivery
		err := row.Scan(&d.ID, &d.ChannelID, &d.ChannelKind, &d.AlertKind, &d.AlertID, &d.Title, &d.Body, &d.Status, &d.Attempts,
			&d.LastError, &d.CreatedAt, &d.SentAt)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("economy: alert deliveries: %w", err)
	}
	return out, nil
}

// QueueAlertDeliveries queues one delivery per channel for each alert raised since that channel was added and not yet
// queued to it: for workspaceID, or for every workspace when it is "". It returns how many it queued.
func (s *DualTokenStore) QueueAlertDeliveries(ctx context.Context, workspaceID string) (int, error) {
	type pending struct{ ws, channel, kind, alert, title, body string }
	var todo []pending
	rows, err := s.pool.Query(ctx, `SELECT c.workspace_id, c.id, a.id, a.kind, a.subject, a.agent_id, a.category, a.period, a.period_start,
		a.percent, a.spent_minor, a.limit_minor, a.currency, a.created_at
		FROM alert_channels c JOIN spend_alerts a ON a.workspace_id = c.workspace_id AND a.created_at >= c.created_at
		WHERE ($1 = '' OR c.workspace_id = $1) AND NOT EXISTS (SELECT 1 FROM alert_deliveries d
			WHERE d.channel_id = c.id AND d.alert_kind = 'limit' AND d.alert_id = a.id)
		ORDER BY a.created_at, a.id LIMIT 1000`, workspaceID)
	if err != nil {
		return 0, fmt.Errorf("economy: alert deliveries: %w", err)
	}
	for rows.Next() {
		var p pending
		var a SpendAlert
		if err := rows.Scan(&p.ws, &p.channel, &a.ID, &a.Kind, &a.Subject, &a.AgentID, &a.Category, &a.Period, &a.PeriodStart,
			&a.Percent, &a.SpentMinor, &a.LimitMinor, &a.Currency, &a.CreatedAt); err != nil {
			rows.Close()
			return 0, fmt.Errorf("economy: alert deliveries: %w", err)
		}
		n := alertNotice(a)
		p.kind, p.alert, p.title, p.body = AlertKindLimit, a.ID, n.Title, n.Body
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("economy: alert deliveries: %w", err)
	}
	rows, err = s.pool.Query(ctx, `SELECT c.workspace_id, c.id, a.id, a.last_hour_ulxc, a.usual_per_hour_ulxc, a.paused
		FROM alert_channels c JOIN agent_spend_alerts a ON a.workspace_id = c.workspace_id AND a.created_at >= c.created_at
		WHERE ($1 = '' OR c.workspace_id = $1) AND NOT EXISTS (SELECT 1 FROM alert_deliveries d
			WHERE d.channel_id = c.id AND d.alert_kind = 'unusual_spend' AND d.alert_id = a.id)
		ORDER BY a.created_at, a.id LIMIT 1000`, workspaceID)
	if err != nil {
		return 0, fmt.Errorf("economy: alert deliveries: %w", err)
	}
	for rows.Next() {
		var p pending
		var lastHour, usual int64
		var paused bool
		if err := rows.Scan(&p.ws, &p.channel, &p.alert, &lastHour, &usual, &paused); err != nil {
			rows.Close()
			return 0, fmt.Errorf("economy: alert deliveries: %w", err)
		}
		p.kind, p.title = AlertKindUnusualSpend, "Unusual spend"
		p.body = fmt.Sprintf("An agent spent %s in the last hour; it usually spends %s an hour.", Money(lastHour, "LXC"), Money(usual, "LXC"))
		if paused {
			p.body += " It is paused until you resume it."
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("economy: alert deliveries: %w", err)
	}
	queued := 0
	for _, p := range todo {
		tag, err := s.pool.Exec(ctx, `INSERT INTO alert_deliveries (id, workspace_id, channel_id, alert_kind, alert_id, title, body)
			VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (channel_id, alert_kind, alert_id) DO NOTHING`,
			"ald_"+uuid.NewString(), p.ws, p.channel, p.kind, p.alert, p.title, p.body)
		if err != nil {
			return queued, fmt.Errorf("economy: alert deliveries: %w", err)
		}
		queued += int(tag.RowsAffected())
	}
	return queued, nil
}

// SendAlertDeliveries sends the queued deliveries — for workspaceID, or every workspace when it is "" — through client,
// and returns how many were sent. A failure is kept queued to be tried again, and marked failed on its last attempt.
func (s *DualTokenStore) SendAlertDeliveries(ctx context.Context, workspaceID string, client *http.Client, now time.Time) (int, error) {
	type queued struct {
		AlertDelivery
		ws, url, secret string
	}
	rows, err := s.pool.Query(ctx, `SELECT d.id, d.workspace_id, d.channel_id, c.kind, c.url, c.secret, d.alert_kind, d.alert_id, d.title,
		d.body, d.attempts, d.created_at FROM alert_deliveries d JOIN alert_channels c ON c.id = d.channel_id
		WHERE d.status = 'queued' AND ($1 = '' OR d.workspace_id = $1) ORDER BY d.created_at, d.id LIMIT 200`, workspaceID)
	if err != nil {
		return 0, fmt.Errorf("economy: alert deliveries: %w", err)
	}
	todo, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (queued, error) {
		var q queued
		err := row.Scan(&q.ID, &q.ws, &q.ChannelID, &q.ChannelKind, &q.url, &q.secret, &q.AlertKind, &q.AlertID, &q.Title, &q.Body,
			&q.Attempts, &q.CreatedAt)
		return q, err
	})
	if err != nil {
		return 0, fmt.Errorf("economy: alert deliveries: %w", err)
	}
	sent := 0
	for _, q := range todo {
		var sendErr error
		switch q.ChannelKind {
		case AlertChannelPush:
			sendErr = s.pushAlert(ctx, q.ws, q.AlertDelivery)
		case AlertChannelWebhook:
			sendErr = postAlertWebhook(ctx, client, q.url, q.secret, q.ws, q.AlertDelivery, now)
		case AlertChannelSlack:
			body, _ := json.Marshal(map[string]string{"text": "*" + q.Title + "*\n" + q.Body})
			sendErr = postAlert(ctx, client, q.url, body, nil)
		}
		if sendErr == nil {
			if _, err := s.pool.Exec(ctx, `UPDATE alert_deliveries SET status = 'sent', attempts = attempts + 1, last_error = '',
				sent_at = $2 WHERE id = $1`, q.ID, now); err != nil {
				return sent, fmt.Errorf("economy: alert deliveries: %w", err)
			}
			sent++
			continue
		}
		status := "queued"
		if q.Attempts+1 >= alertDeliveryAttempts {
			status = "failed"
		}
		if _, err := s.pool.Exec(ctx, `UPDATE alert_deliveries SET status = $2, attempts = attempts + 1, last_error = $3 WHERE id = $1`,
			q.ID, status, sendErr.Error()); err != nil {
			return sent, fmt.Errorf("economy: alert deliveries: %w", err)
		}
	}
	return sent, nil
}

// SweepAlertChannels queues the alerts raised since the last sweep to every channel and sends what is queued.
func (s *DualTokenStore) SweepAlertChannels(ctx context.Context, client *http.Client, now time.Time) (queued, sent int, err error) {
	if queued, err = s.QueueAlertDeliveries(ctx, ""); err != nil {
		return queued, 0, err
	}
	sent, err = s.SendAlertDeliveries(ctx, "", client, now)
	return queued, sent, err
}

// pushAlert sends d to the owner's phones and browsers — unless d is a limit alert whose spend notice (B53.7) has
// already been pushed, so the owner is not told twice. With neither native nor web push set up it is an error.
func (s *DualTokenStore) pushAlert(ctx context.Context, workspaceID string, d AlertDelivery) error {
	if s.nativePusher == nil && s.approvalPusher == nil {
		return errors.New("push is not set up on this server")
	}
	if d.AlertKind == AlertKindLimit {
		var pushed bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM spend_notices WHERE workspace_id = $1 AND kind = 'budget'
			AND subject = $2 AND status = 'sent')`, workspaceID, d.AlertID).Scan(&pushed); err == nil && pushed {
			return nil
		}
	}
	s.pushDevices(ctx, workspaceID, nativepush.Message{Title: d.Title, Body: d.Body, Data: map[string]string{
		"type": "alert", "alert_kind": d.AlertKind, "alert_id": d.AlertID, "workspace_id": workspaceID}})
	payload, _ := json.Marshal(map[string]any{"type": "alert", "alert_kind": d.AlertKind, "alert_id": d.AlertID,
		"workspace_id": workspaceID, "title": d.Title, "body": d.Body})
	s.pushWeb(ctx, workspaceID, payload)
	return nil
}

// postAlertWebhook POSTs d to a webhook, signed with its secret.
func postAlertWebhook(ctx context.Context, client *http.Client, to, secret, workspaceID string, d AlertDelivery, now time.Time) error {
	body, _ := json.Marshal(map[string]any{"type": "alert", "id": d.ID, "workspace_id": workspaceID, "alert_kind": d.AlertKind,
		"alert_id": d.AlertID, "title": d.Title, "body": d.Body, "created_at": d.CreatedAt.UTC().Format(time.RFC3339)})
	ts := strconv.FormatInt(now.Unix(), 10)
	return postAlert(ctx, client, to, body, map[string]string{"X-Talyvor-Timestamp": ts,
		"X-Talyvor-Signature": "v1=" + AlertSignature(secret, ts, body)})
}

// AlertSignature is a webhook POST's signature: hex(HMAC-SHA256(secret, timestamp + "." + body)).
func AlertSignature(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// postAlert POSTs body as JSON to to; anything but a 2xx is an error.
func postAlert(ctx context.Context, client *http.Client, to string, body []byte, headers map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, to, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if uerr := (*url.Error)(nil); errors.As(err, &uerr) {
		return uerr.Err // without the url: a Slack hook's is its credential, and the error is shown to the owner
	}
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("answered %d", resp.StatusCode)
	}
	return nil
}

// shownAlertURL is a channel's url as it is shown: a Slack hook's is its credential, so only its last four characters.
func shownAlertURL(kind, u string) string {
	if kind != AlertChannelSlack || len(u) <= len(slackHookPrefix)+4 {
		return u
	}
	return slackHookPrefix + "…" + u[len(u)-4:]
}
