package rooms

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// tasks.go — B41.152: THE OPEN TASKS BOARD: TASKS SIZED FROM THE RATE CARD THAT ANY MEMBER MAY CLAIM (migration 0359).
//
// The room's owner or an editor posts a task at a size on the room's rate card; the size's points are copied onto it
// then, so a later rate card does not change them. Any member that accepted the room's current terms claims an open
// task for itself, and only for itself, and lets it go again with nothing lost. A task is never assigned to anyone and
// carries no deadline or penalty: TaskDraft has no field for either, and the routes refuse a body that sends one, so
// contributors stay volunteers, not workers (ERA 1996 s230(3)(b)). Gated on room_equity like the rate card.

const (
	maxTaskTitle       = 200
	maxTaskDescription = 4000
)

// Task is one task on a room's board, as its members see it.
type Task struct {
	ID                    string     `json:"id"`
	RoomID                string     `json:"room_id"`
	PosterWorkspaceID     string     `json:"poster_workspace_id"`
	Title                 string     `json:"title"`
	Description           string     `json:"description"`
	Size                  string     `json:"size"`
	Points                int64      `json:"points"`
	ClaimantWorkspaceID   string     `json:"claimant_workspace_id,omitempty"` // empty: open
	ClaimantWorkspaceName string     `json:"claimant_workspace_name,omitempty"`
	ClaimedAt             *time.Time `json:"claimed_at,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
}

// TaskDraft is POST /v1/rooms/{id}/tasks. It names no person and no date, on purpose.
type TaskDraft struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Size        string `json:"size"`
}

// A claim counts only while its claimant is a live member of the room: one that left or was removed holds nothing.
const taskCols = `t.id, t.room_id, t.poster_workspace_id, t.title, t.description, t.size, t.points,
	COALESCE(m.workspace_id, ''), COALESCE(w.name, ''), CASE WHEN m.workspace_id IS NULL THEN NULL ELSE t.claimed_at END, t.created_at`

const taskFrom = ` FROM room_tasks t
	LEFT JOIN room_members m ON m.room_id = t.room_id AND m.workspace_id = t.claimant_workspace_id AND m.removed_at IS NULL
	LEFT JOIN workspaces w ON w.id = m.workspace_id`

func scanTask(row pgx.Row) (t Task, err error) {
	err = row.Scan(&t.ID, &t.RoomID, &t.PosterWorkspaceID, &t.Title, &t.Description, &t.Size, &t.Points,
		&t.ClaimantWorkspaceID, &t.ClaimantWorkspaceName, &t.ClaimedAt, &t.CreatedAt)
	return t, err
}

func readTask(ctx context.Context, tx pgx.Tx, roomID, taskID string) (Task, error) {
	t, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskCols+taskFrom+` WHERE t.room_id = $1 AND t.id = $2`, roomID, taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return t, fmt.Errorf("%w: no such task in this room", ErrNotFound)
	}
	return t, err
}

// Tasks is roomID's board, open tasks first, oldest first, for ws, a member that accepted the room's current terms.
func (s *Store) Tasks(ctx context.Context, ws, roomID string) (out []Task, err error) {
	out = []Task{}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := ownershipMember(ctx, tx, roomID, ws); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+taskCols+taskFrom+` WHERE t.room_id = $1
			ORDER BY m.workspace_id IS NOT NULL, t.created_at, t.id`, roomID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTask(rows)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// PostTask puts a task on roomID's board at a size on its rate card; the room's owner or an editor does.
func (s *Store) PostTask(ctx context.Context, ws, roomID string, d TaskDraft) (out Task, err error) {
	d.Title, d.Description, d.Size = strings.TrimSpace(d.Title), strings.TrimSpace(d.Description), strings.TrimSpace(d.Size)
	switch {
	case d.Title == "" || len(d.Title) > maxTaskTitle:
		return out, invalid("a task has a title of 1 to %d characters", maxTaskTitle)
	case len(d.Description) > maxTaskDescription:
		return out, invalid("a task's description is at most %d characters", maxTaskDescription)
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		m, err := ownershipMember(ctx, tx, roomID, ws)
		if err != nil {
			return err
		}
		switch {
		case m.Role != RoleOwner && m.Role != RoleEditor:
			return forbidden("only the room's owner or an editor posts a task")
		case m.MutedAt != nil:
			return forbidden("the room's owner has muted you: you read the room and post nothing in it")
		}
		if r, err := lockRoomAny(ctx, tx, roomID); err != nil {
			return err
		} else if err := shut(r); err != nil {
			return err
		}
		rc, err := readRateCard(ctx, tx, roomID)
		if err != nil {
			return err
		}
		var points int64
		for _, z := range rc.Sizes {
			if z.Size == d.Size {
				points = z.Points
			}
		}
		if points == 0 {
			return invalid("size %q is not on the room's rate card", d.Size)
		}
		id := "rtsk_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := tx.Exec(ctx, `INSERT INTO room_tasks (id, room_id, poster_workspace_id, title, description, size, points)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, roomID, ws, d.Title, d.Description, d.Size, points); err != nil {
			return err
		}
		out, err = readTask(ctx, tx, roomID, id)
		return err
	})
	return out, err
}

// ClaimTask claims an open task for ws itself, or one whose claimant has left the room; claiming one ws already holds is a no-op. Nobody claims for another.
func (s *Store) ClaimTask(ctx context.Context, ws, roomID, taskID string) (out Task, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		m, err := ownershipMember(ctx, tx, roomID, ws)
		if err != nil {
			return err
		}
		switch {
		case m.Role == RoleViewer:
			return forbidden("a viewer reads the room and does not claim its tasks")
		case m.MutedAt != nil:
			return forbidden("the room's owner or an editor has muted you: you read the room and claim nothing in it")
		}
		if r, err := lockRoomAny(ctx, tx, roomID); err != nil {
			return err
		} else if err := shut(r); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE room_tasks SET claimant_workspace_id = $3, claimed_at = now()
			WHERE room_id = $1 AND id = $2 AND (claimant_workspace_id IS NULL OR NOT EXISTS (SELECT 1 FROM room_members m
				WHERE m.room_id = $1 AND m.workspace_id = room_tasks.claimant_workspace_id AND m.removed_at IS NULL))`, roomID, taskID, ws)
		if err != nil {
			return err
		}
		if out, err = readTask(ctx, tx, roomID, taskID); err == nil && tag.RowsAffected() == 0 && out.ClaimantWorkspaceID != ws {
			return fmt.Errorf("%w: %s has claimed the task", ErrConflict, out.ClaimantWorkspaceName)
		}
		return err
	})
	return out, err
}

// ReleaseTask lets go of a task ws claimed, so it is open again; only its claimant does, and it costs nothing.
func (s *Store) ReleaseTask(ctx context.Context, ws, roomID, taskID string) (out Task, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := ownershipMember(ctx, tx, roomID, ws); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE room_tasks SET claimant_workspace_id = NULL, claimed_at = NULL
			WHERE room_id = $1 AND id = $2 AND claimant_workspace_id = $3`, roomID, taskID, ws)
		if err != nil {
			return err
		}
		if out, err = readTask(ctx, tx, roomID, taskID); err == nil && tag.RowsAffected() == 0 {
			return forbidden("only the member that claimed a task lets it go")
		}
		return err
	})
	return out, err
}
