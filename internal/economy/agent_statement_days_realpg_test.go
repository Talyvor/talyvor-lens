package economy

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// B57.13: a statement read by day makes one model's requests on one day one line, its fee the line under it, and pages
// through every line once, newest first, with the same totals on every page.
func TestAgentStatementByDay_OneLinePerModelAndDay(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := screenedStore(pool)
	const ws = "ws-b5713"
	agent, err := s.CreateAgent(ctx, ws, "Research", "owner")
	if err != nil {
		t.Fatal(err)
	}
	acct := agentAccount(agent.ID)
	// n entries of kind at start, start+1s, …: amount from the agent (negative) or to it, each with its other leg.
	post := func(start time.Time, n int, kind, ref, model string, feeBPS *int64, amount int64, other string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `WITH e AS (SELECT gen_random_uuid() AS id, g FROM generate_series(1, $1::int) g)
			INSERT INTO agent_postings (entry_id, workspace_id, account, amount_ulxc, kind, ref, model, fee_bps, created_at)
			SELECT e.id, $2, l.account, l.sign * $3::bigint, $4, CASE WHEN $5 = '' THEN '' ELSE $5 || e.g END, NULLIF($6, ''), $7,
			       $8::timestamptz + (e.g - 1) * interval '1 second'
			  FROM e CROSS JOIN (VALUES ($9, 1), ($10, -1)) AS l(account, sign)`,
			n, ws, amount, kind, ref, model, feeBPS, start, acct, other); err != nil {
			t.Fatal(err)
		}
	}
	bps := int64(550)
	// The agent's day is London's: 23:00 UTC on 19 October is the first hour of 20 October there.
	day := time.Date(2026, 10, 19, 23, 0, 0, 0, time.UTC)
	post(day.Add(-time.Hour), 1, "fund", "", "", nil, 10_000_000, "workspace")
	for _, q := range []struct {
		at    time.Time
		n     int
		model string
	}{{day, 312, "claude-sonnet-5"}, {day.Add(2 * time.Hour), 5, "gpt-5"}, {day.Add(25 * time.Hour), 3, "claude-sonnet-5"}} {
		ref := q.model + q.at.Format("-0102-1504-")
		post(q.at, q.n, "hold", ref, q.model, nil, -1_000, "spend")
		post(q.at.Add(500*time.Millisecond), q.n, "settle", ref, q.model, nil, 100, "spend")
		post(q.at.Add(600*time.Millisecond), q.n, LXCTypePlatformFee, ref, q.model, &bps, -5, "spend")
	}
	post(day.Add(3*time.Hour), 1, "card", "auth_1", "", nil, -2_000, "spend")

	from, to := day.Add(-2*time.Hour), day.Add(48*time.Hour)
	st, err := s.AgentPeriodStatement(ctx, ws, agent.ID, from, to)
	if err != nil {
		t.Fatal(err)
	}
	london, _ := time.LoadLocation("Europe/London")
	byDay := st.ByDay(london, "Europe/London")
	if byDay.Group != "day" || byDay.TimeZone != "Europe/London" || !reflect.DeepEqual(byDay.Accounts, st.Accounts) {
		t.Fatalf("grouped statement says group %q, zone %q, accounts %+v; want day, Europe/London and %+v", byDay.Group, byDay.TimeZone, byDay.Accounts, st.Accounts)
	}
	type want struct {
		kind, model, day, label string
		requests                int64
		amount                  int64
	}
	var got []want
	var sum, ungrouped int64
	for _, l := range byDay.Lines {
		got = append(got, want{l.Kind, l.Model, l.Day, l.Label, l.Requests, l.AmountULXC})
		sum += l.AmountULXC
	}
	for _, l := range st.Lines {
		ungrouped += l.AmountULXC
	}
	fee := "Platform fee 5.5%"
	if w := []want{
		{"spend", "claude-sonnet-5", "2026-10-21", "", 3, -3 * 900},
		{LXCTypePlatformFee, "claude-sonnet-5", "2026-10-21", fee, 0, -3 * 5},
		{"card", "", "", "", 0, -2_000},
		{"spend", "gpt-5", "2026-10-20", "", 5, -5 * 900},
		{LXCTypePlatformFee, "gpt-5", "2026-10-20", fee, 0, -5 * 5},
		{"spend", "claude-sonnet-5", "2026-10-20", "", 312, -312 * 900},
		{LXCTypePlatformFee, "claude-sonnet-5", "2026-10-20", fee, 0, -312 * 5},
		{"fund", "", "", "", 0, 10_000_000},
	}; !reflect.DeepEqual(got, w) {
		t.Fatalf("lines by day =\n%+v\nwant\n%+v", got, w)
	}
	if sum != ungrouped {
		t.Fatalf("the grouped lines sum to %d; the ungrouped ones to %d", sum, ungrouped)
	}
	if m := byDay.Lines[6].Memo; m != OnThoseRequests {
		t.Errorf("fee line memo = %q; want %q", m, OnThoseRequests)
	}

	// Pages of 3 return every line once, in the same order, with the period's totals on each.
	var paged []StatementLine
	cursor := ""
	for pages := 0; ; pages++ {
		p, err := byDay.Page(3, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(p.Accounts, st.Accounts) || len(p.Lines) > 3 || pages > 5 {
			t.Fatalf("page %d: accounts %+v, %d lines", pages, p.Accounts, len(p.Lines))
		}
		paged = append(paged, p.Lines...)
		if cursor = p.NextCursor; cursor == "" {
			break
		}
	}
	if !reflect.DeepEqual(paged, byDay.Lines) {
		t.Fatalf("paged lines =\n%+v\nwant\n%+v", paged, byDay.Lines)
	}
	// Ungrouped, a page is the newest postings first.
	p, err := st.Page(2, "")
	if err != nil || len(p.Lines) != 2 || p.Lines[0] != st.Lines[len(st.Lines)-1] || p.Lines[1] != st.Lines[len(st.Lines)-2] {
		t.Fatalf("first ungrouped page = %+v (%v); want the two newest postings", p.Lines, err)
	}
	if _, err := st.Page(2, "not a cursor"); err == nil {
		t.Fatal("a cursor that is not one was read")
	}
}
