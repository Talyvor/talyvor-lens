package economy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/nativepush"
)

// spend_notices.go — B53.7: NOTICES THAT ARRIVE AT THE RIGHT TIME.
//
// Every minute SweepSpendNotices turns what happened to a workspace's money into notices and sends the ones that are
// due to the owner's devices: web push, and the phone app's native push (B40.15), which stays quiet, without an error,
// until LENS_APNS_* or LENS_FCM_CREDENTIALS are set. The kinds:
//
//	payment — each payment: its amount, its category and what is left of the budget it counts against (B53.4)
//	budget  — each budget, card-cap and agent-limit alert, as it is raised (B53.4)
//	unusual — each unusual payment (B53.5), in the Coach's words; urgent, so it ignores quiet hours
//	tip     — the Coach's sentence on a budget at half or more (B53.6), from 08:00; at most one tip is sent a day
//	digest  — on Monday from 08:00, last week's spending in the Coach's review
//
// The owner sets quiet hours in their own time zone and can switch any kind off. A notice that is not urgent and falls
// due in quiet hours waits until they end; a tip over the day's one waits, and is dropped when its day is over. Every
// notice, sent or not, is listed for the Spending screen.

// Kinds of notice.
const (
	NoticePayment = "payment"
	NoticeBudget  = "budget"
	NoticeUnusual = "unusual"
	NoticeTip     = "tip"
	NoticeDigest  = "digest"
)

// NoticeKinds is every kind of notice, each of which the owner can switch off.
var NoticeKinds = []string{NoticePayment, NoticeBudget, NoticeUnusual, NoticeTip, NoticeDigest}

// Why a waiting notice waits.
const (
	NoticeHeldQuiet = "quiet_hours"
	NoticeHeldTip   = "daily_tip"
)

// noticeMorningHour is the hour, in the owner's time zone, from which the day's tip and Monday's digest are written.
const noticeMorningHour = 8

// SpendNoticeSettings is when and what the owner wants to be told.
type SpendNoticeSettings struct {
	Timezone   string   `json:"timezone"`
	QuietFrom  string   `json:"quiet_from"`  // HH:MM; empty: no quiet hours
	QuietUntil string   `json:"quiet_until"` // HH:MM; before quiet_from when they run past midnight
	Off        []string `json:"off"`         // the kinds switched off
}

