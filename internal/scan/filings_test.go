package scan_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/scan"
)

// The EDGAR fixtures were recorded from sec.gov: UnitedHealth's Form 4s of May 2025, when its CEO, CFO and three
// directors bought shares on the open market within three days (and the CEO's gift of 22 May, which is no buy), one
// 13F-HR, and the two days' form indexes cut to those filings' lines and a few others.
type edgarRequest struct {
	path, userAgent string
	at              time.Time
}

func edgarServer(t *testing.T) (*httptest.Server, func() []edgarRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []edgarRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, edgarRequest{r.URL.Path, r.Header.Get("User-Agent"), time.Now()})
		mu.Unlock()
		b, err := os.ReadFile("testdata/edgar/" + path.Base(r.URL.Path))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []edgarRequest { mu.Lock(); defer mu.Unlock(); return append([]edgarRequest(nil), reqs...) }
}

func TestRecordedInsiderClusterIsFound(t *testing.T) {
	var buys []scan.InsiderBuy
	for i, acc := range []string{"140", "141", "142", "145", "146", "152"} {
		b, err := os.ReadFile("testdata/edgar/0000731766-25-000" + acc + ".txt")
		if err != nil {
			t.Fatal(err)
		}
		points, err := scan.Form4(b)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range points {
			if f := strings.Split(p.Series, "."); f[4] == "P" {
				buys = append(buys, scan.InsiderBuy{Point: int64(i), Issuer: f[2], Owner: f[3], At: p.ObservedAt})
			}
		}
	}
	got := scan.FindClusters(buys)
	if len(got) != 1 || got[0].Issuer != "731766" || got[0].Insiders != 5 || len(got[0].Buys) != 5 ||
		day(got[0].First) != "2025-05-14" || day(got[0].Last) != "2025-05-16" {
		t.Fatalf("clusters: %+v; want UnitedHealth's five insiders from 14 to 16 May 2025", got)
	}
	// Two insiders are not a cluster, nor are three whose buys span more than the window.
	if c := scan.FindClusters(buys[:2]); len(c) != 0 {
		t.Errorf("two insiders made a cluster: %+v", c)
	}
	spread := append([]scan.InsiderBuy(nil), buys[:3]...)
	spread[2].At = spread[0].At.Add(scan.ClusterWindow + 24*time.Hour)
	if c := scan.FindClusters(spread); len(c) != 0 {
		t.Errorf("buys %v apart made a cluster: %+v", scan.ClusterWindow+24*time.Hour, c)
	}

	b, _ := os.ReadFile("testdata/edgar/0001085146-25-003362.txt")
	holdings, err := scan.Form13F(b, "0001726609")
	if err != nil {
		t.Fatal(err)
	}
	var alphabet *scan.Observation
	for i := range holdings {
		if holdings[i].Series == "sec.13f.1726609.02079K305" {
			alphabet = &holdings[i]
		}
	}
	if len(holdings) == 0 || alphabet == nil || alphabet.Value != 4283 || day(alphabet.ObservedAt) != "2025-03-31" {
		t.Errorf("13F: %d holdings, Alphabet class A %+v; want 4283 shares at 31 Mar 2025", len(holdings), alphabet)
	}
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// The name search returns Lockheed Martin and its subsidiary Sikorsky; only the UEI checked to be Lockheed Martin is
// matched, and with no checked UEI nothing is, however exactly its name matches.
func TestContractsAreMatchedOnlyWhenCertain(t *testing.T) {
	fixture, err := os.ReadFile("testdata/usaspending-lockheed-2025-05-15.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/search/spending_by_transaction/" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()
	collect := func(listed []scan.Listed) map[string]float64 {
		got := map[string]float64{}
		err := (&scan.USAspending{Base: srv.URL, Listed: listed}).Collect(context.Background(), scan.NewFetcher(srv.Client(), 60000),
			time.Time{}, time.Date(2025, 5, 20, 0, 0, 0, 0, time.UTC), func(o scan.Observation) error {
				if o.ContentHash == "" || o.RetrievedAt.IsZero() || len(o.Transforms) != 2 || o.Unit != "USD" {
					t.Errorf("point without its provenance: %+v", o)
				}
				got[o.Series] += o.Value
				return nil
			})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := collect(scan.ListedRecipients)
	if got["usaspending.contracts.listed.936468.CONT_AWD_SPE4A125P1540_9700_-NONE-_-NONE-"] != 79910.4 {
		t.Errorf("Lockheed Martin's own award was not matched: %v", got)
	}
	if got["usaspending.contracts.unmatched.UTJWTSLMFNG4.CONT_AWD_N0038325FX935_9700_N0038320GX901_9700"] != 171091.18 {
		t.Errorf("Sikorsky's award should stay unmatched: %v", got)
	}
	for series := range collect([]scan.Listed{{Search: "LOCKHEED MARTIN", Ticker: "LMT", CIK: "936468"}}) {
		if !strings.HasPrefix(series, "usaspending.contracts.unmatched.") {
			t.Errorf("matched on its name alone: %s", series)
		}
	}
}

// Through the store: the collector reads two days of filings, stores every point with its provenance, and records the
// cluster as a derived figure whose inputs are the five buys; every request names Talyvor and is paced at the register's
// limit; a re-run records nothing new.
func TestEDGARCollectorRecordsTheClusterWithItsBuys(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	clear := func() {
		for _, q := range []string{
			`DELETE FROM scan_point_inputs WHERE point_id IN (SELECT id FROM scan_points WHERE series LIKE 'sec.%')`,
			`DELETE FROM scan_points WHERE series LIKE 'sec.%'`} {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
	}
	clear()
	t.Cleanup(func() { clear(); _ = s.SetEnabled(ctx, "sec-edgar", false) })
	if err := s.SetEnabled(ctx, "sec-edgar", true); err != nil {
		t.Fatal(err)
	}
	src, err := s.Source(ctx, "sec-edgar")
	if err != nil || src.RequestsPerMinute == nil || *src.RequestsPerMinute > 600 {
		t.Fatalf("sec-edgar's register limit: %+v %v; the SEC allows ten requests a second", src, err)
	}
	srv, reqs := edgarServer(t)
	edgar := &scan.EDGAR{Base: srv.URL, Store: s}
	now := time.Date(2025, 5, 23, 12, 0, 0, 0, time.UTC)
	run := func() {
		err := edgar.Collect(ctx, scan.NewFetcher(srv.Client(), *src.RequestsPerMinute), time.Time{}, now, func(o scan.Observation) error {
			_, err := s.Record(ctx, o)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	run()

	var id int64
	var insiders float64
	if err := pool.QueryRow(ctx, `SELECT id, value FROM scan_points WHERE kind = 'derived' AND series = 'sec.form4.cluster.731766'`).
		Scan(&id, &insiders); err != nil {
		t.Fatalf("no UnitedHealth cluster recorded: %v", err)
	}
	inputs, err := s.Inputs(ctx, id)
	if err != nil || insiders != 5 || len(inputs) != 5 {
		t.Fatalf("cluster: %v insiders, inputs %v (%v); want the five buys", insiders, inputs, err)
	}
	shown, err := s.ForDisplay(ctx, id)
	if err != nil || len(shown.Attribution) != 1 || !strings.Contains(shown.Attribution[0], "EDGAR") {
		t.Errorf("for display: %+v %v", shown, err)
	}
	var holdings int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM scan_points WHERE series LIKE 'sec.13f.1726609.%'
		AND content_hash ~ '^[0-9a-f]{64}$' AND licence <> ''`).Scan(&holdings); err != nil || holdings == 0 {
		t.Errorf("13F holdings stored with provenance: %d %v", holdings, err)
	}

	all := reqs()
	gap := time.Minute / time.Duration(*src.RequestsPerMinute)
	for i, r := range all {
		if r.userAgent != "Talyvor Scan hello@talyvor.com" {
			t.Fatalf("request %s without Talyvor's declared User-Agent: %q", r.path, r.userAgent)
		}
		if i > 0 && r.at.Sub(all[i-1].at) < gap-5*time.Millisecond {
			t.Errorf("requests %s and %s %v apart, faster than the register's limit", all[i-1].path, r.path, r.at.Sub(all[i-1].at))
		}
	}

	// A re-run fetches only the indexes again and records no second cluster.
	run()
	for _, r := range reqs()[len(all):] {
		if strings.HasSuffix(r.path, ".txt") {
			t.Errorf("the re-run fetched %s again", r.path)
		}
	}
	var clusters int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM scan_points WHERE kind = 'derived' AND series LIKE 'sec.form4.cluster.%'`).
		Scan(&clusters); err != nil || clusters != 1 {
		t.Errorf("%d clusters after a re-run (%v), want 1", clusters, err)
	}
}
