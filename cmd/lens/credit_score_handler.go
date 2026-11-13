package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B30.39 — a credit score for companies, with the reasons shown (economy/credit_score.go).
//
//	GET  /v1/money/credit-score          the company's score from 0 to 1000, its band A to E, the reasons worst
//	                                     first, the history it came from, and the latest review
//	POST /v1/money/credit-score/review   {note} — ask a person to review it: a task on the operator's list
//	                                     (`lens credit-scores`); while one is open, asking again answers that one
//
// The workspace is the key's; only its owner or an admin reads the score or asks for a review. Mounted in the
// authed group.

type creditScorer interface {
	CreditScore(ctx context.Context, workspaceID string, now time.Time) (economy.CreditScore, error)
	RequestCreditScoreReview(ctx context.Context, workspaceID, by, note string, now time.Time) (economy.CreditScoreReview, error)
}

func mountCreditScoreRoutes(r chi.Router, store creditScorer) {
	owner := func(w http.ResponseWriter, req *http.Request) (ws, who string, ok bool) {
		ws, _ = auth.WorkspaceIdentity(req.Context())
		who, owner := storedanswers.OwnerOrAdmin(req.Context())
		if ws == "" || !owner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may see its credit score")
			return "", "", false
		}
		return ws, who, true
	}
	refused := func(w http.ResponseWriter, err error) {
		if errors.Is(err, economy.ErrCreditScoreCompaniesOnly) {
			writeJSONErr(w, http.StatusConflict, err.Error())
			return
		}
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
	r.Get("/v1/money/credit-score", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := owner(w, req)
		if !ok {
			return
		}
		cs, err := store.CreditScore(req.Context(), ws, time.Now())
		if err != nil {
			refused(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, cs)
	})
	r.Post("/v1/money/credit-score/review", func(w http.ResponseWriter, req *http.Request) {
		ws, who, ok := owner(w, req)
		if !ok {
			return
		}
		var in struct {
			Note string `json:"note"`
		}
		if err := json.NewDecoder(io.LimitReader(req.Body, 1<<16)).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"note": "<optional: what the reviewer should know>"}`)
			return
		}
		review, err := store.RequestCreditScoreReview(req.Context(), ws, who, in.Note, time.Now())
		if err != nil {
			refused(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, review)
	})
}
