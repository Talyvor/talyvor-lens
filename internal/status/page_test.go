package status

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/brand"

	"github.com/alicebob/miniredis/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
)

// fakePinger is a stand-in for *pgxpool.Pool. It returns the configured
// error after sleeping for the configured delay so we can simulate
// "Postgres down" without needing pgxmock's ping support.
type fakePinger struct {
	err   error
	delay time.Duration
}

func (f *fakePinger) Ping(ctx context.Context) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

func runEmbeddedNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := &natsserver.Options{
		Host:     "127.0.0.1",
		Port:     -1,
		StoreDir: t.TempDir(),
		NoLog:    true,
		NoSigs:   true,
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("natsserver.NewServer: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		srv.Shutdown()
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(func() {
		nc.Close()
		srv.Shutdown()
		srv.WaitForShutdown()
	})
	return nc
}

// happyProviderServer returns 200 on HEAD so the provider check
// classifies as operational. Returned URL is shared by all four
// provider fields to keep the test fixture small.
func happyProviderServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestPage(t *testing.T, pinger pgxPinger) *StatusPage {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	nc := runEmbeddedNATS(t)
	srv := happyProviderServer(t)

	sp := newStatusPage(pinger, rc, nc, "0.1.0")
	sp.openaiURL = srv.URL
	sp.anthropicURL = srv.URL
	sp.googleURL = srv.URL
	sp.bedrockURL = srv.URL
	return sp
}

func TestCheck_ReturnsOperationalWhenAllHealthy(t *testing.T) {
	sp := newTestPage(t, &fakePinger{})
	resp := sp.Check(context.Background())
	if resp.Status != StatusOperational {
		t.Errorf("overall = %q, want operational; components=%+v providers=%+v", resp.Status, resp.Components, resp.Providers)
	}
	for _, c := range resp.Components {
		if c.Status == StatusOutage {
			t.Errorf("component %s reported outage: %+v", c.Name, c)
		}
	}
}

func TestCheck_ReturnsOutageWhenPostgresDown(t *testing.T) {
	sp := newTestPage(t, &fakePinger{err: errors.New("connection refused")})
	resp := sp.Check(context.Background())
	if resp.Status != StatusOutage {
		t.Errorf("overall = %q, want outage", resp.Status)
	}
	var pg *Component
	for i := range resp.Components {
		if resp.Components[i].Name == "PostgreSQL" {
			pg = &resp.Components[i]
		}
	}
	if pg == nil || pg.Status != StatusOutage {
		t.Errorf("PostgreSQL component should be outage; got %+v", pg)
	}
}

// TestClassifyLatency_ThresholdsMatchSpec is REPLACED by thresholds_test.go.
//
// It asserted the single shared band — <=100ms operational, <=500ms degraded,
// >500ms outage — including the two cases that turned out to be the defect:
// `{150, nil, StatusDegraded}` annotated "slow Redis lands here" (150ms is not a
// slow Redis, it is a healthy Google Gemini), and `{501, nil, StatusOutage}`
// (501ms is not an outage, it is AWS Bedrock on a normal day — measured p90 465ms).
//
// The spec the name appealed to was written for same-host components and then
// applied to transatlantic ones, so a test that faithfully encoded it kept the
// miscalibration pinned in place. What replaces it asserts the two bands
// separately, against MEASURED provider latencies rather than against the spec.

func TestClassifyLatency_LocalAndProviderBandsAreSeparate(t *testing.T) {
	// The guarantee the old test was really protecting: an error is always an
	// outage, and a healthy reading is always operational — on both bands.
	if got := classifyLocalLatency(0, errors.New("boom")); got != StatusOutage {
		t.Errorf("local error = %q, want outage", got)
	}
	if got := classifyProviderLatency(0, errors.New("boom")); got != StatusOutage {
		t.Errorf("provider error = %q, want outage", got)
	}
	// And the bands are genuinely different, or separating them bought nothing.
	if localHealthyMs >= providerHealthyMs {
		t.Fatalf("localHealthyMs (%d) must be well below providerHealthyMs (%d)",
			localHealthyMs, providerHealthyMs)
	}
}

// TestComputeOverall_IsWorstOfOurOwnComponents. This replaces
// TestComputeOverall_IsWorstOfAllComponents, whose final case asserted
// "provider outage still raises overall to outage".
//
// That is the behaviour being corrected, not a regression: the banner answers
// "is Talyvor working?", and on the live box it read DEGRADED — with every Lens
// component operational — because OpenAI answered a HEAD in 257ms. The provider
// rows still carry each provider's real status, which is the useful information;
// see TestOverall_ThirdPartyProviderDoesNotDefineOurStatus in thresholds_test.go
// for the full argument.
func TestComputeOverall_IsWorstOfOurOwnComponents(t *testing.T) {
	allOK := []ComponentStatus{StatusOperational, StatusOperational, StatusOperational}
	if got := computeOverall(allOK, nil); got != StatusOperational {
		t.Errorf("all-ok overall = %q, want operational", got)
	}
	oneDeg := []ComponentStatus{StatusOperational, StatusDegraded, StatusOperational}
	if got := computeOverall(oneDeg, nil); got != StatusDegraded {
		t.Errorf("one-degraded overall = %q, want degraded", got)
	}
	oneOut := []ComponentStatus{StatusOperational, StatusOutage, StatusOperational}
	if got := computeOverall(oneOut, nil); got != StatusOutage {
		t.Errorf("one-outage overall = %q, want outage", got)
	}
	// A provider outage no longer raises OUR status — it is reported on its own row.
	if got := computeOverall(allOK, []ComponentStatus{StatusOutage}); got != StatusOperational {
		t.Errorf("provider-outage overall = %q, want operational (theirs, not ours)", got)
	}
}

func TestServeHTTP_ReturnsHTMLByDefault(t *testing.T) {
	sp := newTestPage(t, &fakePinger{})
	sp.UpdateCache(sp.Check(context.Background()))

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	sp.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "TALYVOR LENS STATUS") {
		t.Errorf("HTML missing brand string")
	}
}

