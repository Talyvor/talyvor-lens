package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/market"
)

// messages.go — B32.30: a room's messages (migration 0212).
//
// A member posts text to the room; its author edits or deletes it, and the room's owner or an editor removes any
// message. A deleted message leaves a tombstone: the row stays, its body emptied, with who deleted it. In a public
// room every message, and every edit, is scanned as a listing is published (internal/market's ScanText): a secret or
// personal data refuses it with what was found, and writes nothing. A member posts at most LENS_ROOM_MESSAGES_PER_MINUTE
// messages a minute in one room. Every post, edit and delete appends a room_events row, which members' event streams
// read after their cursor.
//
// Every write takes the room's row lock first, and takes its seq and its event cursor under it: within a room they
// commit in the order they were taken, so a stream that has read cursor N never later finds a smaller one appear.

// Message kinds, as migration 0212 checks them. A member posts text; the others are posted by later items (a
// contribution, a run, a prize) and by Talyvor.
const (
	KindText         = "text"
	KindContribution = "contribution"
	KindRun          = "run"
	KindPrize        = "prize"
	KindSystem       = "system"
)

// Room event kinds.
const (
	EventPosted  = "message.posted"
	EventEdited  = "message.edited"
	EventDeleted = "message.deleted"
)

const (
	// MaxMessage is the longest message body, in characters.
	MaxMessage = 8000
	// defaultMessagesPerMinute is LENS_ROOM_MESSAGES_PER_MINUTE's default.
	defaultMessagesPerMinute = 20
	// MessagesPerMinuteSetting names the setting a refusal names.
	MessagesPerMinuteSetting = "LENS_ROOM_MESSAGES_PER_MINUTE"

	pageDefault = 50
	pageMax     = 200
	eventsMax   = 200
)

// RateError is a member past LENS_ROOM_MESSAGES_PER_MINUTE in a room.
type RateError struct {
	PerMinute  int
	RetryAfter time.Duration
}

func (e *RateError) Error() string {
	return fmt.Sprintf("rooms: a member posts at most %d messages a minute in a room (%s); try again in %ds",
		e.PerMinute, MessagesPerMinuteSetting, int(e.RetryAfter.Seconds()))
}

// ScanRefusal is a public room's message refused by the publish scan, with what it found.
type ScanRefusal struct {
	Reason string
	Scan   market.Scan
}

func (e *ScanRefusal) Error() string { return "rooms: " + e.Reason }

// Message is a room's message as one reader sees it.
type Message struct {
	ID                   string          `json:"id"`
	Cursor               int64           `json:"cursor"` // its place in the room: GET .../messages?before= and ?after= take it
	RoomID               string          `json:"room_id"`
	AuthorWorkspaceID    string          `json:"author_workspace_id"`
	AuthorWorkspaceName  string          `json:"author_workspace_name"`
	AuthorUserID         string          `json:"author_user_id,omitempty"` // shown to the author's own workspace only
	AuthorAgentID        string          `json:"author_agent_id,omitempty"`
	AuthorAgentName      string          `json:"author_agent_name,omitempty"` // an agent's message carries its name (B32.36)
	Kind                 string          `json:"kind"`
	Body                 string          `json:"body"`
	Refs                 json.RawMessage `json:"refs"`
	CreatedAt            time.Time       `json:"created_at"`
	EditedAt             *time.Time      `json:"edited_at,omitempty"`
	DeletedAt            *time.Time      `json:"deleted_at,omitempty"`
	DeletedByWorkspaceID string          `json:"deleted_by_workspace_id,omitempty"`
	Scan                 *market.Scan    `json:"scan,omitempty"` // a public room's message: what the scan found
}

// Page is GET /v1/rooms/{id}/messages: messages oldest first, whether there are more beyond them, and the room's event
// cursor as of the same read, to stream what happens after it.
type Page struct {
	Messages     []Message `json:"messages"`
	More         bool      `json:"more"`
	EventsCursor int64     `json:"events_cursor"`
}

// Event is one room_events row, with the message it is about as it now stands.
type Event struct {
	Cursor  int64     `json:"cursor"`
	Kind    string    `json:"kind"`
	Ref     string    `json:"ref"`
	At      time.Time `json:"at"`
	Message *Message  `json:"message,omitempty"`
}

// SetMessagesPerMinute sets LENS_ROOM_MESSAGES_PER_MINUTE; below 1 it is the default.
func (s *Store) SetMessagesPerMinute(n int) {
	if n < 1 {
		n = defaultMessagesPerMinute
	}
	s.perMinute = n
}

