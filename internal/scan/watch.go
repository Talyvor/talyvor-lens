package scan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Scan agents (B52.27): watches that run on their own. A watch belongs to an agent with its own wallet and names the
// series its owner picked — each a dataset in a region — and the owner's idea of unusual: outside the five-year range
// for the same weeks, or at least so many standard deviations from the five-year normal. Each run reads every series'
// latest point; when it is unusual and the point before it was not, the watch sends one alert, a short factual report
// built only from the engine's figures (Models.Assess, so a series whose model failed its backtest never alerts) with
// its sources' attribution, and the agent's wallet pays for it in credits within its limits, exactly once. A paused
// agent, or one its wallet cannot pay for, sends nothing and is charged nothing, and the watch says why. An alert
// states what the data did; it never says what to do about it.

// Rules for what counts as unusual.
const (
	RuleRange = "range" // outside the lowest and highest of the same weeks in the past five years
	RuleZ     = "z"     // at least Z standard deviations from the five-year normal
)

// maxWatchSeries bounds the series one watch reads.
const maxWatchSeries = 50

var (
	ErrBadWatch      = errors.New("scan: this watch cannot be set up")
	ErrWatchNotFound = errors.New("scan: no such watch in this workspace")
)

// Watch is a Scan agent's watch.
type Watch struct {
	ID          string     `json:"id"`
	AgentID     string     `json:"agent_id"`
	Series      []string   `json:"series"`
	Rule        string     `json:"rule"`
	Z           *float64   `json:"z,omitempty"`
	Active      bool       `json:"active"`
	LastRunAt   *time.Time `json:"last_run_at,omitempty"`
	LastRefusal string     `json:"last_refusal,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	Alerts      []Alert    `json:"alerts"`
}

// Alert is one alert a watch sent, with what the agent's wallet paid for it.
type Alert struct {
	ID          int64     `json:"id"`
	Series      string    `json:"series"`
	ObservedAt  time.Time `json:"observed_at"`
	Body        string    `json:"body"`
	Finding     Finding   `json:"finding"`
	Attribution []string  `json:"attribution"`
	ChargedULXC int64     `json:"charged_ulxc"`
	CreatedAt   time.Time `json:"created_at"`
}

// Wallet is what a watch asks of its agent's wallet. *economy.DualTokenStore satisfies it.
type Wallet interface {
	// AgentMayRun refuses, with the reason, an agent that is paused or archived.
	AgentMayRun(ctx context.Context, workspaceID, agentID string) error
	// ChargeAgent charges the agent's wallet ulxc of credits, exactly once per requestID, within its limits.
	ChargeAgent(ctx context.Context, workspaceID, agentID, requestID string, ulxc int64, description string) error
}

// Watcher runs every active watch. Price is what one alert costs the agent, in µLXC (fees.Current().ScanAlertULXC).
type Watcher struct {
	store  *Store
	models *Models
	wallet Wallet
	price  func() int64
}

func NewWatcher(store *Store, wallet Wallet, price func() int64) *Watcher {
	return &Watcher{store: store, models: NewModels(store), wallet: wallet, price: price}
}

// Create sets up a watch for agentID on series, unusual by rule (with z for RuleZ). Every series must have a model.
func (w *Watcher) Create(ctx context.Context, workspaceID, agentID string, series []string, rule string, z *float64) (Watch, error) {
	seen := map[string]bool{}
	var list []string
	for _, s := range series {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		if _, ok := FamilyOf(s); !ok {
			return Watch{}, fmt.Errorf("%w: %q is not a series Scan models", ErrBadWatch, s)
		}
		seen[s] = true
		list = append(list, s)
	}
	switch {
	case len(list) == 0:
		return Watch{}, fmt.Errorf("%w: name at least one series", ErrBadWatch)
	case len(list) > maxWatchSeries:
		return Watch{}, fmt.Errorf("%w: at most %d series", ErrBadWatch, maxWatchSeries)
	case rule == RuleRange && z != nil:
		return Watch{}, fmt.Errorf("%w: the five-year range takes no z", ErrBadWatch)
	case rule == RuleZ && (z == nil || !(*z > 0) || math.IsInf(*z, 0)):
		return Watch{}, fmt.Errorf("%w: z must be a number of standard deviations above 0", ErrBadWatch)
	case rule != RuleRange && rule != RuleZ:
		return Watch{}, fmt.Errorf("%w: rule must be %q or %q", ErrBadWatch, RuleRange, RuleZ)
	}
	wt := Watch{ID: "swt_" + uuid.NewString(), AgentID: agentID, Series: list, Rule: rule, Z: z, Active: true, Alerts: []Alert{}}
	err := w.store.pool.QueryRow(ctx, `INSERT INTO scan_watches (id, workspace_id, agent_id, series, rule, z)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`, wt.ID, workspaceID, agentID, list, rule, z).Scan(&wt.CreatedAt)
	return wt, err
}

// List is the workspace's watches, newest first, each with its last 20 alerts.
func (w *Watcher) List(ctx context.Context, workspaceID string) ([]Watch, error) {
	rows, err := w.store.pool.Query(ctx, `SELECT id, agent_id, series, rule, z, active, last_run_at, last_refusal, created_at
		FROM scan_watches WHERE workspace_id = $1 ORDER BY created_at DESC, id`, workspaceID)
	if err != nil {
		return nil, err
	}
	out := []Watch{}
	for rows.Next() {
		var wt Watch
		if err := rows.Scan(&wt.ID, &wt.AgentID, &wt.Series, &wt.Rule, &wt.Z, &wt.Active, &wt.LastRunAt, &wt.LastRefusal,
			&wt.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, wt)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Alerts, err = w.alerts(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (w *Watcher) alerts(ctx context.Context, watchID string) ([]Alert, error) {
	rows, err := w.store.pool.Query(ctx, `SELECT id, series, observed_at, body, finding, attribution, charged_ulxc, created_at
		FROM scan_watch_alerts WHERE watch_id = $1 ORDER BY created_at DESC, id DESC LIMIT 20`, watchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		var a Alert
		var finding []byte
		if err := rows.Scan(&a.ID, &a.Series, &a.ObservedAt, &a.Body, &finding, &a.Attribution, &a.ChargedULXC, &a.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(finding, &a.Finding); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Stop stops a watch; it sends nothing more.
func (w *Watcher) Stop(ctx context.Context, workspaceID, watchID string) error {
	tag, err := w.store.pool.Exec(ctx, `UPDATE scan_watches SET active = FALSE WHERE id = $1 AND workspace_id = $2`,
		watchID, workspaceID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrWatchNotFound
	}
	return err
}

// Run runs every active watch once and answers how many alerts it sent. One watch failing is logged; the rest run.
func (w *Watcher) Run(ctx context.Context, now time.Time) (sent int, err error) {
	rows, err := w.store.pool.Query(ctx, `SELECT id, workspace_id, agent_id, series, rule, z FROM scan_watches
		WHERE active ORDER BY created_at, id`)
	if err != nil {
		return 0, err
	}
	type due struct {
		id, ws string
		Watch
	}
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.ws, &d.AgentID, &d.Series, &d.Rule, &d.Z); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, d := range list {
		n, refusal, err := w.runWatch(ctx, d.id, d.ws, d.Watch)
		sent += n
		if err != nil {
			if ctx.Err() != nil {
				return sent, ctx.Err()
			}
			slog.Warn("scan: watch run failed", slog.String("watch", d.id), slog.String("err", err.Error()))
			continue
		}
		if _, err := w.store.pool.Exec(ctx, `UPDATE scan_watches SET last_run_at = $2, last_refusal = $3 WHERE id = $1`,
			d.id, now, refusal); err != nil {
			return sent, err
		}
	}
	return sent, nil
}

// runWatch reads each of the watch's series and sends an alert for each that crossed into unusual. refusal is why the
// agent could not run or pay, "" when nothing stopped it.
func (w *Watcher) runWatch(ctx context.Context, id, workspaceID string, wt Watch) (sent int, refusal string, err error) {
	if err := w.wallet.AgentMayRun(ctx, workspaceID, wt.AgentID); err != nil {
		return 0, err.Error(), nil
	}
	for _, series := range wt.Series {
		pts, unit, err := w.models.points(ctx, series)
		if err != nil {
			return sent, "", err
		}
		f, _ := FamilyOf(series)
		if len(pts) < 2 || !unusual(f, pts, len(pts)-1, wt.Rule, wt.Z) || unusual(f, pts, len(pts)-2, wt.Rule, wt.Z) {
			continue
		}
		last := pts[len(pts)-1]
		var done bool
		if err := w.store.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM scan_watch_alerts WHERE watch_id = $1
			AND series = $2 AND observed_at = $3)`, id, series, last.at).Scan(&done); err != nil {
			return sent, "", err
		}
		if done {
			continue // this series already alerted on its latest point; the watch's other series still run
		}
		fd, err := w.models.Assess(ctx, series)
		if errors.Is(err, ErrNoModel) || errors.Is(err, ErrNotTested) || errors.Is(err, ErrModelRefused) || errors.Is(err, ErrNoNormal) {
			continue // no figure from a model that has not passed its backtest reaches an alert
		}
		if err != nil {
			return sent, "", err
		}
		shown, err := w.store.ForDisplay(ctx, fd.Points["outlook_low"])
		if errors.Is(err, ErrNotDisplayable) {
			continue // a source whose licence keeps it internal never reaches an alert
		}
		if err != nil {
			return sent, "", err
		}
		lo, hi, _ := rangeAt(pts, last.at, f.Window)
		body := alertBody(series, unit, f, wt.Rule, fd, lo, hi, shown.Attribution)
		price := w.price()
		requestID := fmt.Sprintf("scan-watch:%s:%s:%d", id, series, last.at.Unix())
		if price > 0 {
			if err := w.wallet.ChargeAgent(ctx, workspaceID, wt.AgentID, requestID, price,
				"Scan alert: "+series+" on "+last.at.Format(time.DateOnly)); err != nil {
				return sent, err.Error(), nil // unpaid, so unsent; the charge is retried on the next run, once
			}
		}
		b, _ := json.Marshal(fd)
		tag, err := w.store.pool.Exec(ctx, `INSERT INTO scan_watch_alerts (watch_id, workspace_id, agent_id, series,
			observed_at, point_id, body, finding, attribution, charged_ulxc, request_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) ON CONFLICT (watch_id, series, observed_at) DO NOTHING`,
			id, workspaceID, wt.AgentID, series, last.at, last.id, body, string(b), shown.Attribution, price, requestID)
		if err != nil {
			return sent, "", err
		}
		sent += int(tag.RowsAffected())
	}
	return sent, "", nil
}

