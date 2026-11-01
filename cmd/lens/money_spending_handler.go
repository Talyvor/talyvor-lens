package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B53.3 — every payment gets a category; one correction teaches it; spend tags (economy/spending.go).
//
//	GET /v1/money/spending?from=&to=   — the workspace's card charges and payments out in [from, to) (YYYY-MM-DD or
//	                          RFC 3339; this month by default), newest first, each with its category, where that came
//	                          from and its tags; and totals for the owner's own wallet and each agent's, per currency,
//	                          by category and by tag
//	PUT /v1/money/spending/merchants   {merchant, category} — always put this merchant's payments, past and future, in
//	                          category; an empty category forgets the correction
//	PUT /v1/money/entries/{id}/tags    {tags: [...]} — the owner's own tags on one movement of money, replacing its
//	                          last ones
//
// The workspace is the key's; only its owner or an admin may use these. Mounted in the authed group.

type moneySpendingBank interface {
	Spending(ctx context.Context, workspaceID string, from, to time.Time) (economy.Spending, error)
	SetSpendCategory(ctx context.Context, workspaceID, merchant, category, by string) error
	SetEntryTags(ctx context.Context, workspaceID, entryID string, tags []string) ([]string, error)
}

func mountMoneySpendingRoutes(r chi.Router, bank moneySpendingBank) {
	owner := func(w http.ResponseWriter, req *http.Request) (string, string, bool) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusBadRequest, "spending is a workspace's: use a workspace's key")
			return "", "", false
		}
		who, ok := storedanswers.OwnerOrAdmin(req.Context())
		if !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may see or sort its spending")
			return "", "", false
		}
		return ws, who, true
	}
	answer := func(w http.ResponseWriter, err error, ok func()) {
		switch {
		case errors.Is(err, economy.ErrSpending):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrSpendEntryNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			ok()
		}
	}
	r.Get("/v1/money/spending", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := owner(w, req)
		if !ok {
			return
		}
		var at [2]time.Time
		for i, name := range []string{"from", "to"} {
			v := req.URL.Query().Get(name)
			if v == "" {
				continue
			}
			t, err := time.Parse(time.DateOnly, v)
			if err != nil {
				if t, err = time.Parse(time.RFC3339, v); err != nil {
					writeJSONErr(w, http.StatusBadRequest, name+" must be a date (YYYY-MM-DD) or an RFC 3339 time")
					return
				}
			}
			at[i] = t
		}
		out, err := bank.Spending(req.Context(), ws, at[0], at[1])
		answer(w, err, func() { writeJSONOK(w, http.StatusOK, out) })
	})
	r.Put("/v1/money/spending/merchants", func(w http.ResponseWriter, req *http.Request) {
		ws, who, ok := owner(w, req)
		if !ok {
			return
		}
		var in struct{ Merchant, Category string }
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"merchant", "category"}`)
			return
		}
		err := bank.SetSpendCategory(req.Context(), ws, in.Merchant, in.Category, who)
		answer(w, err, func() {
			writeJSONOK(w, http.StatusOK, map[string]string{"merchant": in.Merchant, "category": in.Category})
		})
	})
	r.Put("/v1/money/entries/{id}/tags", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := owner(w, req)
		if !ok {
			return
		}
		var in struct{ Tags []string }
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"tags": [...]}`)
			return
		}
		tags, err := bank.SetEntryTags(req.Context(), ws, chi.URLParam(req, "id"), in.Tags)
		answer(w, err, func() {
			writeJSONOK(w, http.StatusOK, map[string]any{"entry_id": chi.URLParam(req, "id"), "tags": tags})
		})
	})
}
