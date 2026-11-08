package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// B28.452 — the Lens side of Chat's Share (talyvor-suite B28.127, apps/bff/chat_shares.go). A private chat lives in
// its person's browser; sharing it hands Lens a copy, read by anyone holding the link until the link is turned off.
//
//	POST   /v1/workspaces/{wsID}/chat-shares       {conversation_id, title, messages} → 201 the link
//	GET    /v1/workspaces/{wsID}/chat-shares       → the live links, newest first
//	DELETE /v1/workspaces/{wsID}/chat-shares/{id}  → 204, the copy deleted; 404 when it is not this workspace's
//	GET    /v1/public/chat-shares/{token}          NO CREDENTIAL → {title, messages, created_at}, or 404
//
// Another workspace's credential never reaches the first three: workspaceIsolationMiddleware refuses it first.

// chatShareMaxBytes bounds a shared copy, as the BFF does.
const chatShareMaxBytes = 512 << 10

// chatShareTitleMax is the longest title a link carries, in characters.
const chatShareTitleMax = 200

// chatShareMessage is one turn of a shared chat: who said it and what.
type chatShareMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatShare is a live link, as its workspace sees it.
type chatShare struct {
	ID             string    `json:"id"`
	Token          string    `json:"token"`
	ConversationID string    `json:"conversation_id"`
	Title          string    `json:"title"`
	CreatedAt      time.Time `json:"created_at"`
}

func mountChatShareRoutes(r chi.Router, pool *pgxpool.Pool) {
	r.Post("/v1/workspaces/{wsID}/chat-shares", func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			ConversationID string             `json:"conversation_id"`
			Title          string             `json:"title"`
			Messages       []chatShareMessage `json:"messages"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, chatShareMaxBytes)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"conversation_id", "title", "messages": [{"role", "content"}, …]}`)
			return
		}
		in.Title = strings.Join(strings.Fields(in.Title), " ")
		if in.Title == "" || utf8.RuneCountInString(in.Title) > chatShareTitleMax {
			writeJSONErr(w, http.StatusBadRequest, "a shared chat needs a title of at most 200 characters")
			return
		}
		if in.ConversationID == "" || len(in.ConversationID) > 200 {
			writeJSONErr(w, http.StatusBadRequest, "invalid conversation_id")
			return
		}
		if len(in.Messages) == 0 {
			writeJSONErr(w, http.StatusBadRequest, "there is nothing in this chat to share yet")
			return
		}
		for _, m := range in.Messages {
			if (m.Role != "user" && m.Role != "assistant") || strings.TrimSpace(m.Content) == "" {
				writeJSONErr(w, http.StatusBadRequest, "each turn is a question or an answer with words in it")
				return
			}
		}
		secret := make([]byte, 24) // 32 URL-safe base64 characters
		if _, err := rand.Read(secret); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "the link could not be made")
			return
		}
		messages, _ := json.Marshal(in.Messages)
		s := chatShare{ID: uuid.NewString(), Token: base64.RawURLEncoding.EncodeToString(secret), ConversationID: in.ConversationID, Title: in.Title}
		err := pool.QueryRow(req.Context(), `
			INSERT INTO chat_shares (id, workspace_id, token, conversation_id, title, messages)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`,
			s.ID, chi.URLParam(req, "wsID"), s.Token, s.ConversationID, s.Title, messages).Scan(&s.CreatedAt)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "the link could not be made")
			return
		}
		writeJSONOK(w, http.StatusCreated, s)
	})

	r.Get("/v1/workspaces/{wsID}/chat-shares", func(w http.ResponseWriter, req *http.Request) {
		rows, err := pool.Query(req.Context(), `
			SELECT id, token, conversation_id, title, created_at FROM chat_shares
			WHERE workspace_id = $1 ORDER BY created_at DESC, id`, chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "the links could not be read")
			return
		}
		out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[chatShare])
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "the links could not be read")
			return
		}
		if out == nil {
			out = []chatShare{}
		}
		writeJSONOK(w, http.StatusOK, out)
	})

	r.Delete("/v1/workspaces/{wsID}/chat-shares/{id}", func(w http.ResponseWriter, req *http.Request) {
		tag, err := pool.Exec(req.Context(), `DELETE FROM chat_shares WHERE workspace_id = $1 AND id = $2`,
			chi.URLParam(req, "wsID"), chi.URLParam(req, "id"))
		switch {
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, "the link could not be turned off")
		case tag.RowsAffected() == 0:
			writeJSONErr(w, http.StatusNotFound, "share not found")
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
}

// publicChatShareHandler is GET /v1/public/chat-shares/{token}, for anyone holding the link: the copy's words only,
// never the workspace or the conversation it came from.
func publicChatShareHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var out struct {
			Title     string          `json:"title"`
			Messages  json.RawMessage `json:"messages"`
			CreatedAt time.Time       `json:"created_at"`
		}
		err := pool.QueryRow(req.Context(), `SELECT title, messages, created_at FROM chat_shares WHERE token = $1`,
			chi.URLParam(req, "token")).Scan(&out.Title, &out.Messages, &out.CreatedAt)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			writeJSONErr(w, http.StatusNotFound, "chat not found")
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, "the chat could not be read")
		default:
			writeJSONOK(w, http.StatusOK, out)
		}
	}
}
