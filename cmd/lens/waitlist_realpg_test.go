package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// B44.6 — the waitlist keeps one row per email on the provision secret, refuses every other credential and a
// malformed address without writing, and hands the operator every row.
func TestWaitlist_OneRowPerEmail_OnlyOnTheProvisionSecret_ReadByTheOperator(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	post := newWaitlistSignupHandler("prov-secret", pool)
	signup := func(header, value, body string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/waitlist", strings.NewReader(body))
		req.Header.Set(header, value)
		w := httptest.NewRecorder()
		post(w, req)
		return w.Code, w.Body.String()
	}
	rows := func(email string) (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM waitlist_signups WHERE email = $1`, email).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	body := `{"email":"  Ada@Example.COM ","page":"/","utm_source":"meta","utm_medium":"paid","utm_campaign":"launch"}`
	if code, out := signup(provisionSecretHeader, "prov-secret", body); code != 200 || !strings.Contains(out, `"stored":true`) {
		t.Fatalf("first signup: %d %s", code, out)
	}
	if code, out := signup(provisionSecretHeader, "prov-secret", `{"email":"ada@example.com"}`); code != 200 || !strings.Contains(out, `"stored":false`) {
		t.Fatalf("second signup: %d %s", code, out)
	}
	if n := rows("ada@example.com"); n != 1 {
		t.Fatalf("the same email twice left %d rows, want 1", n)
	}

	for _, key := range []string{"Bearer ws-key", "Bearer admin-key"} {
		if code, _ := signup("Authorization", key, `{"email":"bob@example.com"}`); code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", key, code)
		}
	}
	if code, _ := signup(provisionSecretHeader, "wrong", `{"email":"bob@example.com"}`); code != http.StatusForbidden {
		t.Errorf("wrong secret: %d, want 403", code)
	}
	if n := rows("bob@example.com"); n != 0 {
		t.Fatalf("a refused credential wrote %d rows", n)
	}

	for _, bad := range []string{"", "no-at.example.com", "a@b@example.com", "a@localhost", "@example.com", "a@.com.", strings.Repeat("a", 250) + "@x.com"} {
		if code, _ := signup(provisionSecretHeader, "prov-secret", `{"email":"`+bad+`"}`); code != http.StatusBadRequest {
			t.Errorf("%q: %d, want 400", bad, code)
		}
	}
	var total int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM waitlist_signups`).Scan(&total); err != nil || total != 1 {
		t.Fatalf("after refusals the table holds %d rows (%v), want 1", total, err)
	}

	signup(provisionSecretHeader, "prov-secret", `{"email":"zed@example.org"}`)
	list := requireAdminOrOperatorRead(credentialAuth{}, newWaitlistListHandler(pool))
	read := func(key string) (int, []byte) {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/waitlist", nil)
		if key != "" {
			req.Header.Set("Authorization", key)
		}
		w := httptest.NewRecorder()
		list(w, req)
		return w.Code, w.Body.Bytes()
	}
	code, out := read("Bearer operator-read-key")
	var got struct{ Signups []waitlistSignup }
	if code != 200 || json.Unmarshal(out, &got) != nil {
		t.Fatalf("operator read: %d %s", code, out)
	}
	if len(got.Signups) != 2 || got.Signups[0].Email != "ada@example.com" || got.Signups[1].Email != "zed@example.org" ||
		got.Signups[0].UTMCampaign == nil || *got.Signups[0].UTMCampaign != "launch" || got.Signups[1].Page != nil {
		t.Fatalf("operator read returned %s", out)
	}
	if code, _ := read(""); code != http.StatusUnauthorized {
		t.Errorf("no key: %d, want 401", code)
	}
}

// B45.35 — a signup through a code adds exactly one referral to that code's row; the same email again, and an email
// using its own code, add none; each email's code is random, distinct, and carries nothing of the email.
func TestWaitlist_ReferralCode_CountsOnceAndRevealsNothing(t *testing.T) {
	pool := agentRoutesDB(t)
	post := newWaitlistSignupHandler("prov-secret", pool)
	signup := func(email, ref string) (out struct {
		Stored  bool   `json:"stored"`
		RefCode string `json:"ref_code"`
	}) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/waitlist", strings.NewReader(`{"email":"`+email+`","ref":"`+ref+`"}`))
		req.Header.Set(provisionSecretHeader, "prov-secret")
		w := httptest.NewRecorder()
		post(w, req)
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.RefCode) != 10 {
			t.Fatalf("signup %s ref=%q: %d %s", email, ref, w.Code, w.Body.String())
		}
		return out
	}
	referrals := func(email string) (n int) {
		t.Helper()
		list := requireAdminOrOperatorRead(credentialAuth{}, newWaitlistListHandler(pool))
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/waitlist", nil)
		req.Header.Set("Authorization", "Bearer operator-read-key")
		w := httptest.NewRecorder()
		list(w, req)
		var got struct{ Signups []waitlistSignup }
		if json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatalf("list: %d %s", w.Code, w.Body.String())
		}
		for _, s := range got.Signups {
			if s.Email == email {
				return s.Referrals
			}
		}
		t.Fatalf("%s not listed", email)
		return 0
	}

	ada := signup("ada@example.com", "")
	bob := signup("bob@example.com", ada.RefCode)
	if !bob.Stored || referrals("ada@example.com") != 1 {
		t.Fatalf("a signup through ada's code: stored=%v, ada has %d referrals, want 1", bob.Stored, referrals("ada@example.com"))
	}
	if again := signup("bob@example.com", ada.RefCode); again.Stored || again.RefCode != bob.RefCode {
		t.Fatalf("bob again: %+v, want stored=false with bob's own code %s", again, bob.RefCode)
	}
	if self := signup("ada@example.com", ada.RefCode); self.Stored || self.RefCode != ada.RefCode {
		t.Fatalf("ada through her own code: %+v", self)
	}
	if n := referrals("ada@example.com"); n != 1 {
		t.Fatalf("after a repeat and a self-referral ada has %d referrals, want 1", n)
	}
	if n := referrals("bob@example.com"); n != 0 {
		t.Fatalf("bob has %d referrals, want 0", n)
	}
	unknown := signup("cy@example.com", "nosuchcode")
	if !unknown.Stored {
		t.Fatal("an unknown code refused the signup")
	}

	if ada.RefCode == bob.RefCode {
		t.Fatalf("two emails share the code %s", ada.RefCode)
	}
	for email, code := range map[string]string{"ada@example.com": ada.RefCode, "bob@example.com": bob.RefCode} {
		for _, part := range []string{"ada", "bob", "example", "com"} {
			if strings.Contains(email, part) && strings.Contains(code, part) {
				t.Errorf("%s's code %s contains %q", email, code, part)
			}
		}
	}
}
