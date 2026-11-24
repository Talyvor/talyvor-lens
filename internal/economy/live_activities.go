package economy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/nativepush"
)

// B41.136 — THE HELD-PAYMENT LIVE ACTIVITY WITH THE APP CLOSED. iOS lets only the open app start a Live Activity, so
// the phone app (B41.36) gives Lens its ActivityKit push-to-start token and each activity's own push token. Each held
// payment — a request prepared for approval (pr:<id>) or an agent's held payment (ap:<id>) — is then pushed to every
// iPhone of the workspace as the activity's start, and once it is approved or declined each activity showing it is
// sent its end, showing which. What the activity shows is what the app writes itself (talyvor-mobile
// src/live-activity.ios.ts heldPayments): the payee, the amount in the currency that pays it, who asked.

// LiveActivityPusher sends one Live Activity push (internal/nativepush.Senders); the native pusher is one when it can.
type LiveActivityPusher interface {
	SendLiveActivity(ctx context.Context, platform, token string, la nativepush.LiveActivity) error
}

// liveActivityName is the app's Live Activity layout (createLiveActivity('TalyvorApproval', …)).
const liveActivityName = "TalyvorApproval"

// liveActivityKey is a held payment as the app keys its activity.
var liveActivityKey = regexp.MustCompile(`^(pr|ap):[A-Za-z0-9_-]{1,120}$`)

// liveActivityPalette is the five colours of the app's palette the activity's layout reads (talyvor-mobile src/theme.ts).
var liveActivityPalette = map[string]map[string]string{
	"light": {"page": "#FFFFFF", "ink": "#111216", "ink2": "#4A5061", "tangerineInk": "#A84100", "success": "#0B7A45"},
	"dark":  {"page": "#0F1013", "ink": "#F2F3F5", "ink2": "#A6ACB9", "tangerineInk": "#FF8A33", "success": "#3DD68C"},
}

// liveActivityProps is the layout's props (ApprovalActivityProps).
type liveActivityProps struct {
	State   string                       `json:"state"` // waiting | approved | declined | expired
	Title   string                       `json:"title"`
	Amount  string                       `json:"amount"`
	Detail  string                       `json:"detail"`
	Hint    string                       `json:"hint"`
	Palette map[string]map[string]string `json:"palette"`
}

func (s *DualTokenStore) liveActivityPusher() LiveActivityPusher {
	p, _ := s.nativePusher.(LiveActivityPusher)
	return p
}

// SetDeviceLiveActivityToken keeps token as iPhone id's push-to-start token; "" removes it. A revoked key takes none.
func (s *DualTokenStore) SetDeviceLiveActivityToken(ctx context.Context, workspaceID, id, token string) (DeviceKey, error) {
	platform, err := s.liveDevice(ctx, workspaceID, id)
	if err != nil {
		return DeviceKey{}, err
	}
	if token != "" && platform != "test" && !nativepush.ValidToken("ios", token) {
		return DeviceKey{}, fmt.Errorf("%w: push_to_start_token is not an ActivityKit push-to-start token", ErrAgentRule)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE workspace_device_keys SET live_activity_token = $3 WHERE id = $1 AND workspace_id = $2`, id, workspaceID, token); err != nil {
		return DeviceKey{}, fmt.Errorf("economy: live activity token: %w", err)
	}
	return scanDeviceKey(s.pool.QueryRow(ctx, `SELECT `+deviceKeyCols+` FROM workspace_device_keys WHERE id = $1`, id))
}

// SetLiveActivityPushToken keeps the push token of the Live Activity iPhone id shows held payment key by. One for a
// payment decided already is sent its end at once.
func (s *DualTokenStore) SetLiveActivityPushToken(ctx context.Context, workspaceID, id, key, token string) error {
	if !liveActivityKey.MatchString(key) {
		return fmt.Errorf("%w: key must be pr:<payment request id> or ap:<approval id>", ErrAgentRule)
	}
	platform, err := s.liveDevice(ctx, workspaceID, id)
	if err != nil {
		return err
	}
	if platform != "test" && !nativepush.ValidToken("ios", token) {
		return fmt.Errorf("%w: push_token is not a Live Activity push token", ErrAgentRule)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO workspace_live_activities (workspace_id, device_id, held_key, push_token) VALUES ($1, $2, $3, $4)
		ON CONFLICT (device_id, held_key) DO UPDATE SET push_token = EXCLUDED.push_token WHERE workspace_live_activities.ended_at IS NULL`,
		workspaceID, id, key, token); err != nil {
		return fmt.Errorf("economy: live activity: %w", err)
	}
	if state := s.heldOutcome(ctx, workspaceID, key); state != "" {
		s.endLiveActivity(workspaceID, key, state)
	}
	return nil
}

