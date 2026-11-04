package scan

// The crop-health index: NDVI per growing region from Sentinel-2 Level-2A images, read from the public Earth Search
// catalogue (Element 84, AWS open data: free, and the Copernicus licence allows use and redistribution with credit).
// Each scene's region is read at a 160 m overview, so a scene costs about a megabyte and no image service is paid for.
// Clouds, their shadows, snow and missing pixels are masked with the scene's own classification (SCL); a scene with
// too little clear sky stores only how clear it was, and its NDVI is never filled in. Readings are averaged per ISO
// week and compared with the same region's own five-year normal for the same weeks.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// cropRegion is one growing region: the central 30 km × 30 km of one Sentinel-2 tile, chosen so the tile's centre
// sits in the region's cropland.
type cropRegion struct {
	ID, Name, Tile, Why string
}

// CropRegions are the six regions Scan watches first, and why each.
var CropRegions = []cropRegion{
	{"iowa-central", "Central Iowa, US (Boone and Story counties)", "15TVG",
		"Iowa grows more corn than any other US state, and NASS rates Iowa's corn and soybeans weekly, so the index can be checked against an official rating."},
	{"illinois-central", "Central Illinois, US (Champaign, Piatt and DeWitt counties)", "16TCK",
		"Illinois is the second corn state and the first soybean state; a second Corn Belt region tells a local event from a regional one."},
	{"kansas-south-central", "South-central Kansas, US (Kingman and Pratt counties)", "14SNG",
		"Kansas is the largest US winter-wheat state, and its wheat grows from autumn to June, a different calendar from corn; NASS rates it weekly."},
	{"france-beauce", "Beauce, France (Loiret and Essonne)", "31UDP",
		"France grows the most soft wheat in the EU and the Beauce plain is its core; the JRC MARS bulletins forecast the same crop."},
	{"ukraine-poltava", "Central Ukraine (Poltava and Kirovohrad oblasts)", "36UWV",
		"Ukraine is a leading wheat and maize exporter through the Black Sea, and official field reporting there is incomplete, so images add the most."},
	{"brazil-mato-grosso", "Mato Grosso, Brazil (Sorriso and Lucas do Rio Verde)", "21LXG",
		"Mato Grosso grows the most soybeans of any state in the world, in the southern-hemisphere season, when the northern regions are bare."},
}

const (
	// ndviOverviewWidth is the overview read: 10980 px at 10 m / 16 ≈ 160 m a pixel.
	ndviOverviewWidth = 687
	// ndviWindowMetres is the side of each region's square, centred on its tile.
	ndviWindowMetres = 30000
	// minClearSky is the share of a region's pixels that must be clear for a scene's NDVI to count: below half, the
	// clear pixels are a biased sample of the region (the edges of cloud), so the reading says "not enough clear sky".
	minClearSky = 0.5
	// ndviWeekSettles is how long after a week ends its scenes are taken as complete (the catalogue publishes a scene
	// within a day or two).
	ndviWeekSettles = 3 * 24 * time.Hour
	// normalYears and normalMinYears: the normal of a week is the mean of the weekly readings near the same date in
	// the five seasons before, and needs readings from at least three of them.
	normalYears, normalMinYears = 5, 3
)

// NotEnoughClearSky is what a week's reading says when no scene that week had enough clear sky.
const NotEnoughClearSky = "not enough clear sky"

// ndviScene is one scene's reading of one region.
type ndviScene struct {
	Item       string    `json:"item"`
	At         time.Time `json:"at"`
	ClearSky   float64   `json:"clear_sky"`
	NDVI       *float64  `json:"ndvi,omitempty"` // nil when ClearSky < minClearSky
	Hash       string    `json:"-"`
	Transforms []string  `json:"-"`
}

type stacItem struct {
	ID         string `json:"id"`
	Properties struct {
		Datetime time.Time `json:"datetime"`
		// OffsetApplied: the catalogue has already taken the processing-baseline 04.00 offset out of the pixel values,
		// though raster:bands still lists it.
		OffsetApplied bool `json:"earthsearch:boa_offset_applied"`
	} `json:"properties"`
	Assets map[string]struct {
		Href  string `json:"href"`
		Bands []struct {
			Scale  *float64 `json:"scale"`
			Offset *float64 `json:"offset"`
		} `json:"raster:bands"`
	} `json:"assets"`
}