func (s *Store) messagesPerMinute() int {
	if s.perMinute < 1 {
		return defaultMessagesPerMinute
	}
	return s.perMinute
}

func checkBody(body string) error {
	if strings.TrimSpace(body) == "" {
		return invalid("a message needs a body")
	}
	if n := utf8.RuneCountInString(body); n > MaxMessage {
		return invalid("a message is at most %d characters; this one is %d", MaxMessage, n)
	}
	return nil
}

// scanPublic scans a public room's message: a secret or personal data refuses it. What else the scan finds — a
// prompt injection — is kept on the row and refuses nothing: a room is a conversation, not a listing others run.
func scanPublic(body string) ([]byte, error) {
	sc := market.ScanText("room_message", []string{body})
	switch {
	case len(sc.Secrets) > 0:
		return nil, &ScanRefusal{Scan: sc, Reason: "this room is public and the message contains a secret (" +
			strings.Join(sc.Secrets, ", ") + ") — remove it and send it again"}
	case len(sc.PersonalData) > 0:
		return nil, &ScanRefusal{Scan: sc, Reason: "this room is public and the message contains personal data (" +
			strings.Join(sc.PersonalData, ", ") + ") — remove it and send it again"}
	}
	sc.Refused, sc.Held = "", ""
	return json.Marshal(sc)
}

const messageCols = `m.id, m.seq, m.room_id, m.author_workspace_id, m.author_user_id, m.author_agent_id,
	COALESCE((SELECT a.name FROM agent_accounts a WHERE a.id = m.author_agent_id), ''), m.kind, m.body, m.refs,
	m.created_at, m.edited_at, m.deleted_at, m.deleted_by_workspace_id, m.scan, ` + workspaceNameOf + `m.author_workspace_id` + workspaceNameEnd

// scanMessage reads a messageCols row as viewer sees it, after the columns lead is scanned into.
func scanMessage(row pgx.Row, viewer string, lead ...any) (Message, error) {
	var m Message
	var refs, sc []byte
	if err := row.Scan(append(lead, &m.ID, &m.Cursor, &m.RoomID, &m.AuthorWorkspaceID, &m.AuthorUserID, &m.AuthorAgentID, &m.AuthorAgentName, &m.Kind,
		&m.Body, &refs, &m.CreatedAt, &m.EditedAt, &m.DeletedAt, &m.DeletedByWorkspaceID, &sc, &m.AuthorWorkspaceName)...); err != nil {
		return m, err
	}
	m.Refs = json.RawMessage(refs)
	if len(sc) > 0 {
		m.Scan = &market.Scan{}
		if err := json.Unmarshal(sc, m.Scan); err != nil {
			return m, err
		}
	}
	if m.AuthorWorkspaceID != viewer {
		m.AuthorUserID = ""
	}
	return m, nil
}

func appendEvent(ctx context.Context, tx pgx.Tx, roomID, kind, ref string) error {
	_, err := tx.Exec(ctx, `INSERT INTO room_events (room_id, kind, ref) VALUES ($1, $2, $3)`, roomID, kind, ref)
	return err
}

func messageErr(op string, err error) error {
	var rate *RateError
	var refusal *ScanRefusal
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrForbidden) ||
		errors.Is(err, ErrConflict) || errors.As(err, &rate) || errors.As(err, &refusal) {
		return err
	}
	return fmt.Errorf("rooms: %s: %w", op, err)
}

