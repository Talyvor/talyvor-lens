package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/partnerkey"
)

// B62.5 — the partner key reads the partner's view and is refused on every other route, read from the router source:
// every "/v1/..." path literal in cmd/lens and internal/api, each by every method. The refusal is Authenticate's own
// (auth.ErrPartnerRoute), so it holds for the handlers behind AuthMiddleware and those that authenticate themselves.

const b625PartnerKey = partnerkey.KeyPrefix + "000000000000000000000000000000000000000000000000"

type b625Partners struct{ revoked bool }

func (p b625Partners) Validate(_ context.Context, raw string) (*partnerkey.Key, error) {
	if raw != b625PartnerKey || p.revoked {
		return nil, partnerkey.ErrInvalid
	}
	return &partnerkey.Key{ID: 7, Name: "the account partner's risk team"}, nil
}

var b625PathLiteral = regexp.MustCompile(`"(/v1/[^"\s]*)"`)
var b625Param = regexp.MustCompile(`\{[^}]*\}`)

// b625RoutesFromSource is every /v1 path literal in the non-test Go files of cmd/lens and internal/api, its
// parameters filled in.
func b625RoutesFromSource(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, dir := range []string{".", "../../internal/api"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range b625PathLiteral.FindAllStringSubmatch(string(src), -1) {
				seen[b625Param.ReplaceAllString(m[1], "x1")] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func TestB625_PartnerKeyReadsItsViewAndIsRefusedOnEveryOtherRoute(t *testing.T) {
	am := auth.NewManager("b625-admin", nil, nil, nil).WithPartnerKeys(b625Partners{})
	try := func(method, path string) error {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+b625PartnerKey)
		actx, err := am.Authenticate(req)
		if err == nil && (actx.AuthMethod != auth.MethodPartnerKey || actx.IsAdmin || actx.WorkspaceID != "" ||
			!slices.Equal(actx.Scopes, []string{auth.ScopePartnerRead}) || actx.PartnerKeyID != 7) {
			t.Fatalf("%s %s: the partner key resolved to %+v, want partner_read and nothing else", method, path, actx)
		}
		return err
	}

	routes := b625RoutesFromSource(t)
	for _, p := range auth.PartnerRoutes {
		if !slices.Contains(routes, p) {
			t.Fatalf("the partner route %s is not in the router source: the route list is broken", p)
		}
	}
	if len(routes) < 500 {
		t.Fatalf("only %d /v1 paths read from the source, want hundreds: the route list is broken", len(routes))
	}
	refused := 0
	for _, path := range routes {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			err := try(method, path)
			reads := slices.Contains(auth.PartnerRoutes, path) && (method == http.MethodGet || method == http.MethodHead)
			switch {
			case reads && err != nil:
				t.Errorf("%s %s: %v, want the partner key admitted to read its view", method, path, err)
			case !reads && !errors.Is(err, auth.ErrPartnerRoute):
				t.Errorf("%s %s: %v, want ErrPartnerRoute", method, path, err)
			case !reads:
				refused++
			}
		}
	}
	t.Logf("the partner key reads %d routes and is refused on %d route and method pairs", len(auth.PartnerRoutes), refused)

	// An escaped path that chi could route elsewhere, and a path under the view, are refused too.
	for _, path := range []string{"/v1/partner/agent-activity%2F..%2F..%2Fadmin", "/v1/partner/agent-activity/x1", "/v1/partner"} {
		if err := try(http.MethodGet, path); !errors.Is(err, auth.ErrPartnerRoute) {
			t.Errorf("GET %s: %v, want ErrPartnerRoute", path, err)
		}
	}

	// Behind AuthMiddleware the refusal is 403, before any handler runs.
	ran := false
	h := auth.AuthMiddleware(auth.New(nil), am)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true }))
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws-1/agents", nil)
	req.Header.Set("Authorization", "Bearer "+b625PartnerKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || ran {
		t.Fatalf("the partner key on a workspace route behind AuthMiddleware: %d (handler ran: %v), want 403", rec.Code, ran)
	}

	// A revoked key is refused on its own view.
	revoked := auth.NewManager("b625-admin", nil, nil, nil).WithPartnerKeys(b625Partners{revoked: true})
	req = httptest.NewRequest(http.MethodGet, auth.PartnerRoutes[0], nil)
	req.Header.Set("Authorization", "Bearer "+b625PartnerKey)
	if _, err := revoked.Authenticate(req); !errors.Is(err, auth.ErrInvalidAuth) {
		t.Fatalf("a revoked partner key: %v, want ErrInvalidAuth", err)
	}
}
