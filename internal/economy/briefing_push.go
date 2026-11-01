package economy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// briefing_push.go — B41.205: THE DAILY BRIEFING AS A MORNING WEB PUSH.
//
// Once a UTC day, at the first minute tick at or after the push hour (LENS_BRIEFING_PUSH_HOUR), every workspace with a
// web push subscription is sent its briefing's Line. The day is claimed in workspace_briefing_pushes before the push
// goes out, so a workspace gets at most one a day however many Lens processes tick.

// RunBriefingPushes pushes the morning briefing to each workspace not yet sent today's, when now is at or past hour
// (UTC); hour < 0 is off, as is a server without a VAPID key. It returns how many workspaces were sent one.
func (s *DualTokenStore) RunBriefingPushes(ctx context.Context, now time.Time, hour int) (int, error) {
	now = now.UTC()
	pusher := s.approvalPusher
	if pusher == nil || hour < 0 || now.Hour() < hour {
		return 0, nil
	}
	day := now.Format("2006-01-02")
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT s.workspace_id FROM workspace_push_subscriptions s
		WHERE NOT EXISTS (SELECT 1 FROM workspace_briefing_pushes p WHERE p.workspace_id = s.workspace_id AND p.day = $1)`, day)
	if err != nil {
		return 0, fmt.Errorf("economy: briefing push: %w", err)
	}
	var workspaces []string
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			rows.Close()
			return 0, err
		}
		workspaces = append(workspaces, ws)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	sent := 0
	for _, ws := range workspaces {
		// Read before claiming, so a briefing that cannot be read is tried again on the next tick.
		b, err := s.DailyBriefing(ctx, ws, now)
		if err != nil {
			slog.Warn("agents: briefing push", slog.String("workspace_id", ws), slog.String("err", err.Error()))
			continue
		}
		tag, err := s.pool.Exec(ctx, `INSERT INTO workspace_briefing_pushes (workspace_id, day) VALUES ($1, $2) ON CONFLICT DO NOTHING`, ws, day)
		if err != nil {
			return sent, fmt.Errorf("economy: briefing push: %w", err)
		}
		if tag.RowsAffected() == 0 {
			continue // another process sent it
		}
		payload, _ := json.Marshal(map[string]any{"type": "daily_briefing", "workspace_id": ws, "line": b.Line, "quiet": b.Quiet})
		s.webPush(ctx, pusher, ws, payload)
		sent++
	}
	return sent, nil
}
