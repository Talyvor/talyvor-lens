package scan

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

var cropMigrate sync.Once

// cropCollectors returns collectors on a migrated real Postgres, with no crop point left from an earlier test.
func cropCollectors(t *testing.T, srv *httptest.Server) *CropCollectors {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG crop collector test")
	}
	ctx := context.Background()
	cropMigrate.Do(func() {
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatalf("connect for migrate: %v", err)
		}
		defer conn.Close(ctx)
		if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
			t.Fatalf("apply migrations: %v", err)
		}
	})
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DELETE FROM scan_point_inputs WHERE point_id IN (SELECT id FROM scan_points WHERE series LIKE 'crop.%');
		DELETE FROM scan_points WHERE series LIKE 'crop.%'`); err != nil {
		t.Fatal(err)
	}
	c := NewCropCollectors(NewStore(pool), srv.Client(), "test-key")
	c.NASSBase, c.WASDEBase, c.MARSPage, c.MARSRepo, c.STACBase =
		srv.URL+"/nass", srv.URL+"/wasde/", srv.URL+"/mars-bulletins_en", srv.URL+"/repository/bitstream/", srv.URL+"/stac"
	return c
}

func value(t *testing.T, c *CropCollectors, series string, at time.Time) float64 {
	t.Helper()
	var v float64
	if err := c.store.pool.QueryRow(context.Background(), `SELECT value FROM scan_points WHERE series = $1 AND observed_at = $2`,
		series, at).Scan(&v); err != nil {
		t.Fatalf("%s at %s: %v", series, at.Format(time.DateOnly), err)
	}
	return v
}

func cropPoints(t *testing.T, c *CropCollectors, prefix string) int {
	t.Helper()
	var n int
	if err := c.store.pool.QueryRow(context.Background(), `SELECT count(*) FROM scan_points WHERE series LIKE $1`,
		prefix+"%").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/crops/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNASSCollectorStoresRatingsOnce(t *testing.T) {
	body := fixture(t, "nass-quickstats-ia-corn.json")
	var keys atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys.Store(r.URL.Query().Get("key"))
		if r.URL.Path != "/nass" {
			http.NotFound(w, r) // the other crop sources, which Run also reads
			return
		}
		if r.URL.Query().Get("state_alpha") == "IA" && r.URL.Query().Get("commodity_desc") == "CORN" {
			_, _ = w.Write(body)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	c := cropCollectors(t, srv)
	ctx, now := context.Background(), time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	if err := c.store.SetEnabled(ctx, "usda-nass", true); err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ {
		if err := c.collectNASS(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	week := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	if g, e := value(t, c, "crop.nass.ia.corn.condition.good", week), value(t, c, "crop.nass.ia.corn.condition.excellent", week); g != 57 || e != 15 {
		t.Errorf("Iowa corn week ending 4 Oct 2026: good %v, excellent %v; NASS says 57 and 15", g, e)
	}
	if n := cropPoints(t, c, "crop.nass."); n != 9 { // ten rows, one "(NA)" never filled in
		t.Errorf("stored %d ratings after two runs, want 9 once each", n)
	}
	if keys.Load() != "test-key" {
		t.Errorf("Quick Stats was called without the key from server settings")
	}

	// Without the key the collector stays off and reads nothing.
	c.nassKey = ""
	keys.Store("")
	c.Run(ctx, now)
	if src, _ := c.store.Source(ctx, "usda-nass"); src.Enabled || keys.Load() != "" {
		t.Error("usda-nass ran or stayed on without " + SettingNASSKey)
	}
}

func TestWASDECollectorReadsTheMonthsProjections(t *testing.T) {
	body := fixture(t, "wasde1026.xml")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/wasde/wasde1026.xml" {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r) // every other month: not kept, or not issued
	}))
	defer srv.Close()
	c := cropCollectors(t, srv)
	ctx, now := context.Background(), time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	if err := c.store.SetEnabled(ctx, "usda-wasde", true); err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ {
		if err := c.collectWASDE(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for series, want := range map[string]float64{
		"crop.wasde.us.corn.2026-27.yield":             181.2, // not the feed-grain table's 4.34 t/acre above it
		"crop.wasde.us.corn.2026-27.production":        16034,
		"crop.wasde.us.corn.2026-27.ending_stocks":     1849, // October's, not September's 1,567
		"crop.wasde.us.soybeans.2026-27.yield":         53.1,
		"crop.wasde.us.soybeans.2026-27.ending_stocks": 315,
		"crop.wasde.us.wheat.2026-27.production":       1534,
	} {
		if got := value(t, c, series, oct); got != want {
			t.Errorf("%s = %v, the October 2026 WASDE says %v", series, got, want)
		}
	}
	if n := cropPoints(t, c, "crop.wasde."); n != 9 {
		t.Errorf("stored %d WASDE figures after two runs, want 9 (3 crops × yield, production, ending stocks)", n)
	}
}

func TestMARSCollectorReadsEUYieldForecasts(t *testing.T) {
	text := string(fixture(t, "mars-JRC146186-page1.txt"))
	var pdfs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mars-bulletins_en":
			fmt.Fprint(w, `<a href="/jrc-news-and-updates/heatwaves-impact-summer-and-winter-crops-2026-07-27_en">Bulletin</a>
				<a href="/jrc-news-and-updates/jrc-at-a-conference-2026-10-01_en">News</a>`)
		case "/jrc-news-and-updates/heatwaves-impact-summer-and-winter-crops-2026-07-27_en":
			fmt.Fprint(w, `<p>Access the latest&nbsp;<a href="https://publications.jrc.ec.europa.eu/repository/handle/JRC146186">JRC MARS Bulletin</a></p>`)
		case "/jrc-news-and-updates/jrc-at-a-conference-2026-10-01_en":
			fmt.Fprint(w, `<p>No bulletin here.</p>`)
		case "/repository/bitstream/JRC146186/JRC146186_01.pdf":
			pdfs.Add(1)
			fmt.Fprint(w, "%PDF-1.6 recorded bulletin")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := cropCollectors(t, srv)
	c.pdfText = func(_ context.Context, pdf []byte) (string, error) {
		if !bytes.HasPrefix(pdf, []byte("%PDF")) {
			return "", fmt.Errorf("not the bulletin")
		}
		return text, nil // page 1 of the real JRC146186 PDF, as coverText reads it
	}
	ctx := context.Background()
	if err := c.store.SetEnabled(ctx, "jrc-mars", true); err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ {
		if err := c.collectMARS(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	issued := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	for series, want := range map[string]float64{
		"crop.mars.eu.soft_wheat.yield_forecast":    5.88,
		"crop.mars.eu.soft_wheat.yield_5yr_avg":     5.88,
		"crop.mars.eu.grain_maize.yield_forecast":   6.93,
		"crop.mars.eu.grain_maize.yield_5yr_avg":    7.08,
		"crop.mars.eu.total_cereals.yield_forecast": 5.40, // "Total cereals 5.475.615.40": a space before the figures
		"crop.mars.eu.rye.yield_forecast":           4.28,
	} {
		if got := value(t, c, series, issued); got != want {
			t.Errorf("%s = %v, the bulletin of 27 July 2026 says %v", series, got, want)
		}
	}
	if n := cropPoints(t, c, "crop.mars."); n != 34 {
		t.Errorf("stored %d MARS figures, want 34 (17 crops × forecast and five-year average)", n)
	}
	if n := pdfs.Load(); n != 1 {
		t.Errorf("downloaded the bulletin %d times in two runs, want once", n)
	}
}

// --- the crop-health index, on a fixture catalogue serving small GeoTIFFs ---

// writeCOG encodes a single-level tiled GeoTIFF the way Earth Search's are: Deflate with horizontal differencing.
func writeCOG(size, tile, bits int, px func(x, y int) uint16) []byte {
	le := binary.LittleEndian
	across := (size + tile - 1) / tile
	var tiles [][]byte
	for ty := 0; ty < across; ty++ {
		for tx := 0; tx < across; tx++ {
			raw := make([]byte, 0, tile*tile*bits/8)
			for y := 0; y < tile; y++ {
				prev := uint16(0)
				for x := 0; x < tile; x++ {
					v := uint16(0)
					if X, Y := tx*tile+x, ty*tile+y; X < size && Y < size {
						v = px(X, Y)
					}
					d := v - prev
					prev = v
					if bits == 16 {
						raw = le.AppendUint16(raw, d)
					} else {
						raw = append(raw, byte(d))
					}
				}
			}
			var z bytes.Buffer
			zw := zlib.NewWriter(&z)
			_, _ = zw.Write(raw)
			zw.Close()
			tiles = append(tiles, z.Bytes())
		}
	}
	n := len(tiles)
	const entries = 10
	ifdEnd := 8 + 2 + entries*12 + 4
	offsetsAt, countsAt := ifdEnd, ifdEnd+4*n
	data := countsAt + 4*n
	b := le.AppendUint32([]byte("II*\x00"), 8)
	b = le.AppendUint16(b, entries)
	entry := func(tag, typ uint16, count, v uint32) {
		b = le.AppendUint16(le.AppendUint16(b, tag), typ)
		b = le.AppendUint32(le.AppendUint32(b, count), v)
	}
	entry(256, 4, 1, uint32(size))
	entry(257, 4, 1, uint32(size))
	entry(258, 3, 1, uint32(bits))
	entry(259, 3, 1, 8)
	entry(277, 3, 1, 1)
	entry(317, 3, 1, 2)
	entry(322, 3, 1, uint32(tile))
	entry(323, 3, 1, uint32(tile))
	entry(324, 4, uint32(n), uint32(offsetsAt))
	entry(325, 4, uint32(n), uint32(countsAt))
	b = le.AppendUint32(b, 0)
	off := data
	for _, t := range tiles {
		b = le.AppendUint32(b, uint32(off))
		off += len(t)
	}
	for _, t := range tiles {
		b = le.AppendUint32(b, uint32(len(t)))
	}
	for _, t := range tiles {
		b = append(b, t...)
	}
	return b
}

func TestCropHealthIndexAgainstItsNormal(t *testing.T) {
	const size = ndviOverviewWidth
	// Five Julys at NDVI 0.8, then July 2026 at 0.6, then a cloudy week.
	type scene struct {
		at     time.Time
		nir    uint16
		cloudy bool
	}
	scenes := []scene{
		{time.Date(2021, 7, 14, 17, 0, 0, 0, time.UTC), 4500, false},
		{time.Date(2022, 7, 13, 17, 0, 0, 0, time.UTC), 4500, false},
		{time.Date(2023, 7, 12, 17, 0, 0, 0, time.UTC), 4500, false},
		{time.Date(2024, 7, 10, 17, 0, 0, 0, time.UTC), 4500, false},
		{time.Date(2025, 7, 16, 17, 0, 0, 0, time.UTC), 4500, false},
		{time.Date(2026, 7, 15, 17, 0, 0, 0, time.UTC), 2000, false}, // (0.2 − 0.05) / (0.2 + 0.05) = 0.6
		{time.Date(2026, 7, 22, 17, 0, 0, 0, time.UTC), 4500, true},
	}
	files := map[string][]byte{}
	var items []map[string]any
	for i, s := range scenes {
		id := fmt.Sprintf("S2A_15TVG_%s_0_L2A", s.at.Format("20060102"))
		cls := uint16(4)
		if s.cloudy {
			cls = 9 // cloud, high probability
		}
		files[id+"/SCL.tif"] = writeCOG(size, 256, 8, func(x, y int) uint16 { return cls })
		files[id+"/B04.tif"] = writeCOG(size, 512, 16, func(x, y int) uint16 { return 500 })
		nir := s.nir
		files[id+"/B08.tif"] = writeCOG(size, 512, 16, func(x, y int) uint16 { return nir + uint16((x+y+i)%2) })
		bands := []map[string]any{{"nodata": 0, "data_type": "uint16", "scale": 0.0001, "offset": -0.1}}
		items = append(items, map[string]any{"id": id, "type": "Feature",
			"properties": map[string]any{"datetime": s.at.Format(time.RFC3339), "grid:code": "MGRS-15TVG",
				"earthsearch:boa_offset_applied": true},
			"assets": map[string]any{
				"scl": map[string]any{"href": "SRV/cogs/" + id + "/SCL.tif"},
				"red": map[string]any{"href": "SRV/cogs/" + id + "/B04.tif", "raster:bands": bands},
				"nir": map[string]any{"href": "SRV/cogs/" + id + "/B08.tif", "raster:bands": bands},
			}})
	}
	var srv *httptest.Server
	var searches atomic.Int32
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stac/search" {
			searches.Add(1)
			var q struct {
				Query map[string]map[string]string `json:"query"`
			}
			_ = json.NewDecoder(r.Body).Decode(&q)
			out := []map[string]any{}
			if q.Query["grid:code"]["eq"] == "MGRS-15TVG" {
				out = items
			}
			b, _ := json.Marshal(map[string]any{"type": "FeatureCollection", "features": out})
			_, _ = w.Write(bytes.ReplaceAll(b, []byte("SRV"), []byte(srv.URL)))
			return
		}
		f, ok := files[strings.TrimPrefix(r.URL.Path, "/cogs/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(f))
	}))
	defer srv.Close()
	c := cropCollectors(t, srv)
	ctx, now := context.Background(), time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	if err := c.store.SetEnabled(ctx, "copernicus-sentinel", true); err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ {
		if err := c.collectNDVI(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	if n := cropPoints(t, c, "crop.ndvi.iowa-central.clear_sky"); n != len(scenes) {
		t.Errorf("stored %d scenes after two runs, want %d once each", n, len(scenes))
	}

	july := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	h, err := c.store.CropHealth(ctx, "iowa-central", july)
	if err != nil {
		t.Fatal(err)
	}
	near := func(p *float64, want float64) bool { return p != nil && math.Abs(*p-want) < 1e-3 }
	if h.Status != "reading" || !near(h.NDVI, 0.6) || !near(h.Normal, 0.8) || !near(h.Anomaly, -0.2) {
		t.Fatalf("week of 13 July 2026: %+v; want NDVI 0.6 against a normal of 0.8, anomaly −0.2", h)
	}
	shown, err := c.store.ForDisplay(ctx, h.Points["anomaly"])
	if err != nil {
		t.Fatal(err)
	}
	if len(shown.Attribution) != 1 || shown.Attribution[0] != "Contains modified Copernicus Sentinel data" {
		t.Errorf("the anomaly is shown with attribution %q, want Copernicus's credit", shown.Attribution)
	}
	if in, _ := c.store.Inputs(ctx, h.Points["anomaly"]); len(in) != 2 {
		t.Errorf("the anomaly lists inputs %v, want the week's reading and its normal", in)
	}

	cloudy, err := c.store.CropHealth(ctx, "iowa-central", time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if cloudy.Status != NotEnoughClearSky || cloudy.NDVI != nil {
		t.Errorf("a week whose only scene is cloud reads %+v, want %q and no NDVI", cloudy, NotEnoughClearSky)
	}
	if none, _ := c.store.CropHealth(ctx, "iowa-central", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)); none.Status != "no image yet" {
		t.Errorf("a week with no scene reads %q", none.Status)
	}
}