// SpendNotice is one notice and whether it was sent.
type SpendNotice struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	Urgent    bool           `json:"urgent"`
	Data      map[string]any `json:"data"`
	Status    string         `json:"status"` // waiting, sent or dropped
	Held      string         `json:"held,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	SentAt    *time.Time     `json:"sent_at,omitempty"`
}

// quiet reports whether local falls in the quiet hours.
func (n SpendNoticeSettings) quiet(local time.Time) bool {
	if n.QuietFrom == "" {
		return false
	}
	from, _ := clockMinutes(n.QuietFrom)
	until, _ := clockMinutes(n.QuietUntil)
	m := local.Hour()*60 + local.Minute()
	if from < until {
		return m >= from && m < until
	}
	return m >= from || m < until
}

func (s *DualTokenStore) noticeSettings(ctx context.Context, workspaceID string) (SpendNoticeSettings, *time.Time, error) {
	out := SpendNoticeSettings{Timezone: "UTC", Off: []string{}}
	var seen *time.Time
	err := s.pool.QueryRow(ctx, `SELECT timezone, quiet_from, quiet_until, off, seen_until FROM spend_notice_settings WHERE workspace_id = $1`,
		workspaceID).Scan(&out.Timezone, &out.QuietFrom, &out.QuietUntil, &out.Off, &seen)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, nil, fmt.Errorf("economy: notice settings: %w", err)
	}
	return out, seen, nil
}

// SpendNoticeSettings is workspaceID's notice settings; every kind is on, with no quiet hours, until the owner sets them.
func (s *DualTokenStore) SpendNoticeSettings(ctx context.Context, workspaceID string) (SpendNoticeSettings, error) {
	out, _, err := s.noticeSettings(ctx, workspaceID)
	return out, err
}

// SetSpendNoticeSettings saves workspaceID's notice settings and answers them as saved.
func (s *DualTokenStore) SetSpendNoticeSettings(ctx context.Context, workspaceID string, in SpendNoticeSettings) (SpendNoticeSettings, error) {
	if in.Timezone == "" {
		in.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil {
		return SpendNoticeSettings{}, fmt.Errorf("%w: timezone %q is not an IANA time zone", ErrSpending, in.Timezone)
	}
	if (in.QuietFrom == "") != (in.QuietUntil == "") {
		return SpendNoticeSettings{}, fmt.Errorf("%w: set both quiet_from and quiet_until, or neither", ErrSpending)
	}
	if in.QuietFrom != "" {
		from, err := clockMinutes(in.QuietFrom)
		if err != nil {
			return SpendNoticeSettings{}, fmt.Errorf("%w: quiet_from: %v", ErrSpending, err)
		}
		until, err := clockMinutes(in.QuietUntil)
		if err != nil {
			return SpendNoticeSettings{}, fmt.Errorf("%w: quiet_until: %v", ErrSpending, err)
		}
		if from == until {
			return SpendNoticeSettings{}, fmt.Errorf("%w: quiet_from and quiet_until must differ", ErrSpending)
		}
		in.QuietFrom, in.QuietUntil = fmt.Sprintf("%02d:%02d", from/60, from%60), fmt.Sprintf("%02d:%02d", until/60, until%60)
	}
	off := []string{}
	for _, k := range in.Off {
		if !slices.Contains(NoticeKinds, k) {
			return SpendNoticeSettings{}, fmt.Errorf("%w: off may name only %s", ErrSpending, strings.Join(NoticeKinds, ", "))
		}
		if !slices.Contains(off, k) {
			off = append(off, k)
		}
	}
	in.Off = off
	if _, err := s.pool.Exec(ctx, `INSERT INTO spend_notice_settings (workspace_id, timezone, quiet_from, quiet_until, off) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (workspace_id) DO UPDATE SET timezone = EXCLUDED.timezone, quiet_from = EXCLUDED.quiet_from,
		quiet_until = EXCLUDED.quiet_until, off = EXCLUDED.off, updated_at = now()`,
		workspaceID, in.Timezone, in.QuietFrom, in.QuietUntil, in.Off); err != nil {
		return SpendNoticeSettings{}, fmt.Errorf("economy: notice settings: %w", err)
	}
	return in, nil
}

// SpendNotices is workspaceID's latest notices, newest first.
func (s *DualTokenStore) SpendNotices(ctx context.Context, workspaceID string) ([]SpendNotice, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, kind, title, body, urgent, data, status, held, created_at, sent_at FROM spend_notices
		WHERE workspace_id = $1 ORDER BY created_at DESC, id LIMIT 100`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: notices: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (SpendNotice, error) {
		var n SpendNotice
		err := row.Scan(&n.ID, &n.Kind, &n.Title, &n.Body, &n.Urgent, &n.Data, &n.Status, &n.Held, &n.CreatedAt, &n.SentAt)
		return n, err
	})
	if err != nil {
		return nil, fmt.Errorf("economy: notices: %w", err)
	}
	return out, nil
}

// SweepSpendNotices runs NotifySpending for every workspace with a payment in the last eight days, an alert, or a
// notice waiting, and answers how many notices it sent.
// shortcut: reads eight days of entries each minute to find them; keep a list of active workspaces when that grows slow.
func (s *DualTokenStore) SweepSpendNotices(ctx context.Context, ai CoachInferer, model string, now time.Time) (int, error) {
	week := now.AddDate(0, 0, -8)
	rows, err := s.pool.Query(ctx, `SELECT workspace_id FROM money_entries WHERE kind IN ('card_charge', 'payment_out') AND created_at >= $1
		UNION SELECT workspace_id FROM spend_alerts WHERE created_at >= $1
		UNION SELECT workspace_id FROM spend_notices WHERE status = 'waiting'`, week)
	if err != nil {
		return 0, fmt.Errorf("economy: sweep notices: %w", err)
	}
	workspaces, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("economy: sweep notices: %w", err)
	}
	n := 0
	var errs []error
	for _, ws := range workspaces {
		wsCtx, cancel := context.WithTimeout(ctx, 40*time.Second) // the Coach's model calls take up to 20s
		sent, err := s.NotifySpending(wsCtx, ws, ai, model, now)
		cancel()
		n += sent
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ws, err))
		}
	}
	return n, errors.Join(errs...)
}

