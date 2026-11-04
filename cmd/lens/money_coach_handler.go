package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
)

// B53.6 — Coach, the spending agent that explains, never advises on products (economy/coach.go).
//
//	GET  /v1/money/coach        — the Coach's sentences: one for each unusual payment of the last seven days, one for
//	                              each budget at half or more, and the week's review; each with the fact it words and
//	                              whether the model's sentence or the fixed template stands
//	POST /v1/money/coach/ask    {question} — a question about a product, credit, investing, insurance, debt or gambling
//	                              gets the free-help pointers and nothing else; any other gets the Coach's sentences
//
// The workspace is the key's; only its owner or an admin may use these (spendingOwner). Mounted in the authed group.
// The Coach only reads (economy.CoachReader): no route here pays, approves or issues anything.

func mountMoneyCoachRoutes(r chi.Router, bank economy.CoachReader, ai economy.CoachInferer, model string) {
	report := func(w http.ResponseWriter, req *http.Request, ws string, wrap func(economy.CoachReport) any) {
		out, err := economy.Coach(req.Context(), bank, ai, model, ws, time.Now().UTC())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, wrap(out))
	}
	r.Get("/v1/money/coach", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		report(w, req, ws, func(out economy.CoachReport) any { return out })
	})
	r.Post("/v1/money/coach/ask", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		var in struct{ Question string }
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Question) == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"question"}`)
			return
		}
		if a, ok := economy.CoachQuestion(in.Question); ok {
			writeJSONOK(w, http.StatusOK, a)
			return
		}
		report(w, req, ws, func(out economy.CoachReport) any {
			return map[string]any{"text": "I explain your own spending: what you paid, how it compares with your budgets, and anything unusual.",
				"help": []economy.CoachHelp{}, "report": out}
		})
	})
}