// searchScenes lists a tile's Level-2A scenes between since and until, oldest first.
func (c *CropCollectors) searchScenes(ctx context.Context, tile string, since, until time.Time) ([]stacItem, error) {
	body, _ := json.Marshal(map[string]any{
		"collections": []string{"sentinel-2-l2a"},
		"query":       map[string]any{"grid:code": map[string]string{"eq": "MGRS-" + tile}},
		"datetime":    since.UTC().Format(time.RFC3339) + "/" + until.UTC().Format(time.RFC3339),
		"limit":       100,
		"sortby":      []map[string]string{{"field": "properties.datetime", "direction": "asc"}},
	})
	method, target := http.MethodPost, c.STACBase+"/search"
	var items []stacItem
	for page := 0; page < 200; page++ {
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.client.Do(req)
		if err != nil {
			return nil, err
		}
		var res struct {
			Features []stacItem `json:"features"`
			Links    []struct {
				Rel    string          `json:"rel"`
				Href   string          `json:"href"`
				Method string          `json:"method"`
				Body   json.RawMessage `json:"body"`
			} `json:"links"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, maxCropBody)).Decode(&res)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("stac search: HTTP %d", resp.StatusCode)
		}
		if err != nil {
			return nil, fmt.Errorf("stac search: %w", err)
		}
		items = append(items, res.Features...)
		next := ""
		for _, l := range res.Links {
			if l.Rel == "next" {
				next, method, body = l.Href, http.MethodGet, nil
				if strings.EqualFold(l.Method, http.MethodPost) {
					method, body = http.MethodPost, l.Body
				}
			}
		}
		if next == "" || len(res.Features) == 0 {
			return items, nil
		}
		target = next
	}
	return nil, errors.New("stac search: too many pages")
}

// rangeGetter reads byte ranges of one URL.
func (c *CropCollectors) rangeGetter(href string, fetched *bytes.Buffer) rangeReader {
	return func(ctx context.Context, off, n int64) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, href, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
		resp, err := c.client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: HTTP %d", href, resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, n))
		if err == nil && off > 0 { // the header is structure; the tiles are the content
			fetched.Write(b)
		}
		return b, err
	}
}

// readScene reads one region from one scene.
func (c *CropCollectors) readScene(ctx context.Context, it stacItem) (ndviScene, error) {
	s := ndviScene{Item: it.ID, At: it.Properties.Datetime.UTC()}
	var fetched bytes.Buffer
	open := func(asset string) (*cogFile, int, cogLevel, error) {
		a, ok := it.Assets[asset]
		if !ok {
			return nil, 0, cogLevel{}, fmt.Errorf("%s: no %s asset", it.ID, asset)
		}
		f, err := openCOG(ctx, c.rangeGetter(a.Href, &fetched))
		if err != nil {
			return nil, 0, cogLevel{}, fmt.Errorf("%s %s: %w", it.ID, asset, err)
		}
		i, lv := f.level(ndviOverviewWidth)
		if lv.width != ndviOverviewWidth && abs(lv.width-ndviOverviewWidth) > 1 {
			return nil, 0, cogLevel{}, fmt.Errorf("%s %s: no %d px overview", it.ID, asset, ndviOverviewWidth)
		}
		return f, i, lv, nil
	}
	scl, sclLevel, lv, err := open("scl")
	if err != nil {
		return s, err
	}
	side := int(math.Round(ndviWindowMetres / (109800.0 / float64(lv.width))))
	x0, y0 := (lv.width-side)/2, (lv.height-side)/2
	mask, _, err := scl.window(ctx, sclLevel, x0, y0, x0+side, y0+side)
	if err != nil {
		return s, err
	}
	clearPx := 0
	for _, v := range mask {
		if v == 4 || v == 5 { // vegetation, not vegetated; never cloud, shadow, cirrus, snow, water or no data
			clearPx++
		}
	}
	s.ClearSky = float64(clearPx) / float64(len(mask))
	s.Transforms = []string{"STAC item " + it.ID,
		fmt.Sprintf("SCL at the %d px overview, central %d×%d px (%d km square)", lv.width, side, side, ndviWindowMetres/1000),
		"clear = SCL class 4 or 5"}
	if s.ClearSky >= minClearSky {
		var bands [2][]uint16
		for i, asset := range []string{"red", "nir"} {
			f, level, blv, err := open(asset)
			if err != nil {
				return s, err
			}
			if blv.width != lv.width {
				return s, fmt.Errorf("%s %s: overview %d px, SCL %d px", it.ID, asset, blv.width, lv.width)
			}
			if bands[i], _, err = f.window(ctx, level, x0, y0, x0+side, y0+side); err != nil {
				return s, err
			}
		}
		scale, offset := bandScale(it, "red")
		nscale, noffset := bandScale(it, "nir")
		var sum float64
		var n int
		for i, m := range mask {
			red, nir := bands[0][i], bands[1][i]
			if (m != 4 && m != 5) || red == 0 || nir == 0 { // 0 is no data
				continue
			}
			r, ni := float64(red)*scale+offset, float64(nir)*nscale+noffset
			if r+ni <= 0 {
				continue
			}
			sum += (ni - r) / (ni + r)
			n++
		}
		if n > 0 {
			v := sum / float64(n)
			s.NDVI = &v
			s.Transforms = append(s.Transforms,
				fmt.Sprintf("reflectance = DN × %g + %g (B04), DN × %g + %g (B08)", scale, offset, nscale, noffset),
				"NDVI = (B08 − B04) / (B08 + B04), mean over clear pixels")
		}
	}
	sum := sha256.Sum256(fetched.Bytes())
	s.Hash = hex.EncodeToString(sum[:])
	return s, nil
}

// bandScale is an asset's scale and offset to reflectance, as its STAC item gives them.
func bandScale(it stacItem, asset string) (scale, offset float64) {
	scale = 1e-4
	if b := it.Assets[asset].Bands; len(b) > 0 {
		if b[0].Scale != nil {
			scale = *b[0].Scale
		}
		if b[0].Offset != nil && !it.Properties.OffsetApplied {
			offset = *b[0].Offset
		}
	}
	return scale, offset
}

func ndviSeries(region, what string) string { return "crop.ndvi." + region + "." + what }

// weekStart is the Monday 00:00 UTC that begins t's ISO week.
func weekStart(t time.Time) time.Time {
	t = t.UTC()
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

// weeklyNDVI is a week's reading: the mean of its scenes' NDVI weighted by how clear each was. ok is false when no
// scene had enough clear sky.
func weeklyNDVI(scenes []ndviScene) (v float64, ok bool) {
	var sum, w float64
	for _, s := range scenes {
		if s.NDVI != nil {
			sum += *s.NDVI * s.ClearSky
			w += s.ClearSky
		}
	}
	if w == 0 {
		return 0, false
	}
	return sum / w, true
}

// ndviNormal is the normal for the week starting week: the mean of the weekly readings within ten days of the same
// date in each of the five seasons before. ok is false with readings from fewer than three of them.
func ndviNormal(weekly map[time.Time]float64, week time.Time) (v float64, inputs []time.Time, ok bool) {
	years := map[int]bool{}
	var sum float64
	for back := 1; back <= normalYears; back++ {
		same := week.AddDate(-back, 0, 0)
		for k, val := range weekly {
			if d := k.Sub(same); d >= -10*24*time.Hour && d <= 10*24*time.Hour {
				sum += val
				inputs = append(inputs, k)
				years[back] = true
			}
		}
	}
	if len(years) < normalMinYears {
		return 0, nil, false
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Before(inputs[j]) })
	return sum / float64(len(inputs)), inputs, true
}

func (c *CropCollectors) collectNDVI(ctx context.Context, now time.Time) error {
	for _, r := range CropRegions {
		since, err := c.since(ctx, "copernicus-sentinel", ndviSeries(r.ID, "clear_sky"), now)
		if err != nil {
			return err
		}
		items, err := c.searchScenes(ctx, r.Tile, since, now)
		if err != nil {
			return fmt.Errorf("%s: %w", r.ID, err)
		}
		var failed int
		var first error
		for _, it := range items {
			// A scene that cannot be read (some older ones are published only as JPEG 2000 on s3://) is skipped, so
			// it never stops the region. shortcut: a failure more than two weeks behind the newest scene is not
			// retried; keep a list of failed scenes if that loses readings.
			if err := c.storeScene(ctx, r, it); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if failed++; first == nil {
					first = err
				}
			}
		}
		if failed > 0 {
			slog.Warn("scan: crop-health scenes not read", slog.String("region", r.ID), slog.Int("scenes", failed),
				slog.String("first", first.Error()))
		}
		if err := c.deriveWeeks(ctx, r, now); err != nil {
			return fmt.Errorf("%s: %w", r.ID, err)
		}
	}
	return nil
}

func (c *CropCollectors) storeScene(ctx context.Context, r cropRegion, it stacItem) error {
	var done bool
	if err := c.store.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM scan_points WHERE kind = 'observed'
		AND source_id = 'copernicus-sentinel' AND series = $1 AND observed_at = $2)`,
		ndviSeries(r.ID, "clear_sky"), it.Properties.Datetime.UTC()).Scan(&done); err != nil || done {
		return err
	}
	s, err := c.readScene(ctx, it)
	if err != nil {
		return err
	}
	at := time.Now().UTC()
	if s.NDVI != nil {
		if _, err := c.recordOnce(ctx, Observation{SourceID: "copernicus-sentinel", Series: ndviSeries(r.ID, "scene"),
			ObservedAt: s.At, Value: *s.NDVI, Unit: "NDVI", RetrievedAt: at, ContentHash: s.Hash, Transforms: s.Transforms}); err != nil {
			return err
		}
	}
	// Stored last: it marks the scene read.
	_, err = c.recordOnce(ctx, Observation{SourceID: "copernicus-sentinel", Series: ndviSeries(r.ID, "clear_sky"),
		ObservedAt: s.At, Value: s.ClearSky, Unit: "fraction of pixels", RetrievedAt: at, ContentHash: s.Hash,
		Transforms: s.Transforms[:3]})
	return err
}

type storedPoint struct {
	id    int64
	at    time.Time
	value float64
}

func (c *CropCollectors) points(ctx context.Context, series string) ([]storedPoint, error) {
	rows, err := c.store.pool.Query(ctx, `SELECT id, observed_at, value FROM scan_points WHERE series = $1
		ORDER BY observed_at, id`, series)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedPoint, error) {
		var p storedPoint
		err := row.Scan(&p.id, &p.at, &p.value)
		p.at = p.at.UTC()
		return p, err
	})
}

