package status

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/partners"
)

// documentedKeys is every key /status.json answers, at every level, as README.md's Status section lists them (B37.3).
// A field added to the response fails TestStatusJSON_KeysAreTheDocumentedSet until it is added here and there on
// purpose.
var documentedKeys = []string{
	"status", "version", "uptime_hours", "updated_at",
	"components", "components[].name", "components[].status", "components[].latency_ms", "components[].measured",
	"components[].message", "components[].checked_at",
	"providers", "providers[].name", "providers[].status", "providers[].latency_ms", "providers[].checked_at",
	"rails", "rails[].service", "rails[].name", "rails[].mode", "rails[].status", "rails[].last_success",
	"rails[].last_failure", "rails[].capabilities", "rails[].capabilities[].key", "rails[].capabilities[].cleared",
	"rails[].lists_age_hours",
	"rails_summary", "rails_summary.up", "rails_summary.down", "rails_summary.idle", "rails_summary.down_names",
	"card_payments",
}

// plantedError is what a failing dependency says: a database user, a host name, an address, a reference id and a key.
const plantedError = "failed to connect to `user=lens_ws_7f3c database=talyvor_lens`: db-primary.internal:5432 " +
	"(10.0.3.7): dial error ref=req_01HZX9 key=sk-live-4f2a"

// halfDown is every partner rail, the even ones last failing and the odd ones last answering, so every rail field
// and rails_summary.down_names carry a value.
type halfDown struct{}

func (halfDown) Rails() []partners.Rail {
	ok, failed := time.Now().UTC().Add(-time.Minute), time.Now().UTC()
	out := []partners.Rail{}
	for i, s := range partners.Services {
		r := partners.Rail{Service: s, Mode: "test", LastSuccess: &ok, LastFailure: &failed}
		if i%2 == 1 {
			r.LastSuccess, r.LastFailure = &failed, &ok
		}
		out = append(out, r)
	}
	return out
}