// NotifySpending makes notices of workspaceID's payments, alerts and unusual payments since its last sweep, and of its
// tip and digest when they are due; sends the waiting notices that may go at now; and answers how many it sent.
func (s *DualTokenStore) NotifySpending(ctx context.Context, workspaceID string, ai CoachInferer, model string, now time.Time) (int, error) {
	set, seen, err := s.noticeSettings(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	loc, err := time.LoadLocation(set.Timezone)
	if err != nil {
		loc = time.UTC
	}
	local := now.In(loc)
	on := func(kind string) bool { return !slices.Contains(set.Off, kind) }
	from := now.Add(-5 * time.Minute)
	if seen != nil {
		from = *seen
	}
	from = from.Add(-2 * time.Minute) // a payment committed just after the last read still counts; the unique key keeps it once
	add := func(n SpendNotice, subject string) error {
		if n.Status == "" {
			n.Status = "waiting"
		}
		data, _ := json.Marshal(n.Data)
		_, err := s.pool.Exec(ctx, `INSERT INTO spend_notices (id, workspace_id, kind, subject, title, body, urgent, data, status, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT (workspace_id, kind, subject) DO NOTHING`,
			"snt_"+uuid.NewString(), workspaceID, n.Kind, subject, n.Title, n.Body, n.Urgent, data, n.Status, now)
		if err != nil {
			return fmt.Errorf("economy: notice: %w", err)
		}
		return nil
	}
	var budgets []SpendBudget
	loadBudgets := func() error {
		if budgets != nil {
			return nil
		}
		budgets, err = s.SpendBudgets(ctx, workspaceID, now)
		if budgets == nil {
			budgets = []SpendBudget{}
		}
		return err
	}
	var report *CoachReport
	coach := func() (CoachReport, error) {
		if report == nil {
			r, err := Coach(ctx, s, ai, model, workspaceID, now)
			if err != nil {
				return CoachReport{}, err
			}
			report = &r
		}
		return *report, nil
	}

	sp, err := s.Spending(ctx, workspaceID, from, now.Add(time.Second))
	if err != nil {
		return 0, err
	}
	if len(sp.Payments) > 0 && on(NoticePayment) {
		if err := loadBudgets(); err != nil {
			return 0, err
		}
		for i, p := range sp.Payments { // newest first: what came after p is sp.Payments[:i]
			if err := add(paymentNotice(p, sp.Payments[:i], budgets), p.EntryID); err != nil {
				return 0, err
			}
		}
	}
	if len(sp.Payments) > 0 && on(NoticeUnusual) {
		findings, err := s.SpendFindings(ctx, workspaceID, from, now.Add(time.Second))
		if err != nil {
			return 0, err
		}
		for _, f := range findings {
			subject := f.Rule + "/" + f.EntryID + "/" + f.AgentID + "/" + f.At.UTC().Format(time.RFC3339)
			if err := add(unusualNotice(f), subject); err != nil {
				return 0, err
			}
		}
	}
	if on(NoticeBudget) {
		alerts, err := s.SpendAlerts(ctx, workspaceID)
		if err != nil {
			return 0, err
		}
		for _, a := range alerts {
			if !a.CreatedAt.Before(from) {
				if err := add(alertNotice(a), a.ID); err != nil {
					return 0, err
				}
			}
		}
	}
	day := local.Format(time.DateOnly)
	morning := local.Hour() >= noticeMorningHour && !set.quiet(local)
	if morning && on(NoticeTip) {
		var written bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM spend_notices WHERE workspace_id = $1 AND kind = 'tip' AND subject LIKE $2)`,
			workspaceID, day+"/%").Scan(&written); err != nil {
			return 0, fmt.Errorf("economy: notice: %w", err)
		}
		if !written {
			if err := loadBudgets(); err != nil {
				return 0, err
			}
		}
		if !written && slices.ContainsFunc(budgets, func(b SpendBudget) bool { return b.Percent >= 50 }) {
			r, err := coach()
			if err != nil {
				return 0, err
			}
			i := 0
			for _, l := range r.Lines {
				if l.Fact.Kind == "budget" {
					if err := add(SpendNotice{Kind: NoticeTip, Title: "Coach", Body: l.Sentence, Data: map[string]any{"fact": l.Fact}},
						day+"/"+strconv.Itoa(i)); err != nil {
						return 0, err
					}
					i++
				}
			}
		}
	}
	if morning && on(NoticeDigest) && local.Weekday() == time.Monday {
		var written bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM spend_notices WHERE workspace_id = $1 AND kind = 'digest' AND subject = $2)`,
			workspaceID, day).Scan(&written); err != nil {
			return 0, fmt.Errorf("economy: notice: %w", err)
		}
		if !written {
			r, err := coach()
			if err != nil {
				return 0, err
			}
			n := SpendNotice{Kind: NoticeDigest, Title: "Your week", Data: map[string]any{"review": r.Review}}
			var said []string
			for _, l := range r.Review {
				said = append(said, l.Sentence)
			}
			if n.Body = strings.Join(said, " "); n.Body == "" {
				n.Status = "dropped" // nothing spent last week: nothing to say, and nothing to ask again until next Monday
			}
			if err := add(n, day); err != nil {
				return 0, err
			}
		}
	}

	sent, err := s.deliverSpendNotices(ctx, workspaceID, set, local, now)
	if err != nil {
		return sent, err
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO spend_notice_settings (workspace_id, seen_until) VALUES ($1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET seen_until = EXCLUDED.seen_until`, workspaceID, now); err != nil {
		return sent, fmt.Errorf("economy: notice settings: %w", err)
	}
	return sent, nil
}

// deliverSpendNotices sends workspaceID's waiting notices that may go at now, oldest first, and answers how many.
func (s *DualTokenStore) deliverSpendNotices(ctx context.Context, workspaceID string, set SpendNoticeSettings, local, now time.Time) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, kind, title, body, urgent, created_at FROM spend_notices
		WHERE workspace_id = $1 AND status = 'waiting' ORDER BY created_at, id`, workspaceID)
	if err != nil {
		return 0, fmt.Errorf("economy: notices: %w", err)
	}
	waiting, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (SpendNotice, error) {
		var n SpendNotice
		err := row.Scan(&n.ID, &n.Kind, &n.Title, &n.Body, &n.Urgent, &n.CreatedAt)
		return n, err
	})
	if err != nil {
		return 0, fmt.Errorf("economy: notices: %w", err)
	}
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())
	var tipSent bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM spend_notices WHERE workspace_id = $1 AND kind = 'tip' AND status = 'sent'
		AND sent_at >= $2)`, workspaceID, midnight).Scan(&tipSent); err != nil {
		return 0, fmt.Errorf("economy: notices: %w", err)
	}
	sent := 0
	for _, n := range waiting {
		status, held := "waiting", ""
		switch {
		case slices.Contains(set.Off, n.Kind), n.Kind == NoticeTip && n.CreatedAt.Before(midnight):
			status = "dropped"
		case !n.Urgent && set.quiet(local):
			held = NoticeHeldQuiet
		case n.Kind == NoticeTip && tipSent:
			held = NoticeHeldTip
		default:
			s.pushSpendNotice(ctx, workspaceID, n)
			status, tipSent, sent = "sent", tipSent || n.Kind == NoticeTip, sent+1
		}
		if _, err := s.pool.Exec(ctx, `UPDATE spend_notices SET status = $2, held = $3, sent_at = CASE WHEN $2 = 'sent' THEN $4::timestamptz END
			WHERE id = $1`, n.ID, status, held, now); err != nil {
			return sent, fmt.Errorf("economy: notices: %w", err)
		}
	}
	return sent, nil
}

// pushSpendNotice sends n to the owner's phones and browsers.
func (s *DualTokenStore) pushSpendNotice(ctx context.Context, workspaceID string, n SpendNotice) {
	s.pushDevices(ctx, workspaceID, nativepush.Message{Title: n.Title, Body: n.Body, Data: map[string]string{
		"type": "spend_notice", "kind": n.Kind, "notice_id": n.ID, "workspace_id": workspaceID}})
	payload, _ := json.Marshal(map[string]any{"type": "spend_notice", "kind": n.Kind, "notice_id": n.ID, "workspace_id": workspaceID,
		"title": n.Title, "body": n.Body})
	s.pushWeb(ctx, workspaceID, payload)
}

// covers reports whether p counts against b, as SpendBudgets counts it.
func (b SpendBudget) covers(p SpendPayment) bool {
	return p.Currency == b.Currency && (b.AgentID == BudgetAllWallets || p.AgentID == b.AgentID) && (b.Category == "" || p.Category == b.Category)
}

// paymentNotice is p's notice: what was paid where, its category, and what was left of its budget just after it —
// the budget on its category if there is one, else the overall one. newer are the payments made since p.
func paymentNotice(p SpendPayment, newer []SpendPayment, budgets []SpendBudget) SpendNotice {
	who, merchant := "You", strings.TrimSpace(p.Merchant)
	if p.AgentID != "" {
		who = "An agent"
	}
	if merchant == "" {
		merchant = "a payee"
	}
	category := categoryWords(p.Category)
	n := SpendNotice{Kind: NoticePayment, Title: fmt.Sprintf("%s paid %s at %s", who, FormatMinor(p.AmountMinor, p.Currency), merchant),
		Body: capitalised(category), Data: map[string]any{"entry_id": p.EntryID, "agent_id": p.AgentID,
			"amount_minor": p.AmountMinor, "currency": p.Currency, "category": p.Category}}
	var b *SpendBudget
	for i := range budgets {
		if budgets[i].covers(p) && !p.CreatedAt.Before(budgets[i].PeriodStart) && (b == nil || b.Category == "" && budgets[i].Category != "") {
			b = &budgets[i]
		}
	}
	if b == nil {
		return n
	}
	left := b.LeftMinor
	for _, q := range newer {
		if b.covers(q) {
			left += q.AmountMinor
		}
	}
	what := "budget"
	if b.Category != "" {
		what = category + " budget"
	}
	if left >= 0 {
		n.Body += fmt.Sprintf(" · %s left of this %s's %s", FormatMinor(left, b.Currency), b.Period, what)
	} else {
		n.Body += fmt.Sprintf(" · %s over this %s's %s", FormatMinor(-left, b.Currency), b.Period, what)
	}
	n.Data["budget_id"], n.Data["budget_left_minor"] = b.ID, left
	return n
}

// unusualNotice is a B53.5 finding's notice, in the Coach's template words: urgent, it may be a payment the owner did
// not make. The template, not the model, so it goes at once.
func unusualNotice(f SpendFinding) SpendNotice {
	return SpendNotice{Kind: NoticeUnusual, Title: "Unusual payment", Body: coachTemplate(findingFact(f)), Urgent: true,
		Data: map[string]any{"rule": f.Rule, "entry_id": f.EntryID, "agent_id": f.AgentID}}
}

// alertNotice is a B53.4 alert's notice.
func alertNotice(a SpendAlert) SpendNotice {
	what := "Your spending"
	if a.Category != "" {
		what = capitalised(categoryWords(a.Category))
	}
	title, body := "Budget", fmt.Sprintf("%s is at %d%% of this %s's budget", what, a.Percent, a.Period)
	switch a.Kind {
	case SpendAlertCard:
		title, body = "Card", fmt.Sprintf("A card has used %d%% of its cap", a.Percent)
	case SpendAlertAgentLimit:
		title, body = "Agent limit", fmt.Sprintf("An agent has used %d%% of its %s limit", a.Percent, a.Period)
	}
	return SpendNotice{Kind: NoticeBudget, Title: title,
		Body: body + ": " + briefingAmount(a.SpentMinor, a.Currency) + " of " + briefingAmount(a.LimitMinor, a.Currency) + ".",
		Data: map[string]any{"alert_id": a.ID, "kind": a.Kind, "subject": a.Subject, "percent": a.Percent}}
}

func capitalised(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