// liveDevice is the platform of workspaceID's unrevoked device id, which must be an iPhone (or the web's test key).
func (s *DualTokenStore) liveDevice(ctx context.Context, workspaceID, id string) (string, error) {
	var platform string
	err := s.pool.QueryRow(ctx, `SELECT platform FROM workspace_device_keys WHERE id = $1 AND workspace_id = $2 AND revoked_at IS NULL`,
		id, workspaceID).Scan(&platform)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrDeviceKeyNotFound
	}
	if err != nil {
		return "", fmt.Errorf("economy: device key: %w", err)
	}
	if platform != "ios" && platform != "test" {
		return "", fmt.Errorf("%w: Live Activities are an iPhone's alone", ErrAgentRule)
	}
	return platform, nil
}

// heldOutcome is how held payment key was decided — approved, declined or expired — or "" while it waits or cannot be read.
func (s *DualTokenStore) heldOutcome(ctx context.Context, workspaceID, key string) string {
	var status string
	switch key[:3] {
	case "pr:":
		r, err := s.PaymentRequest(ctx, workspaceID, "", key[3:])
		if err != nil || r.Status == "pending_approval" {
			return ""
		}
		status = r.Status
	case "ap:":
		a, err := s.GetAgentApproval(ctx, workspaceID, key[3:])
		if err != nil || a.Status == "pending" {
			return ""
		}
		status = a.Status
	}
	switch status {
	case "expired":
		return "expired"
	case "approved", "used", "auto":
		return "approved"
	}
	return "declined"
}

// heldActivity is what held payment key's activity shows in state, and the link a tap opens.
func (s *DualTokenStore) heldActivity(ctx context.Context, workspaceID, key, state string) (liveActivityProps, string, error) {
	p := liveActivityProps{State: state, Hint: "Tap to approve with Face ID", Palette: liveActivityPalette}
	id := key[3:]
	if key[:3] == "pr:" {
		r, err := s.PaymentRequest(ctx, workspaceID, "", id)
		if err != nil {
			return p, "", err
		}
		var asked []PaymentRequestItem
		for _, it := range r.Items {
			if it.Decision == ItemNeedsApproval {
				asked = append(asked, it)
			}
		}
		switch {
		case len(asked) == 1:
			p.Title = asked[0].PayeeName
		case r.Summary != "":
			p.Title = r.Summary
		default:
			p.Title = fmt.Sprintf("%d payments", len(asked))
		}
		p.Amount = liveTotals(asked)
		p.Detail = "From the company’s account"
		if r.AgentID != "" {
			name, err := agentName(ctx, s.pool, r.AgentID)
			if err != nil {
				return p, "", err
			}
			p.Detail = name + " asks"
		}
		return p, "talyvor://approve/" + url.PathEscape(id), nil
	}
	a, err := s.GetAgentApproval(ctx, workspaceID, id)
	if err != nil {
		return p, "", err
	}
	switch {
	case a.Payee != nil && a.Payee.Name != "":
		p.Title = a.Payee.Name
	case a.Payee != nil:
		p.Title = a.Payee.ID
	default:
		p.Title = "Ask " + a.Model
	}
	// In the currency that pays it, as its approval sheet writes it: an account's or a card's own, else dollars (B61.15).
	p.Amount = LXCMoney(a.AmountULXC)
	if f := a.PaidFrom; f != nil && (f.Kind == "account" || f.Kind == "card") && f.Currency != "" {
		p.Amount = Money(f.AmountMinor, f.Currency)
	}
	name, err := agentName(ctx, s.pool, a.AgentID)
	if err != nil {
		return p, "", err
	}
	p.Detail = name + " asks"
	return p, "talyvor://approvals/" + url.PathEscape(id), nil
}

// liveTotals is the items' total in each currency, dollars first: "$340.00 + €12.00", as the app adds them.
func liveTotals(items []PaymentRequestItem) string {
	var order []string
	sum := map[string]int64{}
	for _, it := range items {
		if _, ok := sum[it.Currency]; !ok {
			order = append(order, it.Currency)
		}
		sum[it.Currency] += it.AmountMinor
	}
	if i := slices.Index(order, "USD"); i > 0 {
		order = append([]string{"USD"}, slices.Delete(order, i, i+1)...)
	}
	out := ""
	for i, c := range order {
		if i > 0 {
			out += " + "
		}
		out += Money(sum[c], c)
	}
	return out
}

