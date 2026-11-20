package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// openRegistrationGrants is the connector store as /oauth/register leaves it: anyone may register a client with any
// https redirect_uri, so "the client registered it" says nothing about whether Talyvor should send a browser there.
type openRegistrationGrants struct {
	connectorGrants
	beginErr error
}

func (openRegistrationGrants) CheckClient(context.Context, string, string) error { return nil }

func (g openRegistrationGrants) Begin(context.Context, string, string, string, string, string, string) (string, error) {
	return "cg_test", g.beginErr
}

// B67.1 (CodeQL go/unvalidated-url-redirection #56): /oauth/authorize answered every request error with a 302 to the
// redirect_uri before anyone had seen or approved the client, so a self-registered client turned Lens's own URL into
// a redirect to any site. An error is answered here, on Lens; the browser only leaves for a client's site from the
// connect page, after the owner has read the client's name and host and approved it.
func TestB671_AuthorizeNeverRedirectsToAnUnapprovedClient(t *testing.T) {
	const pub, evil = "https://lens.test", "https://attacker.example/cb"
	challenge := strings.Repeat("c", 43)
	good := url.Values{"response_type": {"code"}, "client_id": {"tc_x"}, "redirect_uri": {evil}, "state": {"s"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "resource": {pub + listingResourcePrefix + "lst_1"}}
	with := func(k, v string) url.Values {
		q := url.Values{}
		for kk, vv := range good {
			q[kk] = vv
		}
		q.Set(k, v)
		return q
	}
	for _, tc := range []struct {
		name     string
		q        url.Values
		beginErr error
		code     string
	}{
		{"a response type other than code", with("response_type", "token"), nil, "unsupported_response_type"},
		{"no PKCE", with("code_challenge_method", "plain"), nil, "invalid_request"},
		{"a state that is too long", with("state", strings.Repeat("s", 513)), nil, "invalid_request"},
		{"a resource that is not a listing", with("resource", "https://elsewhere.example/"), nil, "invalid_target"},
		{"a request that could not be opened", good, errors.New("db down"), "server_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chi.NewRouter()
			mountConnectorPublicRoutes(r, openRegistrationGrants{beginErr: tc.beginErr}, nil, nil, pub)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+tc.q.Encode(), nil))
			if loc := w.Header().Get("Location"); loc != "" || w.Code != http.StatusBadRequest && w.Code != http.StatusInternalServerError {
				t.Fatalf("authorize = %d to %q, want an answer on Lens and no redirect", w.Code, loc)
			}
			if !strings.Contains(w.Body.String(), `"error":"`+tc.code+`"`) {
				t.Fatalf("body %s does not name the OAuth error %s", w.Body.String(), tc.code)
			}
		})
	}

	// The one redirect left is to Talyvor's own connect page.
	r := chi.NewRouter()
	mountConnectorPublicRoutes(r, openRegistrationGrants{}, nil, nil, pub)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+good.Encode(), nil))
	if w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), connectURLBase+"?request=") {
		t.Fatalf("a good request = %d to %q, want the connect page", w.Code, w.Header().Get("Location"))
	}
}
