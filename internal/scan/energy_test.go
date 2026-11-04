package scan_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/scan"
)

// sources serves the recorded fixtures as each source's API would, and keeps every request it answered. The ENTSOG
// and ECMWF fixtures were recorded from the live APIs; GIE and EIA need an API key to record, so theirs are written
// to the shape of the responses their API documentation publishes.
type sources struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
	sums map[string]string // fixture → hex SHA-256
}

func newSources(t *testing.T) *sources {
	t.Helper()
	files := map[string][]byte{}
	for _, f := range []string{"gie-agsi.json", "gie-alsi.json", "entsog-2025-10-01.json", "eia-wstk.json",
		"ecmwf-20261103-00z-24h.index", "ecmwf-20261103-00z-24h-2t.grib2"} {
		b, err := os.ReadFile("testdata/" + f)
		if err != nil {
			t.Fatal(err)
		}
		files[f] = b
	}
	s := &sources{sums: map[string]string{}}
	for f, b := range files {
		sum := sha256.Sum256(b)
		s.sums[f] = hex.EncodeToString(sum[:])
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs = append(s.reqs, r)
		s.mu.Unlock()
		p := r.URL.Path
		var f string
		switch {
		case p == "/agsi":
			f = "gie-agsi.json"
		case p == "/alsi":
			f = "gie-alsi.json"
		case p == "/entsog/operationalData":
			f = "entsog-2025-10-01.json"
		case strings.HasPrefix(p, "/eia/v2/petroleum/stoc/wstk/data"):
			f = "eia-wstk.json"
		case strings.HasSuffix(p, "-24h-oper-fc.index"):
			f = "ecmwf-20261103-00z-24h.index"
		case strings.HasSuffix(p, "-24h-oper-fc.grib2") && r.Header.Get("Range") == "bytes=26297816-26617386":
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(files["ecmwf-20261103-00z-24h-2t.grib2"])
			return
		default:
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(files[f])
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *sources) collectors(gieKey, eiaKey string) []scan.Collector {
	return []scan.Collector{
		&scan.AGSI{Key: gieKey, Base: s.URL + "/agsi"}, &scan.ALSI{Key: gieKey, Base: s.URL + "/alsi"},
		&scan.ENTSOG{Base: s.URL + "/entsog"}, &scan.EIA{Key: eiaKey, Base: s.URL + "/eia"}, &scan.ECMWF{Base: s.URL + "/ecmwf"},
	}
}

func (s *sources) first(path string) *http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.reqs {
		if strings.HasPrefix(r.URL.Path, path) {
			return r
		}
	}
	return nil
}

func TestEnergyCollectorsReadTheirFixtures(t *testing.T) {
	src := newSources(t)
	now := time.Now().UTC()
	since := now.Add(-scan.Backfill)
	got := map[string][]scan.Observation{}
	for _, c := range src.collectors("gie-key", "eia-key") {
		err := c.Collect(context.Background(), scan.NewFetcher(src.Client(), 60000), since, now, func(o scan.Observation) error {
			got[c.Source()] = append(got[c.Source()], o)
			return nil
		})
		if err != nil {
			t.Fatalf("%s: %v", c.Source(), err)
		}
	}
	find := func(source, series string, at time.Time) scan.Observation {
		for _, o := range got[source] {
			if o.Series == series && o.ObservedAt.Equal(at) {
				return o
			}
		}
		t.Fatalf("%s: no %s at %s among %d points", source, series, at, len(got[source]))
		return scan.Observation{}
	}
	date := func(s string) time.Time { d, _ := time.Parse(time.RFC3339, s); return d }
	for _, c := range []struct {
		source, series, at, unit, fixture string
		value                             float64
	}{
		{"gie-agsi", "gie.agsi.eu.gas_in_storage", "2026-10-30T00:00:00Z", "TWh", "gie-agsi.json", 1043.5678},
		{"gie-agsi", "gie.agsi.de.full", "2026-10-29T00:00:00Z", "%", "gie-agsi.json", 92.17},
		{"gie-alsi", "gie.alsi.eu.inventory", "2026-10-30T00:00:00Z", "GWh", "gie-alsi.json", 29876.5},
		{"entsog", "entsog.flow.norway-emden", "2025-10-01T04:00:00Z", "kWh/d", "entsog-2025-10-01.json", 322956962},
		{"entsog", "entsog.flow.turkstream-strandzha", "2025-10-02T04:00:00Z", "kWh/d", "entsog-2025-10-01.json", 564655263},
		{"eia", "eia.stocks.cushing-crude", "2026-10-30T00:00:00Z", "thousand barrels", "eia-wstk.json", 22412},
		{"eia", "eia.stocks.us-crude-ex-spr", "2026-10-30T00:00:00Z", "thousand barrels", "eia-wstk.json", 416789},
	} {
		o := find(c.source, c.series, date(c.at))
		if o.Value != c.value || o.Unit != c.unit || o.ContentHash != src.sums[c.fixture] || o.RetrievedAt.IsZero() || len(o.Transforms) == 0 {
			t.Errorf("%s %s: %+v; want %v %s from content %s", c.source, c.series, o, c.value, c.unit, src.sums[c.fixture])
		}
	}
	// A "-" in GIE's data is no figure, never a zero.
	for _, o := range got["gie-agsi"] {
		if o.Series == "gie.agsi.eu.withdrawal" && o.ObservedAt.Equal(date("2026-10-29T00:00:00Z")) {
			t.Errorf("a withdrawal of \"-\" was stored as %v", o.Value)
		}
	}

	// ECMWF keeps four days of runs; each served the recorded 2 m temperature, Berlin's grid point read 277.849 K.
	runs := 0
	for _, o := range got["ecmwf-open"] {
		if o.Series == "ecmwf.t2m.berlin.24h" {
			runs++
			if math.Abs(o.Value-(277.84910583496094-273.15)) > 1e-9 || o.Unit != "°C" ||
				o.ContentHash != src.sums["ecmwf-20261103-00z-24h-2t.grib2"] || len(o.Transforms) != 3 {
				t.Errorf("ecmwf berlin: %+v", o)
			}
		}
	}
	if runs != 5 {
		t.Errorf("ecmwf: %d runs of Berlin's 24h forecast, want the 5 days ECMWF keeps (today and four before)", runs)
	}

	// Five years are backfilled where the source keeps them, and each key goes where its API expects it.
	from := since.Format("2006-01-02")
	if r := src.first("/agsi"); r == nil || r.URL.Query().Get("from") != from || r.Header.Get("x-key") != "gie-key" {
		t.Errorf("AGSI's first request: %v", r)
	}
	if r := src.first("/entsog"); r == nil || r.URL.Query().Get("from") != from {
		t.Errorf("ENTSOG's first request: %v", r)
	}
	if r := src.first("/eia"); r == nil || r.URL.Query().Get("start") != from || r.URL.Query().Get("api_key") != "eia-key" {
		t.Errorf("EIA's first request: %v", r)
	}
}

// An error never carries the request's URL, which can hold an API key, and a redirect off the source's host is refused
// so the key never follows it.
func TestFetcherKeepsTheKey(t *testing.T) {
	f := scan.NewFetcher(&http.Client{Timeout: time.Second}, 60000)
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:1/v2/data?api_key=secret-key", nil)
	if _, _, _, err := f.Get(context.Background(), req); err == nil || strings.Contains(err.Error(), "secret-key") {
		t.Errorf("error: %v", err)
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the redirect was followed to another host, x-key %q", r.Header.Get("x-key"))
	}))
	defer other.Close()
	source := httptest.NewServer(http.RedirectHandler(strings.Replace(other.URL, "127.0.0.1", "localhost", 1), http.StatusFound))
	defer source.Close()
	req, _ = http.NewRequest(http.MethodGet, source.URL, nil)
	req.Header.Set("x-key", "secret-key")
	if _, _, _, err := f.Get(context.Background(), req); err == nil {
		t.Error("a redirect to another host was followed")
	}
}