// startLiveActivity pushes held payment key to each of workspaceID's unrevoked iPhones with a push-to-start token as
// its Live Activity's start, forgetting the tokens APNs says are gone.
func (s *DualTokenStore) startLiveActivity(ctx context.Context, workspaceID, key string) {
	pusher := s.liveActivityPusher()
	if pusher == nil {
		return
	}
	rows, err := s.pool.Query(ctx, `SELECT id, platform, live_activity_token FROM workspace_device_keys
		WHERE workspace_id = $1 AND revoked_at IS NULL AND live_activity_token <> ''`, workspaceID)
	if err != nil {
		slog.Warn("live activity: devices", slog.String("err", err.Error()))
		return
	}
	var devices [][3]string
	for rows.Next() {
		var d [3]string
		if rows.Scan(&d[0], &d[1], &d[2]) == nil {
			devices = append(devices, d)
		}
	}
	rows.Close()
	if len(devices) == 0 {
		return
	}
	props, link, err := s.heldActivity(ctx, workspaceID, key, "waiting")
	if err != nil {
		slog.Warn("live activity: held payment", slog.String("err", err.Error()))
		return
	}
	raw, _ := json.Marshal(props)
	la := nativepush.LiveActivity{Event: "start", Name: liveActivityName, Props: raw, URL: link,
		Alert: nativepush.Message{Title: "Approval needed", Body: props.Title + " · " + props.Amount}}
	for _, d := range devices {
		err := pusher.SendLiveActivity(ctx, d[1], d[2], la)
		switch {
		case errors.Is(err, nativepush.ErrGone):
			_, _ = s.pool.Exec(ctx, `UPDATE workspace_device_keys SET live_activity_token = '' WHERE id = $1 AND live_activity_token = $2`, d[0], d[2])
		case err != nil:
			slog.Warn("live activity: start", slog.String("err", err.Error()))
		}
	}
}

// endLiveActivity sends, in the background, each open Live Activity showing held payment key its end in state
// (approved, declined or expired); each is ended once.
func (s *DualTokenStore) endLiveActivity(workspaceID, key, state string) {
	pusher := s.liveActivityPusher()
	if pusher == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		rows, err := s.pool.Query(ctx, `UPDATE workspace_live_activities l SET ended_at = now() FROM workspace_device_keys d
			WHERE l.workspace_id = $1 AND l.held_key = $2 AND l.ended_at IS NULL AND d.id = l.device_id AND d.workspace_id = l.workspace_id
				AND d.revoked_at IS NULL
			RETURNING d.platform, l.push_token`, workspaceID, key)
		if err != nil {
			slog.Warn("live activity: activities", slog.String("err", err.Error()))
			return
		}
		var tokens [][2]string
		for rows.Next() {
			var t [2]string
			if rows.Scan(&t[0], &t[1]) == nil {
				tokens = append(tokens, t)
			}
		}
		rows.Close()
		if len(tokens) == 0 {
			return
		}
		props, link, err := s.heldActivity(ctx, workspaceID, key, state)
		if err != nil {
			slog.Warn("live activity: held payment", slog.String("err", err.Error()))
			return
		}
		raw, _ := json.Marshal(props)
		for _, t := range tokens {
			if err := pusher.SendLiveActivity(ctx, t[0], t[1], nativepush.LiveActivity{Event: "end", Name: liveActivityName, Props: raw, URL: link}); err != nil &&
				!errors.Is(err, nativepush.ErrGone) {
				slog.Warn("live activity: end", slog.String("err", err.Error()))
			}
		}
	}()
}

// endDecidedApprovals ends the activities of agent approvals just decided: approved, or declined. One still pending
// (the first of two approvals over the second-approver line, B28.315) keeps its activity.
func (s *DualTokenStore) endDecidedApprovals(workspaceID string, approvals ...AgentApproval) {
	for _, a := range approvals {
		switch a.Status {
		case "approved":
			s.endLiveActivity(workspaceID, "ap:"+a.ID, "approved")
		case "denied":
			s.endLiveActivity(workspaceID, "ap:"+a.ID, "declined")
		}
	}
}
