package status

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
)

// fxCleared is a clearance reader where only fx has a clearance in force.
type fxCleared struct{}

func (fxCleared) WalletCapabilities(context.Context) ([]economy.CapabilityStatus, error) {
	out := []economy.CapabilityStatus{}
	for _, c := range economy.Capabilities {
		st := economy.CapabilityStatus{Capability: c}
		if c.Key == economy.CapabilityFX {
			st.Clearance = &economy.Clearance{}
		}
		out = append(out, st)
	}
	return out, nil
}

// B30.12: the page lists every partner in its mode with its clearances, a Test partner whose last call failed shows
// as down, one whose last call succeeded as up, and one never called as neither.
func TestStatus_MoneyRails(t *testing.T) {
	ctx := context.Background()
	reg := partners.NewRegistry(nil)
	scr, err := reg.Screening(ctx, economy.CapabilityPaymentsOut)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scr.ScreenName(ctx, partners.NameScreen{Name: "TESTFAIL Ltd"}); err == nil {
		t.Fatal("the Test screening provider screened a TESTFAIL name")
	}
	acct, err := reg.Account(ctx, economy.CapabilityCurrencyAccounts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acct.OpenAccount(ctx, partners.AccountRequest{ID: "a1", Holder: "Acme", Currency: "GBP"}); err != nil {
		t.Fatal(err)
	}

	page := newStatusPage(&fakePinger{}, nil, nil, "test")
	page.UseMoneyRails(reg, fxCleared{})
	page.UpdateCache(page.Check(ctx))

	rec := httptest.NewRecorder()
	page.ServeJSON(rec, httptest.NewRequest(http.MethodGet, "/status.json", nil))
	var got StatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rails) != len(partners.Services) {
		t.Fatalf("%d rails, want one per partner service (%d)", len(got.Rails), len(partners.Services))
	}
	rails := map[partners.Service]MoneyRail{}
	for _, r := range got.Rails {
		if r.Mode != "test" || r.Name == "" {
			t.Fatalf("%s: mode %q name %q, want a named Test partner", r.Service, r.Mode, r.Name)
		}
		rails[r.Service] = r
	}
	if s := rails[partners.ServiceScreening]; s.Status != StatusOutage || s.LastFailure == nil || s.LastSuccess != nil {
		t.Fatalf("the failing Test screening provider reads %s (success %v, failure %v), want an outage", s.Status, s.LastSuccess, s.LastFailure)
	}
	if a := rails[partners.ServiceAccount]; a.Status != StatusOperational || a.LastSuccess == nil {
		t.Fatalf("the account partner reads %s after a good call, want operational", a.Status)
	}
	if f := rails[partners.ServiceFX]; f.Status != StatusUnknown || len(f.Capabilities) != 1 || f.Capabilities[0].Cleared == nil || !*f.Capabilities[0].Cleared {
		t.Fatalf("fx reads %s with %+v, want never called and its capability cleared", f.Status, f.Capabilities)
	}

	html := renderHTML(&got)
	for _, want := range []string{"Money rails",
		`Sanctions screening<div class="msg">Test partner · no clearance needed</div></td><td><span class="pill bad">outage</span>`,
		`Currency conversion<div class="msg">Test partner · cleared</div></td><td><span class="pill">no calls yet</span>`,
		`Accounts and payments<div class="msg">Test partner · not cleared</div></td><td><span class="pill good">operational</span>`} {
		if !strings.Contains(html, want) {
			t.Fatalf("the status page lacks %q", want)
		}
	}
	if strings.Contains(html, "TESTFAIL") {
		t.Fatal("the status page shows a partner's error text")
	}

	// B37.1: the banner stays Lens's own, and the line under it names the rail that is down.
	if got.Status != StatusOperational {
		t.Fatalf("a Test partner that is down moved the banner to %s", got.Status)
	}
	if s := got.RailsSummary; s.Down != 1 || s.Up != 1 || s.Idle != len(partners.Services)-2 || len(s.DownNames) != 1 || s.DownNames[0] != "Sanctions screening" {
		t.Fatalf("rails_summary %+v, want 1 down (Sanctions screening), 1 up, the rest idle", s)
	}
	if want := `<span class="pill bad">Money rails: 1 of 10 down: Sanctions screening (test partner)</span>`; !strings.Contains(html, want) {
		t.Fatalf("the status page lacks %q", want)
	}
}

// fakeRails is a partners registry whose rails are given.
type fakeRails []partners.Rail

func (f fakeRails) Rails() []partners.Rail { return f }