// statusJSON is /status.json from a page whose PostgreSQL fails with plantedError, whose rails are halfDown, and whose
// sanctions lists are 52 hours old.
func statusJSON(t *testing.T) map[string]any {
	t.Helper()
	page := newTestPage(t, &fakePinger{err: errors.New(plantedError)})
	page.UseMoneyRails(halfDown{}, fxCleared{})
	page.UseScreeningLists(fakeLists{age: 52 * time.Hour, stale: true})
	page.UpdateCache(page.Check(context.Background()))
	rec := httptest.NewRecorder()
	page.ServeJSON(rec, httptest.NewRequest(http.MethodGet, "/status.json", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// walk records every object key's path, and every string value under its path.
func walk(path string, v any, keys map[string]bool, strs map[string][]string) {
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			p := k
			if path != "" {
				p = path + "." + k
			}
			keys[p] = true
			walk(p, child, keys, strs)
		}
	case []any:
		for _, child := range v {
			walk(path+"[]", child, keys, strs)
		}
	case string:
		strs[path] = append(strs[path], v)
	}
}

// B37.3: /status.json's keys, at every level, are exactly the documented set: none more, none missing.
func TestStatusJSON_KeysAreTheDocumentedSet(t *testing.T) {
	keys := map[string]bool{}
	walk("", statusJSON(t), keys, map[string][]string{})
	want := map[string]bool{}
	for _, k := range documentedKeys {
		want[k] = true
	}
	var extra, missing []string
	for k := range keys {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	for k := range want {
		if !keys[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	if len(extra) > 0 || len(missing) > 0 {
		t.Fatalf("/status.json keys differ from the documented set: undocumented %v, missing %v", extra, missing)
	}
}

// B37.3: every text /status.json holds is one the page chose — a status word, a fixed name, a fixed phrase or a time —
// so no reference id, key, error text, host name or anything about a workspace can reach it, whatever a dependency
// says when it fails.
func TestStatusJSON_NoValueCarriesASecret(t *testing.T) {
	strs := map[string][]string{}
	walk("", statusJSON(t), map[string]bool{}, strs)

	set := func(vs ...string) map[string]bool {
		m := map[string]bool{}
		for _, v := range vs {
			m[v] = true
		}
		return m
	}
	statuses := set(string(StatusOperational), string(StatusDegraded), string(StatusOutage), string(StatusUnknown))
	services, names, capabilities := set(), set(), set()
	for _, s := range partners.Services {
		services[string(s)] = true
	}
	for _, n := range railNames {
		names[n] = true
	}
	for _, keys := range railCapabilities {
		for _, k := range keys {
			capabilities[k] = true
		}
	}
	allowed := map[string]map[string]bool{
		"status": statuses, "components[].status": statuses, "providers[].status": statuses, "rails[].status": statuses,
		"version":           set("0.1.0"),
		"components[].name": set("PostgreSQL", "Redis", "NATS", "Proxy"),
		"components[].message": set("not configured", "not connected", "timed out", "cannot connect",
			"self-check: this page was served by the proxy"),
		"providers[].name":           set("OpenAI", "Anthropic", "Google Gemini", "AWS Bedrock"),
		"rails[].service":            services,
		"rails[].name":               names,
		"rails_summary.down_names[]": names,
		"rails[].mode":               set("test", "live"),
		"rails[].capabilities[].key": capabilities,
		"card_payments":              set("live", "test", "off"),
	}
	times := set("updated_at", "components[].checked_at", "providers[].checked_at", "rails[].last_success",
		"rails[].last_failure")

	for path, vs := range strs {
		for _, v := range vs {
			switch {
			case times[path]:
				if _, err := time.Parse(time.RFC3339Nano, v); err != nil {
					t.Errorf("%s = %q is not a time", path, v)
				}
			case allowed[path] == nil:
				t.Errorf("%s = %q: a text field the page never documented", path, v)
			case !allowed[path][v]:
				t.Errorf("%s = %q is not one of the page's own words", path, v)
			}
		}
	}
}

// B37.3: /status answers with every protection production showed on 9 Oct, plus Strict-Transport-Security, and with
// its own Content-Security-Policy: no script may run on it, and it needs none.
func TestStatus_SecurityHeaders(t *testing.T) {
	page := newTestPage(t, &fakePinger{})
	page.UpdateCache(page.Check(context.Background()))
	// shortcut: the middleware is composed here as cmd/lens/main.go's r.Use does; main.go's wiring itself is checked
	// only by a request to the running server.
	h := api.SecurityHeadersMiddleware(http.HandlerFunc(page.ServeHTTP))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))

	for name, want := range map[string]string{
		"Content-Security-Policy":   pageCSP,
		"X-Content-Type-Options":    "nosniff",
		"Referrer-Policy":           "strict-origin-when-cross-origin",
		"X-Frame-Options":           "DENY",
		"Strict-Transport-Security": "max-age=31536000",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("/status %s = %q, want %q", name, got, want)
		}
	}
	if strings.Contains(pageCSP, "script-src") || !strings.HasPrefix(pageCSP, "default-src 'none'") {
		t.Errorf("/status's policy lets a script run: %s", pageCSP)
	}
	if strings.Contains(rec.Body.String(), "<script") {
		t.Error("/status renders a script its policy blocks")
	}
}

// countingPinger counts the PostgreSQL probes Check makes.
type countingPinger struct{ n atomic.Int32 }

func (c *countingPinger) Ping(context.Context) error { c.n.Add(1); return nil }

// countingRails counts the rail reads Check makes.
type countingRails struct{ n atomic.Int32 }

func (c *countingRails) Rails() []partners.Rail { c.n.Add(1); return nil }

// B37.3: both routes answer from the 60-second cache; a request never runs Check, so polling the page never probes a
// dependency.
func TestStatus_ServedFromTheCache(t *testing.T) {
	pinger, rails := &countingPinger{}, &countingRails{}
	page := newStatusPage(pinger, nil, nil, "checked-live")
	page.UseMoneyRails(rails, nil)
	page.UpdateCache(StatusResponse{Status: StatusOperational, Version: "from-the-cache"})

	for _, tc := range []struct {
		path, accept string
		serve        http.HandlerFunc
	}{
		{"/status", "", page.ServeHTTP},
		{"/status", "application/json", page.ServeHTTP},
		{"/status.json", "", page.ServeJSON},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("Accept", tc.accept)
		rec := httptest.NewRecorder()
		tc.serve(rec, req)
		if !strings.Contains(rec.Body.String(), "from-the-cache") {
			t.Errorf("%s (Accept %q) did not answer from the cache", tc.path, tc.accept)
		}
	}
	if pinger.n.Load() != 0 || rails.n.Load() != 0 {
		t.Fatalf("serving the page ran Check: %d PostgreSQL probes, %d rail reads", pinger.n.Load(), rails.n.Load())
	}
}