// TestServeHTTP_RendersTheBrand (B57.14): /status wears Pocket — ink, cloud and
// tangerine with Pocket's status colours, light and dark, Bricolage Grotesque
// and Figtree named over the system stack with no font fetched, and the mark
// beside the name — and none of the colours it wore before.
func TestServeHTTP_RendersTheBrand(t *testing.T) {
	sp := newTestPage(t, &fakePinger{})
	sp.UpdateCache(sp.Check(context.Background()))
	w := httptest.NewRecorder()
	sp.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/status", nil))
	body := w.Body.String()

	for _, want := range []string{
		"#111216", "#F3F4F6", "#FF7A1A", // light: ink, cloud, tangerine
		"#0F1013", "#181A1F", "#F2F3F5", // dark: page, cloud, ink
		"#0B7A45", "#A84100", "#C0262D", // success, caution, danger
		"prefers-color-scheme: dark",
		`"Bricolage Grotesque"`, `"Figtree"`,
		`id="tslf-top"`, `id="tsdf-top"`, `class="tv-mark"`, // the chosen symbol, light and dark (B57.81)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/status is missing the brand's %s", want)
		}
	}
	// The symbol's own file carries the logo kit's colours (B57.81); everything else on the page is Pocket's.
	body = strings.Replace(body, brand.Mark, "", 1)
	for _, old := range []string{"#060A12", "#0F7A6C", `"Space Grotesk"`, `"IBM Plex Mono"`, "#f0a030", "#0c0e12", "#13161c", "#d4d8e2", "#8892a4", "#5ac17d", "#e35d6a", "@font-face", "googleapis"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(old)) {
			t.Errorf("/status still carries %s", old)
		}
	}
}

func TestServeHTTP_ReturnsJSONWhenAcceptIsApplicationJSON(t *testing.T) {
	sp := newTestPage(t, &fakePinger{})
	sp.UpdateCache(sp.Check(context.Background()))

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	sp.ServeHTTP(w, req)

	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type = %q, want application/json", w.Header().Get("Content-Type"))
	}
	var got StatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Errorf("body is not valid JSON: %v", err)
	}
}

func TestServeJSON_AlwaysReturnsJSONRegardlessOfAccept(t *testing.T) {
	sp := newTestPage(t, &fakePinger{})
	sp.UpdateCache(sp.Check(context.Background()))

	req := httptest.NewRequest(http.MethodGet, "/status.json", nil)
	req.Header.Set("Accept", "text/html") // try to confuse it
	w := httptest.NewRecorder()
	sp.ServeJSON(w, req)

	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type = %q, want application/json", w.Header().Get("Content-Type"))
	}
	var got StatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Errorf("body is not valid JSON: %v", err)
	}
}

func TestUptimeHours_IncreasesOverTime(t *testing.T) {
	sp := newTestPage(t, &fakePinger{})
	sp.startedAt = time.Now().Add(-90 * time.Minute) // booted 1.5h ago

	resp := sp.Check(context.Background())
	if resp.UptimeHours < 1.49 || resp.UptimeHours > 1.51 {
		t.Errorf("UptimeHours = %v, want ~1.50", resp.UptimeHours)
	}
	// Two-decimal-place precision asserted by the *.format* used in
	// the implementation — the test just checks the value isn't rounded
	// off to a whole number.
	if float64(int(resp.UptimeHours)) == resp.UptimeHours {
		t.Errorf("UptimeHours = %v, expected fractional precision", resp.UptimeHours)
	}
}

func TestStartCacher_UpdatesCachedResult(t *testing.T) {
	sp := newTestPage(t, &fakePinger{})
	// No cache yet.
	if sp.snapshot() != nil {
		t.Fatal("precondition: no cached snapshot")
	}

	// Drive one cycle synchronously.
	sp.runCheckOnce(context.Background())

	snap := sp.snapshot()
	if snap == nil {
		t.Fatal("snapshot is nil after runCheckOnce")
	}
	if snap.UpdatedAt.IsZero() {
		t.Errorf("UpdatedAt is zero — cache update didn't stamp the time")
	}
}
