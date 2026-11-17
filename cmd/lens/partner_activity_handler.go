package main

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partnerkey"
)

// partner_activity_handler.go — B62.5: A READ-ONLY VIEW FOR THE ACCOUNT PARTNER'S RISK TEAM.
//
//	GET  /v1/partner/agent-activity          every agent payment, newest first; filters from, to, account, state; pages
//	GET  /v1/partner/agent-activity/export   the same rows as a CSV
//	GET  /v1/partner/agent-activity/summary  counts by day, and by state
//
// Read with a partner key (internal/partnerkey) and nothing else: a workspace key, a moderator key, the operator read
// key and the admin key are all refused here, and auth.Manager refuses the partner key on every other route. Every
// read is a partner_key_uses row, written before it is answered; a row that cannot be written answers 503 and shows
// nothing. In Sandbox the view shows Sandbox payments only (economy.PartnerViewMode).
//
//	POST /v1/admin/partner-keys              makes a key, its raw value shown once   admin key + step-up code
//	POST /v1/admin/partner-keys/{id}/revoke  revokes one                             admin key + step-up code
//	GET  /v1/admin/partner-keys              every key, never its value              admin key
//	GET  /v1/admin/partner-keys/{id}/uses    a key's recorded reads                  admin key

const (
	partnerPageDefault = 100
	partnerPageMax     = 500
	// partnerExportMax is the most rows one CSV export holds; narrow the dates for more.
	partnerExportMax = 50_000
)

// partnerKeyUses is the slice of *partnerkey.Store the view's gate needs.
type partnerKeyUses interface {
	RecordUse(ctx context.Context, keyID int64, method, path, query string) error
}

// partnerActivityReader is *economy.DualTokenStore.
type partnerActivityReader interface {
	PartnerViewMode(ctx context.Context) (string, error)
	PartnerActivity(ctx context.Context, f economy.PartnerFilter) ([]economy.PartnerPayment, error)
	PartnerActivitySummary(ctx context.Context, f economy.PartnerFilter) ([]economy.PartnerDay, error)
}

// requirePartnerRead admits a partner key, and only one, and records the read before next answers it.
func requirePartnerRead(am adminAuthenticator, uses partnerKeyUses, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actx, err := am.Authenticate(r)
		if refuseAuthUnavailable(w, err) {
			return
		}
		if errors.Is(err, auth.ErrPartnerRoute) {
			writeJSONErr(w, http.StatusForbidden, "a partner key may only read this view: GET or HEAD")
			return
		}
		if err != nil || actx == nil {
			writeJSONErr(w, http.StatusUnauthorized, "a partner key is required")
			return
		}
		// AuthMethod first: HasScope answers true for the admin key, which does not read this view.
		if actx.AuthMethod != auth.MethodPartnerKey || !actx.HasScope(auth.ScopePartnerRead) || actx.PartnerKeyID == 0 {
			writeJSONErr(w, http.StatusForbidden, "this view is read with a partner key only")
			return
		}
		if err := uses.RecordUse(r.Context(), actx.PartnerKeyID, r.Method, r.URL.Path, r.URL.RawQuery); err != nil {
			writeJSONErr(w, http.StatusServiceUnavailable, "could not record this read in the audit trail, so nothing is shown")
			return
		}
		next.ServeHTTP(w, r)
	}
}

// parsePartnerTime reads an RFC 3339 time or a YYYY-MM-DD day (its start, UTC).
func parsePartnerTime(name, v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.DateOnly, v); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%s must be a date (2026-11-17) or a time (2026-11-17T09:00:00Z)", name)
}

// parsePartnerFilter reads from, to, account, state, limit and cursor.
func parsePartnerFilter(q url.Values) (economy.PartnerFilter, error) {
	var f economy.PartnerFilter
	var err error
	if f.From, err = parsePartnerTime("from", q.Get("from")); err != nil {
		return f, err
	}
	if f.To, err = parsePartnerTime("to", q.Get("to")); err != nil {
		return f, err
	}
	if q.Get("to") != "" && !strings.Contains(q.Get("to"), "T") {
		f.To = f.To.AddDate(0, 0, 1) // a day "to" takes in the whole of that day
	}
	f.AccountID = strings.TrimSpace(q.Get("account"))
	if f.State = q.Get("state"); f.State != "" && !slices.Contains(economy.PartnerStates, f.State) {
		return f, fmt.Errorf("state must be one of %s", strings.Join(economy.PartnerStates, ", "))
	}
	f.Limit = partnerPageDefault
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > partnerPageMax {
			return f, fmt.Errorf("limit must be a number from 1 to %d", partnerPageMax)
		}
		f.Limit = n
	}
	if v := q.Get("cursor"); v != "" {
		raw, err := base64.RawURLEncoding.DecodeString(v)
		at, id, ok := strings.Cut(string(raw), "|")
		t, terr := time.Parse(time.RFC3339Nano, at)
		if err != nil || !ok || terr != nil || id == "" {
			return f, errors.New("cursor is not one this view gave")
		}
		f.BeforeAt, f.BeforeID = t, id
	}
	return f, nil
}

func partnerCursor(p economy.PartnerPayment) string {
	return base64.RawURLEncoding.EncodeToString([]byte(p.At.UTC().Format(time.RFC3339Nano) + "|" + p.ID))
}

