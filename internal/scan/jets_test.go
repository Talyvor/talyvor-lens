package scan_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/scan"
)

// The registry and flights are synthetic, in the shapes of the FAA's MASTER.txt and AeroAPI's /flights/{ident}.
var jetCompanies = []string{"Example Industries Inc", "OTHER EXAMPLE LLC", "SAMPLE PERSON", "EXAMPLE BANK NA TRUSTEE", "REGISTRANT WITHHELD"}

func TestCompanyJetsLeavesOutEveryoneButCompanies(t *testing.T) {
	master, err := os.ReadFile("testdata/faa-master-jets.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, err := scan.CompanyJets(master, jetCompanies, map[string]bool{"N108EX": true})
	if err != nil {
		t.Fatal(err)
	}
	// Left out: N103EX an individual, N104EX deregistered, N105EX a trustee, N106EX a withheld name, N107EX not a jet,
	// N108EX excluded (used by one person), N110EX a partnership.
	want := map[string][]string{"EXAMPLE INDUSTRIES INC": {"N101EX", "N102EX"}, "OTHER EXAMPLE LLC": {"N109EX"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("company jets = %v, want %v", got, want)
	}
}

type jetSource struct {
	*httptest.Server
	mu        sync.Mutex
	requested []string
}

func newJetSource(t *testing.T) *jetSource {
	t.Helper()
	master, err := os.ReadFile("testdata/faa-master-jets.txt")
	if err != nil {
		t.Fatal(err)
	}
	var z bytes.Buffer
	zw := zip.NewWriter(&z)
	w, _ := zw.Create("MASTER.txt")
	_, _ = w.Write(master)
	_ = zw.Close()
	s := &jetSource{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ReleasableAircraft.zip" {
			_, _ = w.Write(z.Bytes())
			return
		}
		reg := strings.TrimPrefix(r.URL.Path, "/aeroapi/flights/")
		if r.Header.Get("x-apikey") != "fa-key" || r.URL.Query().Get("ident_type") != "registration" {
			http.Error(w, "unauthorised", http.StatusUnauthorized)
			return
		}
		s.mu.Lock()
		s.requested = append(s.requested, reg)
		s.mu.Unlock()
		b, err := os.ReadFile("testdata/aeroapi-flights-" + reg + ".json")
		if err != nil {
			_, _ = w.Write([]byte(`{"links":null,"num_pages":1,"flights":[]}`))
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *jetSource) jets(store *scan.Store, on bool) *scan.Jets {
	return &scan.Jets{On: on, Key: "fa-key", Companies: jetCompanies, Store: store,
		Registry: s.URL + "/ReleasableAircraft.zip", Base: s.URL + "/aeroapi"}
}

func (s *jetSource) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.requested
	s.requested = nil
	return out
}

// jetsNow: the newest complete UTC day that ended at least 24 hours before is 2 Nov.
var jetsNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func jetsStore(t *testing.T) (*scan.Store, *pgxpool.Pool) {
	s, pool := testStore(t)
	ctx := context.Background()
	clear := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM scan_points WHERE series LIKE 'jets.%'`)
		_, _ = pool.Exec(ctx, `DELETE FROM scan_jet_exclusions WHERE registration LIKE 'N1__EX'`)
		_, _ = pool.Exec(ctx, `DELETE FROM scan_jet_aircraft WHERE series LIKE 'jets.%'`)
		_, _ = pool.Exec(ctx, `UPDATE scan_sources SET enabled = FALSE WHERE id = 'flightaware-standard'`)
	}
	clear()
	t.Cleanup(clear)
	if _, err := s.ExcludeAircraft(ctx, "N108EX", "single-user"); err != nil {
		t.Fatal(err)
	}
	return s, pool
}

func collectJets(t *testing.T, s *scan.Store, src *jetSource) map[string]float64 {
	t.Helper()
	ctx := context.Background()
	if err := s.SetEnabled(ctx, "flightaware-standard", true); err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	err := src.jets(s, true).Collect(ctx, scan.NewFetcher(src.Client(), 60000), time.Time{}, jetsNow, func(o scan.Observation) error {
		o.SourceID = "flightaware-standard"
		if _, err := s.Record(ctx, o); err != nil {
			return err
		}
		got[o.Series+" "+o.ObservedAt.Format("01-02")] = o.Value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestJetsStayOffBehindTheirSwitch(t *testing.T) {
	s, pool := testStore(t)
	src := newJetSource(t)
	ctx := context.Background()
	// The switch off, with the key and companies set: nothing fetched, nothing stored, and /status says why.
	c := scan.NewCollectors(s, src.Client(), map[string]string{}, src.jets(s, false))
	c.Run(ctx)
	if asked := src.asked(); len(asked) != 0 {
		t.Fatalf("the switched-off beta asked AeroAPI for %v", asked)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM scan_points WHERE series LIKE 'jets.%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("stored %d jets figures (%v)", n, err)
	}
	st := c.Status(ctx)
	if len(st) != 1 || st[0].On || !strings.Contains(st[0].Note, scan.SettingJets) {
		t.Fatalf("status = %+v", st)
	}
	// Even handed straight to Collect, a switched-off beta reads nothing.
	if err := src.jets(s, false).Collect(ctx, scan.NewFetcher(src.Client(), 60000), time.Time{}, jetsNow,
		func(scan.Observation) error { t.Fatal("put called"); return nil }); err != nil || len(src.asked()) != 0 {
		t.Fatalf("switched-off Collect: %v", err)
	}
}

func TestJetsCountPerCompanyAndDayAtLeastADayLate(t *testing.T) {
	s, _ := jetsStore(t)
	src := newJetSource(t)
	got := collectJets(t, s, src)

	// Only the companies' own jets were asked about: never the one used by one person, a trust's or a person's.
	if asked := src.asked(); !reflect.DeepEqual(asked, []string{"N101EX", "N102EX", "N109EX"}) {
		t.Fatalf("asked AeroAPI for %v", asked)
	}
	want := map[string]float64{
		// 1 Nov: N101EX and N102EX each landed once at KPBI. N101EX's blocked (LADD) flight and the flight with no
		// registration (a PIA address) are not counted.
		"jets.example-industries-inc.flights 11-01":       2,
		"jets.example-industries-inc.arrivals.kpbi 11-01": 2,
		"jets.example-industries-inc.flights 11-02":       1,
		"jets.example-industries-inc.arrivals.ksfo 11-02": 1,
		"jets.other-example-llc.flights 11-02":            1,
		"jets.other-example-llc.arrivals.kdal 11-02":      1,
		"jets.example-industries-inc.flights 10-24":       0, // a day without flights is a figure too
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %v (present %v), want %v", k, g, ok, v)
		}
	}
	for k := range got {
		// 3 Nov ended less than 24 hours before: N101EX's flight that day is not counted yet.
		if strings.HasSuffix(k, " 11-03") || strings.HasSuffix(k, " 11-04") {
			t.Errorf("%s is less than a day old", k)
		}
		// Every figure is a company's day: no aircraft is named.
		if strings.Contains(strings.ToUpper(k), "N10") {
			t.Errorf("%s names an aircraft", k)
		}
	}
	if len(got) != 2*10+3 { // two companies × ten days of flights, and three airport-days
		t.Errorf("%d figures: %v", len(got), got)
	}
}

func TestExcludingAnAircraftDeletesEveryFigureItWasIn(t *testing.T) {
	s, pool := jetsStore(t)
	src := newJetSource(t)
	ctx := context.Background()
	collectJets(t, s, src)
	src.asked()

	if _, err := s.ExcludeAircraft(ctx, "N102EX", "flown-too-much"); !errors.Is(err, scan.ErrExclusionReason) {
		t.Fatalf("an unknown reason: %v", err)
	}
	// An erasure request for N102EX: its four figures (1 Nov's two, 2 Nov's two) go; nothing else does.
	n, err := s.ExcludeAircraft(ctx, "n102ex", "erasure")
	if err != nil || n != 4 {
		t.Fatalf("erasure deleted %d figures (%v), want 4", n, err)
	}
	got := collectJets(t, s, src)
	if asked := src.asked(); !reflect.DeepEqual(asked, []string{"N101EX", "N109EX"}) {
		t.Fatalf("after the erasure, asked AeroAPI for %v", asked)
	}
	for k, v := range map[string]float64{
		"jets.example-industries-inc.flights 11-01":       1,
		"jets.example-industries-inc.arrivals.kpbi 11-01": 1,
		"jets.example-industries-inc.flights 11-02":       0,
	} {
		if got[k] != v {
			t.Errorf("after the erasure %s = %v, want %v", k, got[k], v)
		}
	}
	if _, ok := got["jets.example-industries-inc.arrivals.ksfo 11-02"]; ok {
		t.Error("N102EX's 2 Nov arrival is still counted")
	}
	// What is stored, not only what was read: the recount replaced the erased figures.
	var stored float64
	if err := pool.QueryRow(ctx, `SELECT value FROM scan_points WHERE series = 'jets.example-industries-inc.flights'
		AND observed_at = '2026-11-01T00:00:00Z'`).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("stored 1 Nov flights = %v (%v), want 1", stored, err)
	}
}

func TestAnExclusionMadeWhileCollectingStillRemovesTheFigure(t *testing.T) {
	s, pool := jetsStore(t)
	src := newJetSource(t)
	ctx := context.Background()
	if err := s.SetEnabled(ctx, "flightaware-standard", true); err != nil {
		t.Fatal(err)
	}
	// N101EX is erased after the run read the exclusions and before the 1 Nov figure it is in is stored.
	err := src.jets(s, true).Collect(ctx, scan.NewFetcher(src.Client(), 60000), time.Time{}, jetsNow, func(o scan.Observation) error {
		if o.Series == "jets.example-industries-inc.flights" && o.ObservedAt.Day() == 1 {
			if _, err := s.ExcludeAircraft(ctx, "N101EX", "erasure"); err != nil {
				return err
			}
		}
		o.SourceID = "flightaware-standard"
		_, err := s.Record(ctx, o)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM scan_points WHERE series = 'jets.example-industries-inc.flights'
		AND observed_at = '2026-11-01T00:00:00Z'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the 1 Nov figure counting the erased aircraft is still stored (%d, %v)", n, err)
	}
}
