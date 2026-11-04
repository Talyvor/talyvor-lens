package scan

// B52.22 — Scan, part 3: crops. Official reports (USDA NASS crop-condition ratings, the monthly WASDE, the EU's JRC
// MARS bulletins) and a crop-health index from Sentinel-2 images (ndvi.go), each stored with its provenance.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ledongthuc/pdf"
)

// SettingNASSKey names the server setting holding the USDA NASS Quick Stats API key. Without it the NASS collector
// stays off.
const SettingNASSKey = "LENS_SCAN_NASS_KEY"

// cropBackfill is how far back a first run reads: the five-year normal of the oldest week it reports needs five
// seasons before it.
const cropBackfill = 6 * 365 * 24 * time.Hour

var errCropNotPublished = errors.New("scan: not published")

// CropCollectors reads the crop sources into the provenance store.
type CropCollectors struct {
	store   *Store
	client  *http.Client
	nassKey string
	// Base URLs, replaced by fixture servers in tests.
	NASSBase, WASDEBase, MARSPage, MARSRepo, STACBase string
	pdfText                                           func(ctx context.Context, pdf []byte) (string, error)
	marsSeen                                          sync.Map // bulletins read in this process that gave no figure or failed
}

func NewCropCollectors(store *Store, client *http.Client, nassKey string) *CropCollectors {
	return &CropCollectors{store: store, client: client, nassKey: nassKey,
		NASSBase:  "https://quickstats.nass.usda.gov/api/api_GET/",
		WASDEBase: "https://www.usda.gov/oce/commodity/wasde/",
		MARSPage:  "https://joint-research-centre.ec.europa.eu/jrc-mars-bulletins-crop-monitoring-europe_en",
		MARSRepo:  "https://publications.jrc.ec.europa.eu/repository/bitstream/",
		STACBase:  "https://earth-search.aws.element84.com/v1",
		pdfText:   coverText,
	}
}

// Run collects every crop source once. A source that fails is logged and the others still run.
func (c *CropCollectors) Run(ctx context.Context, now time.Time) {
	for _, col := range []struct {
		source string
		run    func(context.Context, time.Time) error
	}{
		{"usda-nass", c.collectNASS},
		{"usda-wasde", c.collectWASDE},
		{"jrc-mars", c.collectMARS},
		{"copernicus-sentinel", c.collectNDVI},
	} {
		if col.source == "usda-nass" && c.nassKey == "" {
			_ = c.store.SetEnabled(ctx, col.source, false)
			slog.Info("scan: USDA NASS collector off: " + SettingNASSKey + " is not set")
			continue
		}
		if err := c.store.SetEnabled(ctx, col.source, true); err != nil {
			slog.Warn("scan: crop collector", slog.String("source", col.source), slog.String("err", err.Error()))
			continue
		}
		if err := col.run(ctx, now); err != nil {
			slog.Warn("scan: crop collector", slog.String("source", col.source), slog.String("err", err.Error()))
		}
	}
}