// partnerMode answers the view's mode, or writes why there is none and returns "".
func partnerMode(w http.ResponseWriter, r *http.Request, store partnerActivityReader) string {
	mode, err := store.PartnerViewMode(r.Context())
	switch {
	case errors.Is(err, economy.ErrPartnerViewLive):
		writeJSONErr(w, http.StatusConflict, "not shown: some money here is live, and a payment's record does not say yet whether it was Sandbox or Live")
		return ""
	case err != nil:
		writeJSONErr(w, http.StatusInternalServerError, "could not read whether this is Sandbox or Live")
		return ""
	}
	return mode
}

func newPartnerActivityHandler(store partnerActivityReader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, err := parsePartnerFilter(r.URL.Query())
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		mode := partnerMode(w, r, store)
		if mode == "" {
			return
		}
		rows, err := store.PartnerActivity(r.Context(), f)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "could not read the agent activity")
			return
		}
		next := ""
		if len(rows) == f.Limit {
			next = partnerCursor(rows[len(rows)-1])
		}
		if rows == nil {
			rows = []economy.PartnerPayment{}
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"mode": mode, "payments": rows, "next_cursor": next})
	})
}

func newPartnerActivityExportHandler(store partnerActivityReader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, err := parsePartnerFilter(r.URL.Query())
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		mode := partnerMode(w, r, store)
		if mode == "" {
			return
		}
		var all []economy.PartnerPayment
		for f.Limit = partnerPageMax; len(all) < partnerExportMax; {
			page, err := store.PartnerActivity(r.Context(), f)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, "could not read the agent activity")
				return
			}
			all = append(all, page...)
			if len(page) < f.Limit {
				break
			}
			f.BeforeAt, f.BeforeID = page[len(page)-1].At, page[len(page)-1].ID
		}
		all = all[:min(len(all), partnerExportMax)]
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="agent-activity-`+mode+`.csv"`)
		cw := csv.NewWriter(w)
		_ = cw.Write(economy.PartnerPaymentFields)
		for _, p := range all {
			version := ""
			if p.RulesVersion != nil {
				version = strconv.FormatInt(*p.RulesVersion, 10)
			}
			_ = cw.Write([]string{csvCell(p.ID), p.At.UTC().Format(time.RFC3339), csvCell(p.AccountID), p.VerificationLevel.String(),
				csvCell(p.AgentID), strconv.FormatInt(p.AmountMinor, 10), csvCell(p.Currency), csvCell(p.Rule), version,
				strconv.FormatBool(p.OwnerApproved), csvCell(p.ApprovalKey), csvCell(p.ProofID), p.State})
		}
		cw.Flush()
	})
}

func newPartnerActivitySummaryHandler(store partnerActivityReader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, err := parsePartnerFilter(r.URL.Query())
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		mode := partnerMode(w, r, store)
		if mode == "" {
			return
		}
		days, err := store.PartnerActivitySummary(r.Context(), f)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "could not count the agent activity")
			return
		}
		if days == nil {
			days = []economy.PartnerDay{}
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"mode": mode, "days": days})
	})
}

// partnerKeyAdmin is *partnerkey.Store.
type partnerKeyAdmin interface {
	Create(ctx context.Context, name, by string) (string, *partnerkey.Key, error)
	Revoke(ctx context.Context, id int64, by string) error
	List(ctx context.Context) ([]partnerkey.Key, error)
	Uses(ctx context.Context, keyID int64, limit int) ([]partnerkey.Use, error)
}

// newPartnerKeyCreateHandler makes a partner key named by the body's name, made by the operator requireStepUp named.
func newPartnerKeyCreateHandler(keys partnerKeyAdmin) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
			writeJSONErr(w, http.StatusBadRequest, `name the key: {"name": "<the partner and what it is for>"}`)
			return
		}
		raw, k, err := keys.Create(r.Context(), body.Name, r.Header.Get(moderatorOperatorHeader))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "could not make the key")
			return
		}
		writeJSONOK(w, http.StatusCreated, map[string]any{"key": raw, "partner_key": k,
			"note": "This is the only time the key is shown. It reads the partner's view of agent activity and nothing else."})
	})
}

func partnerKeyID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSONErr(w, http.StatusBadRequest, "the key's id is a number")
		return 0, false
	}
	return id, true
}

func newPartnerKeyRevokeHandler(keys partnerKeyAdmin) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := partnerKeyID(w, r)
		if !ok {
			return
		}
		switch err := keys.Revoke(r.Context(), id, r.Header.Get(moderatorOperatorHeader)); {
		case errors.Is(err, partnerkey.ErrNotFound):
			writeJSONErr(w, http.StatusNotFound, "no partner key by that id")
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, "could not revoke the key")
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"id": id, "revoked": true})
		}
	})
}

func newPartnerKeyListHandler(keys partnerKeyAdmin) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		list, err := keys.List(r.Context())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "could not list the partner keys")
			return
		}
		if list == nil {
			list = []partnerkey.Key{}
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"partner_keys": list})
	})
}

func newPartnerKeyUsesHandler(keys partnerKeyAdmin) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := partnerKeyID(w, r)
		if !ok {
			return
		}
		uses, err := keys.Uses(r.Context(), id, 1000)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "could not read the key's reads")
			return
		}
		if uses == nil {
			uses = []partnerkey.Use{}
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"uses": uses})
	})
}
