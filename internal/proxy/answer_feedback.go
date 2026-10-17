package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/poolroyalty"
)

// B23.1 — A THUMBS-DOWN REMOVES A WRONG ANSWER.
//
// Every answer a request is served from a cache, and every fresh answer the cache stores, is
// remembered against the request's X-Talyvor-Request-ID for servedAnswerTTL. POST /v1/feedback with
// that request id and signal "negative" (or "repeat", a client's flag that the answer did not
// satisfy) removes that stored answer — its rows in prompt_embeddings (a pooled row's variants go
// with it), and its exact-cache copies — so it is never served to anyone again, and writes an
// answer_removals row naming who marked it. A thumbs-down on a fresh answer removes what it was just
// stored as. Earnings already recorded are untouched.

// servedAnswerTTL is how long after an answer a thumbs-down on it still removes it.
const servedAnswerTTL = 7 * 24 * time.Hour

// servedAnswer names the stored answer a request was given: the rows it was served from (IDs), the
// hashes its copies are stored under (Hashes), the workspace that contributed it when it came from
// the shared pool (Contributor — its own copies of the answer go too), and its sha256 (Digest):
// only copies still holding exactly that answer are removed.
type servedAnswer struct {
	Layer       string   `json:"layer"`
	IDs         []string `json:"ids,omitempty"`
	Hashes      []string `json:"hashes,omitempty"`
	Contributor string   `json:"contributor,omitempty"`
	Digest      string   `json:"digest"`
}

// answerHashes are the two keys a fresh answer to this request is stored under: the workspace's own
// copy and its shared-pool copy (exact and semantic alike — see cache.PromptHash).
func answerHashes(provider, model, cachePrompt, rawPrompt, reqFP string) []string {
	return []string{
		cache.PromptHash(provider, model, cache.FingerprintedKey(cachePrompt, reqFP)),
		cache.PromptHash(provider, model, cache.FingerprintedKey(pooledPromptKey(rawPrompt), reqFP)),
	}
}

// rememberServed records which stored answer answered requestID. Best effort: without the Redis the
// exact cache runs on there is nowhere to keep it, and a failure never touches the response.
func (p *Proxy) rememberServed(ctx context.Context, wsID, requestID, layer string, ids, hashes []string, contributor string, answer []byte) {
	if p.exact == nil || wsID == "" || requestID == "" {
		return
	}
	ref, _ := json.Marshal(servedAnswer{Layer: layer, IDs: ids, Hashes: hashes, Contributor: contributor, Digest: cache.AnswerDigest(answer)})
	if err := p.exact.RememberServed(ctx, wsID, requestID, ref, servedAnswerTTL); err != nil {
		slog.Warn("proxy: could not remember the served answer (a thumbs-down on it will remove nothing)",
			slog.String("request_id", requestID), slog.String("err", err.Error()))
	}
}

// storeAnswer stores a fresh answer (storeCaches) and remembers what it was stored as.
func (p *Proxy) storeAnswer(ctx context.Context, provider, model, cachePrompt, rawPrompt, reqFP string, turn cache.Turn, wsID, requestID string, response []byte) {
	if cacheStoreOff(ctx) {
		return
	}
	if p.storeCaches(ctx, provider, model, cachePrompt, rawPrompt, reqFP, turn, wsID, response) {
		p.rememberServed(ctx, wsID, requestID, "fresh", nil, answerHashes(provider, model, cachePrompt, rawPrompt, reqFP), "", response)
	}
}

// answerRemoval is what a thumbs-down removed.
type answerRemoval struct {
	ServedFrom    string `json:"served_from"`
	Answers       int    `json:"answers_removed"`
	ExactCopies   int    `json:"exact_copies_removed"`
	WasRemembered bool   `json:"-"`
}

// removeServedAnswer removes the stored answer workspace wsID was given for requestID.
func (p *Proxy) removeServedAnswer(ctx context.Context, wsID, requestID string) (answerRemoval, error) {
	var out answerRemoval
	if p.exact == nil {
		return out, nil
	}
	raw, err := p.exact.ServedRef(ctx, wsID, requestID)
	if err != nil || raw == nil {
		return out, err
	}
	var ref servedAnswer
	if err := json.Unmarshal(raw, &ref); err != nil {
		return out, err
	}
	out.ServedFrom, out.WasRemembered = ref.Layer, true
	hashes := append([]string(nil), ref.Hashes...)
	if p.semantic != nil {
		owners := []string{wsID}
		if ref.Contributor != "" && ref.Contributor != wsID {
			owners = append(owners, ref.Contributor)
		}
		removed, err := p.semantic.DeleteServed(ctx, ref.IDs, ref.Hashes, owners, ref.Digest)
		if err != nil {
			return out, err
		}
		out.Answers = len(removed)
		hashes = append(hashes, removed...)
	}
	seen := map[string]bool{}
	for _, h := range hashes {
		if seen[h] {
			continue
		}
		seen[h] = true
		ok, err := p.exact.DeleteIfDigest(ctx, h, ref.Digest)
		if err != nil {
			return out, err
		}
		if ok {
			out.ExactCopies++
		}
	}
	return out, nil
}