// deriveWeeks stores each settled week's reading, its normal and its anomaly, once.
func (c *CropCollectors) deriveWeeks(ctx context.Context, r cropRegion, now time.Time) error {
	scenes, err := c.points(ctx, ndviSeries(r.ID, "scene"))
	if err != nil {
		return err
	}
	clearSky, err := c.points(ctx, ndviSeries(r.ID, "clear_sky"))
	if err != nil {
		return err
	}
	clearAt := map[time.Time]float64{}
	for _, p := range clearSky {
		clearAt[p.at] = p.value
	}
	byWeek := map[time.Time][]storedPoint{}
	for _, p := range scenes {
		byWeek[weekStart(p.at)] = append(byWeek[weekStart(p.at)], p)
	}
	stored, err := c.points(ctx, ndviSeries(r.ID, "week"))
	if err != nil {
		return err
	}
	weekly, weekID := map[time.Time]float64{}, map[time.Time]int64{}
	for _, p := range stored {
		weekly[p.at], weekID[p.at] = p.value, p.id
	}
	weeks := make([]time.Time, 0, len(byWeek))
	for w := range byWeek {
		weeks = append(weeks, w)
	}
	sort.Slice(weeks, func(i, j int) bool { return weeks[i].Before(weeks[j]) })
	for _, w := range weeks {
		if _, ok := weekID[w]; ok || now.Before(w.AddDate(0, 0, 7).Add(ndviWeekSettles)) {
			continue
		}
		var in []ndviScene
		var ids []int64
		for _, p := range byWeek[w] {
			v := p.value
			in = append(in, ndviScene{At: p.at, ClearSky: clearAt[p.at], NDVI: &v})
			ids = append(ids, p.id)
		}
		v, _ := weeklyNDVI(in)
		id, err := c.store.Derive(ctx, Derivation{Series: ndviSeries(r.ID, "week"), ObservedAt: w, Value: v, Unit: "NDVI",
			Inputs: ids, Transforms: []string{"mean of the week's scene NDVI, weighted by each scene's clear sky"}})
		if err != nil {
			return err
		}
		weekly[w], weekID[w] = v, id
	}
	normals, err := c.points(ctx, ndviSeries(r.ID, "normal"))
	if err != nil {
		return err
	}
	hasNormal := map[time.Time]bool{}
	for _, p := range normals {
		hasNormal[p.at] = true
	}
	for w, v := range weekly {
		if hasNormal[w] {
			continue
		}
		normal, inputs, ok := ndviNormal(weekly, w)
		if !ok {
			continue
		}
		ids := make([]int64, len(inputs))
		for i, k := range inputs {
			ids[i] = weekID[k]
		}
		nid, err := c.store.Derive(ctx, Derivation{Series: ndviSeries(r.ID, "normal"), ObservedAt: w, Value: normal,
			Unit: "NDVI", Inputs: ids, Transforms: []string{fmt.Sprintf(
				"mean of the weekly readings within ten days of %s in the %d seasons before (at least %d)",
				w.Format("2 January"), normalYears, normalMinYears)}})
		if err != nil {
			return err
		}
		if _, err := c.store.Derive(ctx, Derivation{Series: ndviSeries(r.ID, "anomaly"), ObservedAt: w,
			Value: v - normal, Unit: "NDVI", Inputs: []int64{weekID[w], nid},
			Transforms: []string{"weekly reading − five-year normal"}}); err != nil {
			return err
		}
	}
	return nil
}