// unusual says whether pts[i] is unusual by rule against the five-year normal or range at its date.
func unusual(f Family, pts []storedPoint, i int, rule string, z *float64) bool {
	p := pts[i]
	if rule == RuleRange {
		lo, hi, ok := rangeAt(pts, p.at, f.Window)
		return ok && (p.value < lo || p.value > hi)
	}
	mean, sd, _, ok := normalAt(pts, p.at, f.Window)
	return ok && z != nil && math.Abs(p.value-mean)/sd >= *z
}

// rangeAt is the lowest and highest of pts within window of the same date in each of the past normalYears years, ok
// as normalAt's: at least normalMinYears years.
func rangeAt(pts []storedPoint, at time.Time, window time.Duration) (lo, hi float64, ok bool) {
	years := 0
	lo, hi = math.Inf(1), math.Inf(-1)
	for back := 1; back <= normalYears; back++ {
		same := at.AddDate(-back, 0, 0)
		i := sort.Search(len(pts), func(i int) bool { return !pts[i].at.Before(same.Add(-window)) })
		found := false
		for ; i < len(pts) && !pts[i].at.After(same.Add(window)); i++ {
			lo, hi, found = math.Min(lo, pts[i].value), math.Max(hi, pts[i].value), true
		}
		if found {
			years++
		}
	}
	return lo, hi, years >= normalMinYears
}

