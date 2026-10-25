package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// B44.6 — the talyvor.com waitlist. The suite BFF writes a signup on the provision secret it already
// holds; the operator reads every row back. Not tenant data: no workspace, no IP, no user agent.

type waitlistRequest struct {
	Email       string `json:"email"`
	Page        string `json:"page"`
	UTMSource   string `json:"utm_source"`
	UTMMedium   string `json:"utm_medium"`
	UTMCampaign string `json:"utm_campaign"`
	Ref         string `json:"ref"` // B45.35: the referrer's ref_code from talyvor.com/?ref=<code>
}

type waitlistSignup struct {
	Email       string    `json:"email"`
	CreatedAt   time.Time `json:"created_at"`
	Page        *string   `json:"page"`
	UTMSource   *string   `json:"utm_source"`
	UTMMedium   *string   `json:"utm_medium"`
	UTMCampaign *string   `json:"utm_campaign"`
	RefCode     string    `json:"ref_code"`
	Referrals   int       `json:"referrals"` // rows whose referred_by is this row
}

// newWaitlistRefCode is 10 lowercase base32 characters (50 bits) from crypto/rand — nothing of the email in it.
func newWaitlistRefCode() (string, error) {
	b := make([]byte, 7)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.EncodeToString(b))[:10], nil
}

// mountWaitlistRoute registers POST /v1/waitlist on the bare router, beside /v1/provision and for the same
// reason: its caller carries LENS_PROVISION_SECRET, not an API key. Unset secret ⇒ not registered.
func mountWaitlistRoute(r chi.Router, secret string, pool *pgxpool.Pool) {
	if secret == "" {
		return
	}
	r.Post("/v1/waitlist", newWaitlistSignupHandler(secret, pool))
}

// normaliseWaitlistEmail trims and lowercases, then accepts at most 254 chars, exactly one @, a non-empty
// local part and a dot inside the domain. ok=false means 400.
func normaliseWaitlistEmail(s string) (string, bool) {
	e := strings.ToLower(strings.TrimSpace(s))
	local, domain, found := strings.Cut(e, "@")
	if !found || len(e) > 254 || local == "" || strings.Contains(domain, "@") ||
		!strings.Contains(strings.Trim(domain, "."), ".") || strings.ContainsAny(e, " \t\r\n") {
		return "", false
	}
	return e, true
}

// waitlistField trims a free-text field to at most 200 characters; empty is stored as NULL.
func waitlistField(s string) *string {
	r := []rune(strings.TrimSpace(s))
	if len(r) == 0 {
		return nil
	}
	if len(r) > 200 {
		r = r[:200]
	}
	v := string(r)
	return &v
}

func newWaitlistSignupHandler(secret string, pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if subtle.ConstantTimeCompare([]byte(req.Header.Get(provisionSecretHeader)), []byte(secret)) != 1 {
			// 403 to any other credential — the admin key and workspace keys may not write here.
			if req.Header.Get(provisionSecretHeader) == "" && req.Header.Get("Authorization") == "" {
				writeJSONErr(w, http.StatusUnauthorized, "provisioning credentials required")
				return
			}
			writeJSONErr(w, http.StatusForbidden, "only the provisioning credential may add to the waitlist")
			return
		}
		var in waitlistRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 8<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		email, ok := normaliseWaitlistEmail(in.Email)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "a valid email address is required")
			return
		}
		code, err := newWaitlistRefCode()
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "could not store the signup")
			return
		}
		// An unknown ref leaves referred_by NULL; a repeat email inserts nothing, so it can never refer itself or count twice.
		// shortcut: a ref_code collision (1 in 2^50 per pair) is a 500 the BFF can retry, not a retry loop here.
		stored := true
		err = pool.QueryRow(req.Context(), `INSERT INTO waitlist_signups (email, page, utm_source, utm_medium, utm_campaign, ref_code, referred_by)
			VALUES ($1, $2, $3, $4, $5, $6, (SELECT id FROM waitlist_signups WHERE ref_code = $7))
			ON CONFLICT (email) DO NOTHING RETURNING ref_code`,
			email, waitlistField(in.Page), waitlistField(in.UTMSource), waitlistField(in.UTMMedium), waitlistField(in.UTMCampaign),
			code, strings.ToLower(strings.TrimSpace(in.Ref))).Scan(&code)
		if errors.Is(err, pgx.ErrNoRows) {
			stored = false
			err = pool.QueryRow(req.Context(), `SELECT ref_code FROM waitlist_signups WHERE email = $1`, email).Scan(&code)
		}
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "could not store the signup")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"stored": stored, "ref_code": code})
	}
}

// newWaitlistListHandler returns every signup, oldest first. Gated by requireAdminOrOperatorRead at the mount.
func newWaitlistListHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		rows, err := pool.Query(req.Context(), `SELECT s.email, s.created_at, s.page, s.utm_source, s.utm_medium, s.utm_campaign, s.ref_code,
			(SELECT count(*) FROM waitlist_signups r WHERE r.referred_by = s.id)
			FROM waitlist_signups s ORDER BY s.created_at, s.id`)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "could not read the waitlist")
			return
		}
		defer rows.Close()
		out := []waitlistSignup{}
		for rows.Next() {
			var s waitlistSignup
			if err := rows.Scan(&s.Email, &s.CreatedAt, &s.Page, &s.UTMSource, &s.UTMMedium, &s.UTMCampaign, &s.RefCode, &s.Referrals); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, "could not read the waitlist")
				return
			}
			out = append(out, s)
		}
		if rows.Err() != nil {
			writeJSONErr(w, http.StatusInternalServerError, "could not read the waitlist")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"signups": out})
	}
}