// CropHealth is one region's crop-health reading for one week.
type CropHealth struct {
	Region, Name string
	Week         time.Time
	Status       string   // "reading", NotEnoughClearSky, or "no image yet"
	NDVI         *float64 // nil unless Status is "reading"
	Normal       *float64 // nil without five seasons of history
	Anomaly      *float64
	Points       map[string]int64 // the stored points behind it, for ForDisplay
}

// CropHealth reads a region's week: its NDVI, normal and anomaly, or why there is none.
func (s *Store) CropHealth(ctx context.Context, region string, week time.Time) (CropHealth, error) {
	week = weekStart(week)
	h := CropHealth{Region: region, Week: week, Status: "no image yet", Points: map[string]int64{}}
	for _, r := range CropRegions {
		if r.ID == region {
			h.Name = r.Name
		}
	}
	if h.Name == "" {
		return h, fmt.Errorf("scan: no crop region %q", region)
	}
	rows, err := s.pool.Query(ctx, `SELECT series, id, value FROM scan_points WHERE series = ANY($1) AND observed_at = $2`,
		[]string{ndviSeries(region, "week"), ndviSeries(region, "normal"), ndviSeries(region, "anomaly")}, week)
	if err != nil {
		return h, err
	}
	defer rows.Close()
	for rows.Next() {
		var series string
		var id int64
		var v float64
		if err := rows.Scan(&series, &id, &v); err != nil {
			return h, err
		}
		what := series[strings.LastIndex(series, ".")+1:]
		h.Points[what] = id
		switch what {
		case "week":
			h.NDVI, h.Status = &v, "reading"
		case "normal":
			h.Normal = &v
		case "anomaly":
			h.Anomaly = &v
		}
	}
	if err := rows.Err(); err != nil || h.NDVI != nil {
		return h, err
	}
	var looked bool
	err = s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM scan_points WHERE series = $1 AND observed_at >= $2
		AND observed_at < $3)`, ndviSeries(region, "clear_sky"), week, week.AddDate(0, 0, 7)).Scan(&looked)
	if looked {
		h.Status = NotEnoughClearSky
	}
	return h, err
}