// Post writes ws's text message to the room, by user: ws must be a live member that is neither a viewer nor muted, the
// room open (a locked room is read-only), the message within LENS_ROOM_MESSAGES_PER_MINUTE and, in a public room, passed by the scan.
func (s *Store) Post(ctx context.Context, ws, user, roomID, body string) (Message, error) {
	if err := checkBody(body); err != nil {
		return Message{}, err
	}
	var out Message
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockRoom(ctx, tx, ws, roomID)
		if err != nil {
			return err
		}
		me, ok, err := member(ctx, tx, roomID, ws)
		if err != nil {
			return err
		}
		if !ok {
			if b, err := banned(ctx, tx, roomID, ws); err != nil {
				return err
			} else if b {
				return errBanned
			}
			return forbidden("join the room to post in it")
		}
		if me.Role == RoleViewer {
			return forbidden("a viewer reads the room and does not post in it")
		}
		if me.MutedAt != nil {
			return forbidden("the room's owner or an editor has muted you: you read the room and post nothing in it")
		}
		if err := shut(r); err != nil {
			return err
		}
		// Counted under the room's lock, so two posts cannot both take the minute's last message.
		var recent int
		var oldest *time.Time
		if err := tx.QueryRow(ctx, `SELECT count(*), min(created_at) FROM room_messages
			WHERE author_workspace_id = $1 AND room_id = $2 AND kind = 'text' AND created_at > now() - interval '1 minute'`,
			ws, roomID).Scan(&recent, &oldest); err != nil {
			return err
		}
		if limit := s.messagesPerMinute(); recent >= limit {
			wait := time.Second
			if oldest != nil {
				wait = max(time.Until(oldest.Add(time.Minute)).Round(time.Second), time.Second)
			}
			return &RateError{PerMinute: limit, RetryAfter: wait}
		}
		var scan []byte
		if r.Visibility == Public {
			if scan, err = scanPublic(body); err != nil {
				return err
			}
		}
		id := "rmsg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if out, err = scanMessage(tx.QueryRow(ctx, `INSERT INTO room_messages AS m (id, room_id, author_workspace_id, author_user_id, author_agent_id, kind, body, scan)
			VALUES ($1, $2, $3, $4, $5, 'text', $6, $7) RETURNING `+messageCols, id, roomID, ws, user, agentOf(ctx), body, jsonText(scan)), ws); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, roomID, EventPosted, id); err != nil {
			return err
		}
		return touch(ctx, tx, roomID)
	})
	return out, messageErr("post", err)
}

// lockMessage reads the room's message for update, with the room locked first and seen as ws sees it.
func lockMessage(ctx context.Context, tx pgx.Tx, ws, roomID, msgID string) (Room, Message, error) {
	r, err := lockRoom(ctx, tx, ws, roomID)
	if err != nil {
		return r, Message{}, err
	}
	m, err := scanMessage(tx.QueryRow(ctx, `SELECT `+messageCols+` FROM room_messages m WHERE m.id = $1 AND m.room_id = $2 FOR UPDATE`,
		msgID, roomID), ws)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, m, fmt.Errorf("%w: no such message in this room", ErrNotFound)
	}
	return r, m, err
}

// Edit replaces the body of ws's own text message, which in a public room is scanned again. A deleted message, or one
// in a closed room, is not edited.
func (s *Store) Edit(ctx context.Context, ws, roomID, msgID, body string) (Message, error) {
	if err := checkBody(body); err != nil {
		return Message{}, err
	}
	var out Message
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, m, err := lockMessage(ctx, tx, ws, roomID, msgID)
		if err != nil {
			return err
		}
		if m.AuthorWorkspaceID != ws {
			return forbidden("only its author edits a message")
		}
		if m.Kind != KindText {
			return forbidden("a %s message is posted by Talyvor and is not edited", m.Kind)
		}
		if m.DeletedAt != nil {
			return fmt.Errorf("%w: the message was deleted", ErrConflict)
		}
		if err := shut(r); err != nil {
			return err
		}
		if me, ok, err := member(ctx, tx, roomID, ws); err != nil {
			return err
		} else if !ok {
			return forbidden("only a member of the room edits its messages")
		} else if me.MutedAt != nil {
			return forbidden("the room's owner or an editor has muted you: you read the room and post nothing in it")
		}
		if m.Body == body {
			out = m
			return nil
		}
		var scan []byte
		if r.Visibility == Public {
			if scan, err = scanPublic(body); err != nil {
				return err
			}
		}
		if out, err = scanMessage(tx.QueryRow(ctx, `UPDATE room_messages AS m SET body = $2, scan = $3, edited_at = now()
			WHERE m.id = $1 RETURNING `+messageCols, msgID, body, jsonText(scan)), ws); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, roomID, EventEdited, msgID); err != nil {
			return err
		}
		return touch(ctx, tx, roomID)
	})
	return out, messageErr("edit", err)
}

// Delete empties a message and stamps it deleted, leaving its tombstone: its author deletes its own, and the room's
// owner or an editor removes any. Deleting a deleted message changes nothing.
func (s *Store) Delete(ctx context.Context, ws, roomID, msgID string) (Message, error) {
	var out Message
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, m, err := lockMessage(ctx, tx, ws, roomID, msgID)
		if err != nil {
			return err
		}
		if m.AuthorWorkspaceID != ws {
			me, ok, err := member(ctx, tx, roomID, ws)
			if err != nil {
				return err
			}
			if !ok || (me.Role != RoleOwner && me.Role != RoleEditor) {
				return forbidden("only its author, or the room's owner or an editor, deletes a message")
			}
		}
		if m.DeletedAt != nil {
			out = m
			return nil
		}
		if out, err = scanMessage(tx.QueryRow(ctx, `UPDATE room_messages AS m SET body = '', deleted_at = now(), deleted_by_workspace_id = $2
			WHERE m.id = $1 RETURNING `+messageCols, msgID, ws), ws); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, roomID, EventDeleted, msgID); err != nil {
			return err
		}
		return touch(ctx, tx, roomID)
	})
	return out, messageErr("delete", err)
}

// readable answers ErrNotFound unless viewer may read the room's messages: everyone reads a public room's, and a
// private room's are read by its live members only.
func readable(ctx context.Context, tx pgx.Tx, viewer, roomID string) error {
	var visibility string
	var isMember bool
	err := tx.QueryRow(ctx, `SELECT r.visibility, EXISTS (SELECT 1 FROM room_members m
		WHERE m.room_id = r.id AND m.workspace_id = $2 AND m.removed_at IS NULL) FROM rooms r WHERE r.id = $1`,
		roomID, viewer).Scan(&visibility, &isMember)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && visibility == Private && !isMember) {
		return fmt.Errorf("%w: no such room", ErrNotFound)
	}
	return err
}

func readTx(ctx context.Context, s *Store, fn func(pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, fn)
}

// Messages is a page of the room's messages as viewer reads them, oldest first: the latest limit of them, those
// before the cursor before, or the first limit after the cursor after. Its events_cursor is read in the same snapshot.
func (s *Store) Messages(ctx context.Context, viewer, roomID string, before, after int64, limit int) (Page, error) {
	if before < 0 || after < 0 {
		return Page{}, invalid("before and after are a message's cursor")
	}
	if before > 0 && after > 0 {
		return Page{}, invalid("page with before or with after, not both")
	}
	if limit <= 0 {
		limit = pageDefault
	}
	limit = min(limit, pageMax)
	p := Page{Messages: []Message{}}
	err := readTx(ctx, s, func(tx pgx.Tx) error {
		if err := readable(ctx, tx, viewer, roomID); err != nil {
			return err
		}
		q := `SELECT ` + messageCols + ` FROM room_messages m WHERE m.room_id = $1 AND ($2 = 0 OR m.seq < $2)
			ORDER BY m.seq DESC LIMIT $3`
		cur := before
		if after > 0 {
			q = `SELECT ` + messageCols + ` FROM room_messages m WHERE m.room_id = $1 AND m.seq > $2 ORDER BY m.seq LIMIT $3`
			cur = after
		}
		rows, err := tx.Query(ctx, q, roomID, cur, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMessage(rows, viewer)
			if err != nil {
				return err
			}
			p.Messages = append(p.Messages, m)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if p.More = len(p.Messages) > limit; p.More {
			p.Messages = p.Messages[:limit]
		}
		if after == 0 {
			for i, j := 0, len(p.Messages)-1; i < j; i, j = i+1, j-1 {
				p.Messages[i], p.Messages[j] = p.Messages[j], p.Messages[i]
			}
		}
		return tx.QueryRow(ctx, `SELECT coalesce(max(cursor), 0) FROM room_events WHERE room_id = $1`, roomID).Scan(&p.EventsCursor)
	})
	return p, messageErr("messages", err)
}

// EventsHead is the room's latest event cursor, for a stream that starts from now.
func (s *Store) EventsHead(ctx context.Context, viewer, roomID string) (int64, error) {
	var head int64
	err := readTx(ctx, s, func(tx pgx.Tx) error {
		if err := readable(ctx, tx, viewer, roomID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT coalesce(max(cursor), 0) FROM room_events WHERE room_id = $1`, roomID).Scan(&head)
	})
	return head, messageErr("events", err)
}

// Events is the room's events after the cursor after, oldest first, each with its message as it now stands — as
// viewer may read them: a private room's member that has been removed reads ErrNotFound from then on.
func (s *Store) Events(ctx context.Context, viewer, roomID string, after int64) ([]Event, error) {
	out := []Event{}
	err := readTx(ctx, s, func(tx pgx.Tx) error {
		if err := readable(ctx, tx, viewer, roomID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT e.cursor, e.kind, e.ref, e.at, `+messageCols+` FROM room_events e
			JOIN room_messages m ON m.id = e.ref WHERE e.room_id = $1 AND e.cursor > $2 ORDER BY e.cursor LIMIT $3`,
			roomID, after, eventsMax)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e Event
			m, err := scanMessage(rows, viewer, &e.Cursor, &e.Kind, &e.Ref, &e.At)
			if err != nil {
				return err
			}
			e.Message = &m
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, messageErr("events", err)
}