// alertBody is the alert's report: what the series did, against what, how sure the model is, and the sources. Every
// figure is the engine's.
func alertBody(series, unit string, f Family, rule string, fd Finding, lo, hi float64, attribution []string) string {
	side := "above"
	if fd.Z < 0 {
		side = "below"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s was %s %s on %s", series, figure(fd.Value), unit, fd.At.Format(time.DateOnly))
	if rule == RuleRange {
		fmt.Fprintf(&b, ", outside its range for the same weeks of the past %d years (%s–%s)", normalYears, figure(lo), figure(hi))
	}
	fmt.Fprintf(&b, ", %.1f standard deviations %s its normal for the time of year (%s–%s). ", math.Abs(fd.Z), side,
		figure(fd.NormalLow), figure(fd.NormalHigh))
	fmt.Fprintf(&b, "The model's 80%% range for %s, %s ahead, is %s–%s %s; it passed its backtest on this series' own history. ",
		fd.OutlookAt.Format(time.DateOnly), ahead(f), figure(fd.OutlookLow), figure(fd.OutlookHigh), unit)
	fmt.Fprintf(&b, "Sources: %s.", strings.Join(attribution, "; "))
	return b.String()
}

// figure writes a number with as many decimals as its size needs.
func figure(v float64) string {
	switch a := math.Abs(v); {
	case a >= 100:
		return fmt.Sprintf("%.0f", v)
	case a >= 1:
		return fmt.Sprintf("%.2f", v)
	default:
		return fmt.Sprintf("%.3g", v)
	}
}

// Every runs the watches once every interval until ctx ends.
func (w *Watcher) Every(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if n, err := w.Run(ctx, time.Now()); err != nil && ctx.Err() == nil {
			slog.Warn("scan: watches failed", slog.String("err", err.Error()))
		} else if n > 0 {
			slog.Info("scan: watch alerts sent", slog.Int("sent", n))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