func TestStatus_RailsLine(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	up := fakeRails{}
	for _, s := range partners.Services {
		up = append(up, partners.Rail{Service: s, Mode: "test", LastSuccess: &now})
	}
	page := newStatusPage(&fakePinger{}, nil, nil, "test")
	page.UseMoneyRails(up, nil)
	got := page.Check(ctx)
	if got.RailsSummary.Up != len(partners.Services) || !strings.Contains(renderHTML(&got), "Money rails: all answering") {
		t.Fatalf("every rail up reads %+v, want all answering", got.RailsSummary)
	}

	// A live rail that is down is real money not moving, so it turns the banner degraded.
	live := append(fakeRails{}, up...)
	live[0] = partners.Rail{Service: partners.ServiceAccount, Mode: "live", LastFailure: &now}
	page.UseMoneyRails(live, nil)
	got = page.Check(ctx)
	if got.Status != StatusDegraded {
		t.Fatalf("a live rail that is down left the banner %s, want degraded", got.Status)
	}
	if !strings.Contains(renderHTML(&got), "Money rails: 1 of 10 down: Accounts and payments (live partner)") {
		t.Fatal("the status page does not name the live rail that is down")
	}
}

// B37.1: an unreachable PostgreSQL reads "cannot connect" on the public page, and its error, which names the database
// user, the database and the address, goes only to the log.
func TestStatus_PostgresErrorStaysInTheLog(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	secret := "failed to connect to `user=lens database=talyvor_lens`: 127.0.0.1:55433 (localhost): dial error"
	page := newStatusPage(&fakePinger{err: errors.New(secret)}, nil, nil, "test")
	for _, path := range []string{"/status", "/status.json"} {
		rec := httptest.NewRecorder()
		if path == "/status" {
			page.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		} else {
			page.ServeJSON(rec, httptest.NewRequest(http.MethodGet, path, nil))
		}
		body := rec.Body.String()
		if !strings.Contains(body, "cannot connect") {
			t.Fatalf("%s does not read cannot connect", path)
		}
		for _, leak := range []string{"user=", "talyvor_lens", "127.0.0.1", "55433", "dial error"} {
			if strings.Contains(body, leak) {
				t.Fatalf("%s shows %q from the database error", path, leak)
			}
		}
	}
	if !strings.Contains(logs.String(), "component=PostgreSQL") || !strings.Contains(logs.String(), "127.0.0.1:55433") {
		t.Fatalf("the log lacks the PostgreSQL error: %s", logs.String())
	}
}

// fakeLists are sanctions lists of a given age.
type fakeLists struct {
	age   time.Duration
	stale bool
}

func (f fakeLists) ListsAge(context.Context) (time.Duration, bool, error) { return f.age, f.stale, nil }

// B37.4: lists older than LENS_SCREENING_MAX_AGE_HOURS turn the Sanctions screening rail down, saying how old they are,
// and /status.json's screening rail carries lists_age_hours.
func TestStatus_StaleSanctionsListsTurnTheScreeningRailDown(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	up := fakeRails{}
	for _, s := range partners.Services {
		up = append(up, partners.Rail{Service: s, Mode: "test", LastSuccess: &now})
	}
	page := newStatusPage(&fakePinger{}, nil, nil, "test")
	page.UseMoneyRails(up, nil)
	page.UseScreeningLists(fakeLists{age: 52*time.Hour + 20*time.Minute, stale: true})
	page.UpdateCache(page.Check(ctx))

	rec := httptest.NewRecorder()
	page.ServeJSON(rec, httptest.NewRequest(http.MethodGet, "/status.json", nil))
	var got struct {
		Rails []map[string]any `json:"rails"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, r := range got.Rails {
		age, has := r["lists_age_hours"]
		switch {
		case r["service"] == string(partners.ServiceScreening) && (age != float64(52) || r["status"] != string(StatusOutage)):
			t.Fatalf("the screening rail with lists 52 h old: %v", r)
		case r["service"] != string(partners.ServiceScreening) && has:
			t.Fatalf("the %v rail carries lists_age_hours", r["service"])
		}
	}
	rec = httptest.NewRecorder()
	page.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if body := rec.Body.String(); !strings.Contains(body, `lists <span class="num">52 h</span> old`) ||
		!strings.Contains(body, "Money rails: 1 of 10 down: Sanctions screening (test partner, lists 52 h old)") {
		t.Fatal("the status page does not show the screening rail down with its lists' age")
	}

	// Fresh lists leave the rail up, and still say how old they are.
	page.UseScreeningLists(fakeLists{age: 3 * time.Hour})
	fresh := page.Check(ctx)
	for _, r := range fresh.Rails {
		if r.Service == partners.ServiceScreening && (r.Status != StatusOperational || r.ListsAgeHours == nil || *r.ListsAgeHours != 3) {
			t.Fatalf("the screening rail with lists 3 h old: %+v", r)
		}
	}
}