// Every runs the collectors now and then at each interval until ctx ends.
func (c *CropCollectors) Every(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		c.Run(ctx, time.Now().UTC())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// since is where a collector resumes: two weeks before its newest point under prefix (sources revise recent figures
// and publish late), or the full backfill on a first run.
func (c *CropCollectors) since(ctx context.Context, source, prefix string, now time.Time) (time.Time, error) {
	var last *time.Time
	err := c.store.pool.QueryRow(ctx, `SELECT max(observed_at) FROM scan_points WHERE kind = 'observed'
		AND source_id = $1 AND series LIKE $2`, source, prefix+"%").Scan(&last)
	if err != nil || last == nil {
		return now.Add(-cropBackfill), err
	}
	return last.Add(-14 * 24 * time.Hour), nil
}

// recordOnce stores an observation unless the source already gave a figure for that series and time, so a re-run
// stores nothing new.
func (c *CropCollectors) recordOnce(ctx context.Context, o Observation) (int64, error) {
	var id int64
	err := c.store.pool.QueryRow(ctx, `SELECT id FROM scan_points WHERE kind = 'observed' AND source_id = $1
		AND series = $2 AND observed_at = $3 ORDER BY id LIMIT 1`, o.SourceID, o.Series, o.ObservedAt).Scan(&id)
	if err == nil {
		return id, nil
	}
	return c.store.Record(ctx, o)
}

const maxCropBody = 64 << 20 // a MARS bulletin with its maps runs to 40 MB

// get fetches a URL and returns its body, the body's SHA-256 and when it was read.
func (c *CropCollectors) get(ctx context.Context, u string) ([]byte, string, time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	req.Header.Set("User-Agent", "TalyvorScan/1 (+https://talyvor.com)")
	resp, err := c.client.Do(req)
	if err != nil { // the error carries the URL, and NASS's carries its key
		return nil, "", time.Time{}, errors.New(redactKey(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", time.Time{}, errCropNotPublished
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", time.Time{}, fmt.Errorf("%s: HTTP %d", redactKey(u), resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxCropBody+1))
	if err != nil {
		return nil, "", time.Time{}, err
	}
	if len(b) > maxCropBody {
		return nil, "", time.Time{}, fmt.Errorf("%s: larger than %d bytes", redactKey(u), maxCropBody)
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), time.Now().UTC(), nil
}

var queryKey = regexp.MustCompile(`key=[^&]*`)

func redactKey(u string) string { return queryKey.ReplaceAllString(u, "key=…") }

// --- USDA NASS Quick Stats: weekly crop-condition ratings ---

// nassConditions are the state ratings Scan reads: the states of its US crop-health regions (ndvi.go).
var nassConditions = []struct{ state, commodity, class, crop string }{
	{"IA", "CORN", "", "corn"},
	{"IA", "SOYBEANS", "", "soybeans"},
	{"IL", "CORN", "", "corn"},
	{"IL", "SOYBEANS", "", "soybeans"},
	{"KS", "WHEAT", "WINTER", "winter_wheat"},
}

var nassCategory = map[string]string{"PCT VERY POOR": "very_poor", "PCT POOR": "poor", "PCT FAIR": "fair",
	"PCT GOOD": "good", "PCT EXCELLENT": "excellent"}

func (c *CropCollectors) collectNASS(ctx context.Context, now time.Time) error {
	since, err := c.since(ctx, "usda-nass", "crop.nass.", now)
	if err != nil {
		return err
	}
	for _, q := range nassConditions {
		v := url.Values{"key": {c.nassKey}, "source_desc": {"SURVEY"}, "statisticcat_desc": {"CONDITION"},
			"agg_level_desc": {"STATE"}, "freq_desc": {"WEEKLY"}, "state_alpha": {q.state},
			"commodity_desc": {q.commodity}, "year__GE": {strconv.Itoa(since.Year())}, "format": {"JSON"}}
		if q.class != "" {
			v.Set("class_desc", q.class)
		}
		body, hash, at, err := c.get(ctx, c.NASSBase+"?"+v.Encode())
		if err != nil {
			return err
		}
		var res struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return fmt.Errorf("nass: %w", err)
		}
		for i, row := range res.Data {
			str := func(k string) string { s, _ := row[k].(string); return strings.TrimSpace(s) }
			cat, ok := nassCategory[str("unit_desc")]
			week, werr := time.Parse("2006-01-02", str("week_ending"))
			value, verr := strconv.ParseFloat(strings.ReplaceAll(str("Value"), ",", ""), 64)
			if !ok || werr != nil || verr != nil || week.Before(since) { // "(NA)", "(D)": not published, never filled in
				continue
			}
			if _, err := c.recordOnce(ctx, Observation{SourceID: "usda-nass",
				Series:     fmt.Sprintf("crop.nass.%s.%s.condition.%s", strings.ToLower(q.state), q.crop, cat),
				ObservedAt: week, Value: value, Unit: "percent", RetrievedAt: at, ContentHash: hash,
				Transforms: []string{fmt.Sprintf("Quick Stats api_GET JSON data[%d].Value (%s, week ending %s)",
					i, str("short_desc"), str("week_ending"))}}); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- WASDE: the monthly U.S. supply and use projections ---

// wasdeTables are the projections Scan reads from the WASDE's XML, by sub-report and matrix.
var wasdeTables = []struct{ report, matrix, crop string }{
	{"sr11", "matrix1", "wheat"},
	{"sr12", "matrix2", "corn"},
	{"sr15", "matrix1", "soybeans"},
}

var wasdeAttributes = map[string]struct{ name, unit string }{
	"Yield per Harvested Acre": {"yield", "bushels per acre"},
	"Production":               {"production", "million bushels"},
	"Ending Stocks":            {"ending_stocks", "million bushels"},
}

// wasdeFigure is one projection read from a WASDE report.
type wasdeFigure struct {
	Series, Unit, Path string
	Value              float64
}

// parseWASDE reads the current month's projections for the coming marketing year, and the report month.
func parseWASDE(b []byte) (time.Time, []wasdeFigure, error) {
	dec := xml.NewDecoder(strings.NewReader(strings.TrimPrefix(string(b), "\xef\xbb\xbf")))
	type frame struct{ report, matrix, attribute, year, month string }
	stack := []frame{{}}
	var reportMonth time.Time
	var out []wasdeFigure
	seen := map[string]bool{}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return time.Time{}, nil, fmt.Errorf("wasde: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			f := stack[len(stack)-1]
			if strings.HasPrefix(t.Name.Local, "matrix") {
				f.matrix = t.Name.Local
			}
			for _, a := range t.Attr {
				v := strings.Join(strings.Fields(a.Value), " ")
				switch k := strings.TrimRight(a.Name.Local, "0123456789"); {
				case t.Name.Local == "Report" && a.Name.Local == "Name" && strings.HasPrefix(v, "sr"):
					f.report = v
				case t.Name.Local == "Report" && a.Name.Local == "Report_Month" && reportMonth.IsZero():
					reportMonth, _ = time.Parse("January 2006", v)
				case k == "attribute":
					f.attribute = v
				case k == "market_year":
					f.year = v
				case k == "forecast_month":
					f.month = v
				case k == "cell_value" && reportMonth.Year() > 0:
					fig, ok := wasdeCell(f.report, f.matrix, f.attribute, f.year, f.month, v, reportMonth)
					if ok && !seen[fig.Series] {
						seen[fig.Series] = true
						out = append(out, fig)
					}
				}
			}
			stack = append(stack, f)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		}
	}
	if reportMonth.IsZero() {
		return time.Time{}, nil, fmt.Errorf("wasde: no report month")
	}
	return reportMonth, out, nil
}

func wasdeCell(report, matrix, attribute, year, month, value string, reportMonth time.Time) (wasdeFigure, bool) {
	attr, ok := wasdeAttributes[attribute]
	if !ok || !strings.Contains(year, "Proj.") || len(month) < 3 || !strings.HasPrefix(reportMonth.Month().String(), month[:3]) {
		return wasdeFigure{}, false
	}
	for _, t := range wasdeTables {
		if t.report == report && t.matrix == matrix {
			v, err := strconv.ParseFloat(strings.ReplaceAll(value, ",", ""), 64)
			if err != nil {
				return wasdeFigure{}, false
			}
			my := strings.NewReplacer("/", "-", " Proj.", "").Replace(year)
			return wasdeFigure{Series: fmt.Sprintf("crop.wasde.us.%s.%s.%s", t.crop, my, attr.name), Unit: attr.unit,
				Value: v, Path: fmt.Sprintf("%s/%s %q, %s, %s", report, matrix, attribute, year, month)}, true
		}
	}
	return wasdeFigure{}, false
}

func (c *CropCollectors) collectWASDE(ctx context.Context, now time.Time) error {
	since, err := c.since(ctx, "usda-wasde", "crop.wasde.", now)
	if err != nil {
		return err
	}
	var errs []error
	for m := time.Date(since.Year(), since.Month(), 1, 0, 0, 0, 0, time.UTC); !m.After(now); m = m.AddDate(0, 1, 0) {
		name := fmt.Sprintf("wasde%02d%02d.xml", int(m.Month()), m.Year()%100)
		body, hash, at, err := c.get(ctx, c.WASDEBase+name)
		if errors.Is(err, errCropNotPublished) || (err == nil && len(body) == 0) {
			continue // not out yet (the next month's file is empty until release), no longer kept, or not issued (Oct 2025)
		}
		if err != nil {
			return err
		}
		month, figs, err := parseWASDE(body)
		if err != nil { // one month's file that does not parse must not stop the others
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		for _, f := range figs {
			if _, err := c.recordOnce(ctx, Observation{SourceID: "usda-wasde", Series: f.Series, ObservedAt: month,
				Value: f.Value, Unit: f.Unit, RetrievedAt: at, ContentHash: hash,
				Transforms: []string{name + " " + f.Path, "thousands separators removed"}}); err != nil {
				return err
			}
		}
	}
	return errors.Join(errs...)
}

// --- JRC MARS Bulletins: EU yield forecasts ---

var (
	marsArticle = regexp.MustCompile(`href="(/jrc-news-and-updates/[a-z0-9-]+_en)"`)
	marsHandle  = regexp.MustCompile(`publications\.jrc\.ec\.europa\.eu/repository/handle/(JRC\d+)`)
	marsIssued  = regexp.MustCompile(`Issued: (\d{1,2} [A-Z][a-z]+ \d{4})`)
	// The cover table: crop, last five years' average, last month's forecast, this forecast, in t/ha to three
	// significant figures. The PDF's text layer runs the cells together ("Grain maize7.096.506.42", "Total cereals
	// 5.475.615.40"), so each figure is read by its shape.
	marsRow = regexp.MustCompile(`([A-Z][a-z]+(?: [a-z]+)*) ?(\d\.\d\d|\d\d\.\d|\d{3})(\d\.\d\d|\d\d\.\d|\d{3})(\d\.\d\d|\d\d\.\d|\d{3})`)
)

// marsFigure is one EU yield figure read from a bulletin's cover table.
type marsFigure struct {
	Crop                  string
	FiveYearAvg, Forecast float64
}

// parseMARS reads a bulletin's issue date and the EU yield forecasts on its cover.
func parseMARS(text string) (time.Time, []marsFigure, error) {
	if !strings.Contains(text, "EU yield forecasts") {
		return time.Time{}, nil, nil // a bulletin of another series: its cover is not the EU's
	}
	m := marsIssued.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, nil, fmt.Errorf("mars: no issue date")
	}
	issued, err := time.Parse("2 January 2006", m[1])
	if err != nil {
		return time.Time{}, nil, fmt.Errorf("mars: %w", err)
	}
	var out []marsFigure
	for _, r := range marsRow.FindAllStringSubmatch(text, -1) {
		avg, _ := strconv.ParseFloat(r[2], 64)
		fc, _ := strconv.ParseFloat(r[4], 64)
		out = append(out, marsFigure{Crop: strings.ReplaceAll(strings.ToLower(r[1]), " ", "_"), FiveYearAvg: avg, Forecast: fc})
	}
	return issued, out, nil
}

// coverText reads the text layer of a bulletin's first page, where its yield table is. The PDF comes from the
// European Commission's publications repository over HTTPS, not from a user, so it is parsed in process, under a
// recover because the parser panics on malformed input.
func coverText(_ context.Context, b []byte) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("mars: unreadable PDF: %v", r)
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "", fmt.Errorf("mars: %w", err)
	}
	if r.NumPage() < 1 {
		return "", fmt.Errorf("mars: the bulletin has no pages")
	}
	return r.Page(1).GetPlainText(nil)
}

func (c *CropCollectors) collectMARS(ctx context.Context, _ time.Time) error {
	page, _, _, err := c.get(ctx, c.MARSPage)
	if err != nil {
		return err
	}
	base, _ := url.Parse(c.MARSPage)
	seen := map[string]bool{}
	var errs []error
	for _, a := range marsArticle.FindAllStringSubmatch(string(page), -1) {
		if seen[a[1]] {
			continue
		}
		seen[a[1]] = true
		ref, _ := base.Parse(a[1])
		article, _, _, err := c.get(ctx, ref.String())
		if err != nil {
			return err
		}
		h := marsHandle.FindStringSubmatch(string(article))
		if h == nil {
			continue // a news item that is not a bulletin
		}
		if err := c.collectBulletin(ctx, h[1]); err != nil { // one bulletin that cannot be read must not stop the others
			c.marsSeen.Store(h[1], true)
			errs = append(errs, fmt.Errorf("%s: %w", h[1], err))
		}
	}
	return errors.Join(errs...)
}

func (c *CropCollectors) collectBulletin(ctx context.Context, handle string) error {
	step := "publication " + handle
	if _, done := c.marsSeen.Load(handle); done {
		return nil
	}
	var stored bool
	if err := c.store.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM scan_points WHERE source_id = 'jrc-mars'
		AND transforms @> jsonb_build_array($1::text))`, step).Scan(&stored); err != nil || stored {
		return err
	}
	pdf, hash, at, err := c.get(ctx, c.MARSRepo+handle+"/"+handle+"_01.pdf")
	if err != nil {
		return err
	}
	text, err := c.pdfText(ctx, pdf)
	if err != nil {
		return err
	}
	issued, figs, err := parseMARS(text)
	if err != nil {
		return err
	}
	if len(figs) == 0 {
		c.marsSeen.Store(handle, true) // shortcut: re-read after a restart; keep a per-bulletin row if that gets costly
		return nil
	}
	for _, f := range figs {
		for _, p := range []struct {
			series string
			value  float64
			column string
		}{
			{"crop.mars.eu." + f.Crop + ".yield_forecast", f.Forecast, "latest forecast"},
			{"crop.mars.eu." + f.Crop + ".yield_5yr_avg", f.FiveYearAvg, "last 5yrs avg yield"},
		} {
			if _, err := c.recordOnce(ctx, Observation{SourceID: "jrc-mars", Series: p.series, ObservedAt: issued,
				Value: p.value, Unit: "t/ha", RetrievedAt: at, ContentHash: hash,
				Transforms: []string{step, "PDF text layer", "cover table \"EU yield forecasts\", " + f.Crop + ", " + p.column}}); err != nil {
				return err
			}
		}
	}
	return nil
}
