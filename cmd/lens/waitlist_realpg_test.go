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