// AuditExec is the database the removal's audit row is written to.
type AuditExec interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const insertAnswerRemovalSQL = `INSERT INTO answer_removals
  (workspace_id, request_id, signal, served_from, marked_by, answers_removed, exact_copies_removed)
VALUES ($1, $2, $3, $4, $5, $6, $7)`

// markedBy names the credential that marked an answer, for the audit row.
func markedBy(ctx context.Context) (wsID, who string) {
	if actx := auth.GetAuthContext(ctx); actx != nil {
		parts := []string{actx.AuthMethod}
		if actx.IsAdmin {
			parts = []string{"operator"}
		}
		if actx.UserID != "" {
			parts = append(parts, "user:"+actx.UserID)
		}
		if actx.APIKeyID != "" {
			parts = append(parts, "key:"+actx.APIKeyID)
		}
		if actx.SessionKeyID != "" {
			parts = append(parts, "session_key:"+actx.SessionKeyID)
		}
		return actx.WorkspaceID, strings.Join(parts, " ")
	}
	if k := auth.GetAPIKey(ctx); k != nil {
		return k.WorkspaceID, "key:" + k.ID
	}
	return "", ""
}

// AnswerFeedbackHandler serves POST /v1/feedback. {"request_id", "signal": "negative"|"repeat"}
// removes the stored answer that request was given and records who marked it; "positive" changes
// nothing. The older {"prompt_hash", "signal"} form still goes to legacy (the quality scorer).
func AnswerFeedbackHandler(p *Proxy, audit AuditExec, legacy func(ctx context.Context, wsID, promptHash, signal string) (bool, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RequestID  string `json:"request_id"`
			PromptHash string `json:"prompt_hash"`
			Signal     string `json:"signal"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil || in.Signal == "" ||
			(in.RequestID == "") == (in.PromptHash == "") {
			writeError(w, http.StatusBadRequest, "signal and exactly one of request_id or prompt_hash are required")
			return
		}
		if in.PromptHash != "" {
			wsID, _, ok := feedbackWorkspace(w, r)
			if !ok {
				return
			}
			// B42.2: only the caller's own workspace's answer is scored; another workspace's
			// prompt hash is 404, exactly like one that does not exist.
			found, err := legacy(r.Context(), wsID, in.PromptHash, in.Signal)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if !found {
				writeError(w, http.StatusNotFound, "answer not found")
				return
			}
			writeFeedbackJSON(w, map[string]bool{"ok": true})
			return
		}
		switch in.Signal {
		case "positive":
			writeFeedbackJSON(w, map[string]any{"request_id": in.RequestID, "signal": in.Signal})
			return
		case "negative", "repeat":
		default:
			writeError(w, http.StatusBadRequest, "signal must be positive, negative or repeat")
			return
		}
		wsID, who, ok := feedbackWorkspace(w, r)
		if !ok {
			return
		}
		removed, err := p.removeServedAnswer(r.Context(), wsID, in.RequestID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not remove the answer: "+err.Error())
			return
		}
		if _, err := audit.Exec(r.Context(), insertAnswerRemovalSQL, wsID, in.RequestID, in.Signal,
			removed.ServedFrom, who, removed.Answers, removed.ExactCopies); err != nil {
			writeError(w, http.StatusInternalServerError, "the answer was removed but the record of it was not written: "+err.Error())
			return
		}
		writeFeedbackJSON(w, map[string]any{
			"request_id": in.RequestID, "signal": in.Signal, "stored": removed.WasRemembered,
			"served_from": removed.ServedFrom, "answers_removed": removed.Answers,
			"exact_copies_removed": removed.ExactCopies,
		})
	}
}

// feedbackWorkspace is the workspace the feedback acts on: the credential's own, or the one the
// operator names. It writes the refusal and returns ok=false when there is none.
func feedbackWorkspace(w http.ResponseWriter, r *http.Request) (wsID, who string, ok bool) {
	wsID, who = markedBy(r.Context())
	if who == "" {
		writeError(w, http.StatusUnauthorized, "no credential")
		return "", "", false
	}
	if actx := auth.GetAuthContext(r.Context()); wsID == "" && actx != nil && actx.IsAdmin {
		wsID = r.Header.Get("X-Talyvor-Workspace") // the operator names the workspace
	}
	if wsID == "" {
		writeError(w, http.StatusBadRequest, "no workspace: send X-Talyvor-Workspace")
		return "", "", false
	}
	return wsID, who, true
}

func writeFeedbackJSON(w http.ResponseWriter, v any) {
	body, _ := json.Marshal(v)
	writeBytes(w, http.StatusOK, body)
}

// hitContributor is the workspace that contributed a pooled hit, or "" for an own-cache hit.
func hitContributor(h *poolroyalty.ServedHit) string {
	if h == nil {
		return ""
	}
	return h.ContributorWorkspace
}
