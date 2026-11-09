package scan

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/economy"
)

// B52.27 — a watch on a recorded series that crosses its threshold fires once and charges its agent's wallet once,
// however often it runs; a paused agent's watch and an agent without the credit send nothing and are charged nothing,
// and the paused one fires once it is resumed.
func TestWatchFiresOnceChargesOnceAndStops(t *testing.T) {
	s := modelStore(t)
	ctx := context.Background()
	run := time.Now().UnixNano()
	series := fmt.Sprintf("eia.stocks.test-watch-%d", run)
	second := series + "-second"
	ws := fmt.Sprintf("ws-scan-watch-%d", run)
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM scan_point_inputs WHERE point_id IN (SELECT id FROM scan_points WHERE starts_with(series, $1))`,
			`DELETE FROM scan_points WHERE starts_with(series, $1)`,
			`DELETE FROM scan_backtests WHERE starts_with(series, $1)`,
		} {
			if _, err := s.pool.Exec(ctx, q, series); err != nil {
				t.Error(err)
			}
		}
	})

	// Eight years of a persistent weekly series; the week before the last sits on its normal, the last far above it.
	pts := weekly(8, 0.9, 1)
	f, _ := FamilyOf(series)
	n := len(pts)
	mean, _, _, _ := normalAt(pts, pts[n-2].at, f.Window)
	pts[n-2].value = mean
	mean, sd, _, _ := normalAt(pts, pts[n-1].at, f.Window)
	pts[n-1].value = mean + 8*sd
	for _, sr := range []string{series, second} {
		recordSeries(t, s, sr, pts)
		if r, err := NewModels(s).RunSeries(ctx, sr); err != nil || !r.Passed {
			t.Fatalf("the series' model: %+v, %v", r, err)
		}
	}

	bank := economy.NewDualTokenStore(nil, s.pool.(*pgxpool.Pool), nil)
	if _, err := s.pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := bank.CreditLXC(ctx, ws, 10_000_000, "top-up", nil); err != nil {
		t.Fatal(err)
	}
	agent := func(name string, fund int64) string {
		a, err := bank.CreateAgent(ctx, ws, name, "user-scan")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bank.FundAgent(ctx, ws, a.ID, fund); err != nil {
			t.Fatal(err)
		}
		var key string
		if err := s.pool.QueryRow(ctx, `INSERT INTO workspace_api_keys (workspace_id, key_hash, key_prefix, scopes)
			VALUES ($1, 'h', 'p', '{proxy}') RETURNING id::text`, ws).Scan(&key); err != nil {
			t.Fatal(err)
		}
		if err := bank.AttachAgentKey(ctx, ws, a.ID, key); err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	const price = 5_000
	paid, paused, poor := agent("scan-paid", 1_000_000), agent("scan-paused", 1_000_000), agent("scan-poor", price-1)
	if err := bank.PauseAgent(ctx, ws, paused, ""); err != nil {
		t.Fatal(err)
	}

	w := NewWatcher(s, bank, func() int64 { return price })
	z := 3.0
	watches := map[string]Watch{}
	for _, c := range []struct {
		agent, rule string
		z           *float64
		series      []string
	}{{paid, RuleRange, nil, []string{series, second}}, {paused, RuleZ, &z, []string{series}}, {poor, RuleZ, &z, []string{series}}} {
		wt, err := w.Create(ctx, ws, c.agent, c.series, c.rule, c.z)
		if err != nil {
			t.Fatal(err)
		}
		watches[c.agent] = wt
	}
	if _, err := w.Create(ctx, ws, paid, []string{"ecmwf.t2m.london"}, RuleRange, nil); err == nil {
		t.Fatal("a watch on a series with no model was set up")
	}

	for i := 0; i < 3; i++ {
		if _, err := w.Run(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	charged := func(agentID string) (rows int, total int64) {
		t.Helper()
		if err := s.pool.QueryRow(ctx, `SELECT count(*), COALESCE(-sum(amount_ulxc), 0)::bigint FROM agent_postings
			WHERE workspace_id = $1 AND account = 'agent:' || $2 AND kind = 'spend'`, ws, agentID).Scan(&rows, &total); err != nil {
			t.Fatal(err)
		}
		return rows, total
	}
	list, err := w.List(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Watch{}
	for _, wt := range list {
		got[wt.AgentID] = wt
	}

	a := got[paid]
	if len(a.Alerts) != 2 || a.Alerts[1].ChargedULXC != price || a.LastRefusal != "" {
		t.Fatalf("the paid agent's watch after three runs: %+v", a)
	}
	if rows, total := charged(paid); rows != 2 || total != 2*price {
		t.Fatalf("the paid agent was charged %d µLXC in %d ledger rows, want %d in each of two", total, rows, price)
	}
	var ledger int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1 AND type = 'spend'
		AND amount = $2 AND metadata->>'request_id' LIKE 'scan-watch:' || $3 || ':%'`, ws, -price, watches[paid].ID).Scan(&ledger); err != nil || ledger != 2 {
		t.Fatalf("%d workspace ledger rows for the alerts (%v), want 2", ledger, err)
	}
	body := a.Alerts[1].Body
	for _, want := range []string{series, "outside its range for the same weeks of the past 5 years", "standard deviations above",
		"80% range", "Sources: Source: U.S. Energy Information Administration."} {
		if !strings.Contains(body, want) {
			t.Fatalf("the alert %q does not say %q", body, want)
		}
	}
	for _, advice := range []string{"buy", "sell", "hold", "long", "short", "target"} {
		if strings.Contains(strings.ToLower(body), advice) {
			t.Fatalf("the alert %q gives advice (%q)", body, advice)
		}
	}

	for _, id := range []string{paused, poor} {
		wt := got[id]
		if len(wt.Alerts) != 0 || wt.LastRefusal == "" || wt.LastRunAt == nil {
			t.Fatalf("watch of %s: %+v", id, wt)
		}
		if rows, _ := charged(id); rows != 0 {
			t.Fatalf("agent %s was charged %d times without an alert", id, rows)
		}
	}
	if !strings.Contains(got[paused].LastRefusal, "paused") {
		t.Fatalf("the paused agent's watch says %q", got[paused].LastRefusal)
	}

	// Resumed, the paused agent's watch fires once on the crossing it missed, and is charged once.
	if err := bank.ResumeAgent(ctx, ws, paused); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := w.Run(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if rows, total := charged(paused); rows != 1 || total != price {
		t.Fatalf("the resumed agent was charged %d µLXC in %d rows", total, rows)
	}
	if err := w.Stop(ctx, ws, watches[poor].ID); err != nil {
		t.Fatal(err)
	}
	if err := w.Stop(ctx, "ws-other", watches[paid].ID); err == nil {
		t.Fatal("another workspace stopped this workspace's watch")
	}

	// A watch of two series whose first has already alerted on its latest point still checks, and alerts on, the
	// second when that one crosses.
	if _, err := s.pool.Exec(ctx, `DELETE FROM scan_watch_alerts WHERE watch_id = $1 AND series = $2`,
		watches[paid].ID, second); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Run(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	alerts, err := w.alerts(ctx, watches[paid].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 || alerts[0].Series != second {
		t.Fatalf("the two-series watch's alerts after its second series crossed: %+v", alerts)
	}
	if rows, total := charged(paid); rows != 2 || total != 2*price {
		t.Fatalf("the second series' alert was charged again: %d µLXC in %d rows", total, rows)
	}
}
