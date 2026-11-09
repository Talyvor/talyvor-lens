package economy

import (
	"context"
	"fmt"
	"time"
)

// agent_analytics.go — B28.326: ONE AGENT'S SPEND BY DAY, MODEL AND DESTINATION.
//
// Read from agent_postings alone, the same rows as the agent's statement for [from, to): every posting that
// took money out of the agent's account goes into exactly one bucket, so the buckets' OutULXC sum to the
// statement's out_ulxc. A hold's unused part comes back on a settle or a release; that is ReturnedULXC, in the
// bucket of the day it came back, and SpentULXC is what stayed spent. Destination is the account the money went
// to, as the statement's counterparty names it: spend (a question or a card), held, workspace, agent:<id>,
// escrow:<id>, pot:<id>.

// SpendBucket is what one agent sent to one destination for one model on one day.
type SpendBucket struct {
	Day          string `json:"day"`   // YYYY-MM-DD in the analytics' time zone
	Model        string `json:"model"` // empty for a movement that was not a question
	Destination  string `json:"destination"`
	OutULXC      int64  `json:"out_ulxc"`
	ReturnedULXC int64  `json:"returned_ulxc"`
	SpentULXC    int64  `json:"spent_ulxc"` // OutULXC − ReturnedULXC
	Postings     int64  `json:"postings"`
}

// SpendAnalytics is one agent's spend over [From, To), in buckets ordered by day, model and destination.
type SpendAnalytics struct {
	WorkspaceID  string        `json:"workspace_id"`
	AgentID      string        `json:"agent_id"`
	From         time.Time     `json:"from"`
	To           time.Time     `json:"to"`
	TimeZone     string        `json:"time_zone"`
	OutULXC      int64         `json:"out_ulxc"` // the statement's out_ulxc for the same period
	ReturnedULXC int64         `json:"returned_ulxc"`
	SpentULXC    int64         `json:"spent_ulxc"`
	Buckets      []SpendBucket `json:"buckets"`
}

// AgentSpendAnalytics buckets agentID's postings out in [from, to), and the settles and releases that returned
// part of them, by day in zone, model and destination.
func (s *DualTokenStore) AgentSpendAnalytics(ctx context.Context, workspaceID, agentID string, from, to time.Time, zone *time.Location) (SpendAnalytics, error) {
	a := SpendAnalytics{WorkspaceID: workspaceID, AgentID: agentID, From: from, To: to, TimeZone: zone.String(), Buckets: []SpendBucket{}}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`,
		agentID, workspaceID).Scan(&exists); err != nil {
		return a, fmt.Errorf("economy: agent analytics: %w", err)
	}
	if !exists {
		return a, ErrAgentNotFound
	}
	rows, err := s.pool.Query(ctx, `
		WITH moved AS (
			SELECT to_char(p.created_at AT TIME ZONE $5, 'YYYY-MM-DD') AS day, COALESCE(p.model, '') AS model,
			       COALESCE((SELECT o.account FROM agent_postings o WHERE o.entry_id = p.entry_id AND o.id <> p.id ORDER BY o.id LIMIT 1), '') AS destination,
			       p.amount_ulxc
			  FROM agent_postings p
			 WHERE p.workspace_id = $1 AND p.account = $2 AND p.created_at >= $3 AND p.created_at < $4
			   AND (p.amount_ulxc < 0 OR p.kind IN ('settle', 'release')))
		SELECT day, model, destination,
		       COALESCE(-sum(amount_ulxc) FILTER (WHERE amount_ulxc < 0), 0)::bigint,
		       COALESCE(sum(amount_ulxc) FILTER (WHERE amount_ulxc > 0), 0)::bigint,
		       count(*)
		  FROM moved GROUP BY day, model, destination ORDER BY day, model, destination`,
		workspaceID, agentAccount(agentID), from, to, a.TimeZone)
	if err != nil {
		return a, fmt.Errorf("economy: agent analytics: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var b SpendBucket
		if err := rows.Scan(&b.Day, &b.Model, &b.Destination, &b.OutULXC, &b.ReturnedULXC, &b.Postings); err != nil {
			return a, fmt.Errorf("economy: agent analytics: %w", err)
		}
		b.SpentULXC = b.OutULXC - b.ReturnedULXC
		a.OutULXC += b.OutULXC
		a.ReturnedULXC += b.ReturnedULXC
		a.Buckets = append(a.Buckets, b)
	}
	a.SpentULXC = a.OutULXC - a.ReturnedULXC
	return a, rows.Err()
}
