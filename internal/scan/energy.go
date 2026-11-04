package scan

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The server settings holding the energy collectors' API keys. GIE's one key serves both AGSI+ and ALSI.
const (
	SettingGIE = "LENS_SCAN_GIE_KEY"
	SettingEIA = "LENS_SCAN_EIA_KEY"
)

// number reads a JSON figure that a source sends either as a number or as a string; "" and "-" are no figure.
func number(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

func decodeJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(v)
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// ── GIE AGSI+ and ALSI ───────────────────────────────────────────────────────────────────────────────────────────

// gieField is one figure of a GIE row: its path in the row, the series suffix and the unit.
type gieField struct {
	path         []string
	suffix, unit string
}

// AGSI collects EU gas storage from GIE AGSI+, for the EU and its five largest storage holders. GIE's register entry
// is internal only until its terms are confirmed, so nothing from it reaches a user (Store.ForDisplay refuses it).
type AGSI struct{ Key, Base string }

func (*AGSI) Source() string  { return "gie-agsi" }
func (*AGSI) Setting() string { return SettingGIE }
func (a *AGSI) Collect(ctx context.Context, f *Fetcher, since, now time.Time, put func(Observation) error) error {
	return collectGIE(ctx, f, or(a.Base, "https://agsi.gie.eu/api"), a.Key, "gie.agsi", []string{"eu", "de", "it", "nl", "fr", "at"},
		[]gieField{
			{[]string{"gasInStorage"}, "gas_in_storage", "TWh"},
			{[]string{"full"}, "full", "%"},
			{[]string{"injection"}, "injection", "GWh/d"},
			{[]string{"withdrawal"}, "withdrawal", "GWh/d"},
		}, since, now, put)
}

// ALSI collects EU LNG terminal stocks and send-out from GIE ALSI.
type ALSI struct{ Key, Base string }

func (*ALSI) Source() string  { return "gie-alsi" }
func (*ALSI) Setting() string { return SettingGIE }
func (a *ALSI) Collect(ctx context.Context, f *Fetcher, since, now time.Time, put func(Observation) error) error {
	return collectGIE(ctx, f, or(a.Base, "https://alsi.gie.eu/api"), a.Key, "gie.alsi", []string{"eu"},
		[]gieField{
			{[]string{"inventory", "gwh"}, "inventory", "GWh"},
			{[]string{"sendOut"}, "send_out", "GWh/d"},
		}, since, now, put)
}

func collectGIE(ctx context.Context, f *Fetcher, base, key, prefix string, countries []string, fields []gieField,
	since, now time.Time, put func(Observation) error) error {
	for _, country := range countries {
		for page, last := 1, 1; page <= last; page++ {
			q := url.Values{"country": {country}, "from": {day(since)}, "to": {day(now)}, "page": {strconv.Itoa(page)}, "size": {"300"}}
			req, _ := http.NewRequest(http.MethodGet, base+"?"+q.Encode(), nil)
			req.Header.Set("x-key", key)
			body, hash, at, err := f.Get(ctx, req)
			if err != nil {
				return err
			}
			var resp struct {
				LastPage int              `json:"last_page"`
				Error    string           `json:"error"`
				Data     []map[string]any `json:"data"`
			}
			if err := decodeJSON(body, &resp); err != nil {
				return fmt.Errorf("%s %s: %w", prefix, country, err)
			}
			if resp.Error != "" {
				return fmt.Errorf("%s %s: %s", prefix, country, resp.Error)
			}
			last = resp.LastPage
			for _, row := range resp.Data {
				gasDay, _ := row["gasDayStart"].(string)
				observed, err := time.Parse("2006-01-02", gasDay)
				if err != nil {
					continue
				}
				for _, fd := range fields {
					var v any = row
					for _, k := range fd.path {
						m, _ := v.(map[string]any)
						v = m[k]
					}
					value, ok := number(v)
					if !ok {
						continue
					}
					if err := put(Observation{Series: prefix + "." + country + "." + fd.suffix, ObservedAt: observed, Value: value,
						Unit: fd.unit, RetrievedAt: at, ContentHash: hash,
						Transforms: []string{fmt.Sprintf("field %s of the row for gas day %s", strings.Join(fd.path, "."), gasDay)}}); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// ── ENTSOG ───────────────────────────────────────────────────────────────────────────────────────────────────────

// entsogPoints are the entry points of Europe's main import corridors, by ENTSOG's operator+point+direction key.
// Chosen because each carries one supplier's gas into the EU or UK, so a change in it is a change in supply.
var entsogPoints = []struct{ slug, key string }{
	{"norway-emden", "de-tso-0005itp-00081entry"},         // Emden (EPT1): Norwegian gas into Germany
	{"norway-dornum", "de-tso-0005itp-00188entry"},        // Dornum / NETRA: Norwegian gas into Germany (Europipe II)
	{"norway-st-fergus", "uk-tso-0001itp-00022entry"},     // St. Fergus: Norwegian and UK North Sea gas into Britain
	{"norway-easington", "uk-tso-0001itp-00091entry"},     // Easington: Langeled, Norwegian gas into Britain
	{"algeria-mazara", "it-tso-0001itp-00093entry"},       // Mazara del Vallo: Transmed, Algerian gas into Italy
	{"algeria-almeria", "es-tso-0006itp-00048entry"},      // Almería: Medgaz, Algerian gas into Spain
	{"azerbaijan-tap", "it-tso-0001itp-00008entry"},       // Melendugno: TAP, Azerbaijani gas into Italy
	{"turkstream-strandzha", "bg-tso-0001itp-00549entry"}, // Strandzha 2: TurkStream, Russian gas into Bulgaria
	{"baltic-pipe-faxe", "pl-tso-0002itp-10009entry"},     // Faxe: Baltic Pipe, Norwegian gas into Poland
}

// ENTSOG collects daily physical flows at entsogPoints, a year of days per request.
type ENTSOG struct{ Base string }

func (*ENTSOG) Source() string  { return "entsog" }
func (*ENTSOG) Setting() string { return "" }
func (e *ENTSOG) Collect(ctx context.Context, f *Fetcher, since, now time.Time, put func(Observation) error) error {
	keys := make([]string, len(entsogPoints))
	slug := map[string]string{}
	for i, p := range entsogPoints {
		keys[i], slug[p.key] = p.key, p.slug
	}
	for from := since; !from.After(now); from = from.AddDate(1, 0, 0) {
		to := from.AddDate(1, 0, -1)
		if to.After(now) {
			to = now
		}
		q := url.Values{"indicator": {"Physical Flow"}, "periodType": {"day"}, "from": {day(from)}, "to": {day(to)},
			"pointDirection": {strings.Join(keys, ",")}, "limit": {"-1"}}
		req, _ := http.NewRequest(http.MethodGet, or(e.Base, "https://transparency.entsog.eu/api/v1")+"/operationalData?"+q.Encode(), nil)
		body, hash, at, err := f.Get(ctx, req)
		if errors.Is(err, ErrNotPublished) { // ENTSOG answers 404 "No result found" for a window without data
			continue
		}
		if err != nil {
			return err
		}
		var resp struct {
			Rows []struct {
				PeriodFrom, OperatorKey, PointKey, DirectionKey, Unit string
				Value                                                 any
			} `json:"operationalData"`
		}
		if err := decodeJSON(body, &resp); err != nil {
			return fmt.Errorf("entsog: %w", err)
		}
		for _, r := range resp.Rows {
			s, ok := slug[strings.ToLower(r.OperatorKey+r.PointKey+r.DirectionKey)]
			value, has := number(r.Value)
			observed, err := time.Parse(time.RFC3339, r.PeriodFrom)
			if !ok || !has || err != nil || r.Unit == "" {
				continue
			}
			if err := put(Observation{Series: "entsog.flow." + s, ObservedAt: observed, Value: value, Unit: r.Unit,
				RetrievedAt: at, ContentHash: hash, Transforms: []string{fmt.Sprintf("value of the Physical Flow row for %s %s %s from %s",
					r.OperatorKey, r.PointKey, r.DirectionKey, r.PeriodFrom)}}); err != nil {
				return err
			}
		}
	}
	return nil
}

// ── EIA ──────────────────────────────────────────────────────────────────────────────────────────────────────────

// eiaSeries are the weekly petroleum stocks Scan reads, by EIA series id.
var eiaSeries = map[string]string{
	"WCESTUS1":              "us-crude-ex-spr",
	"W_EPC0_SAX_YCUOK_MBBL": "cushing-crude",
	"WCSSTUS1":              "us-crude-spr",
	"WGTSTUS1":              "us-gasoline",
	"WDISTUS1":              "us-distillate",
}

// EIA collects the weekly petroleum status report's stocks from EIA's open data API, 5000 rows a request.
type EIA struct{ Key, Base string }

func (*EIA) Source() string  { return "eia" }
func (*EIA) Setting() string { return SettingEIA }
func (e *EIA) Collect(ctx context.Context, f *Fetcher, since, now time.Time, put func(Observation) error) error {
	for offset, total := 0, 1; offset < total; {
		q := url.Values{"api_key": {e.Key}, "frequency": {"weekly"}, "data[0]": {"value"}, "start": {day(since)}, "end": {day(now)},
			"sort[0][column]": {"period"}, "sort[0][direction]": {"asc"}, "offset": {strconv.Itoa(offset)}, "length": {"5000"}}
		for id := range eiaSeries {
			q.Add("facets[series][]", id)
		}
		req, _ := http.NewRequest(http.MethodGet, or(e.Base, "https://api.eia.gov")+"/v2/petroleum/stoc/wstk/data/?"+q.Encode(), nil)
		body, hash, at, err := f.Get(ctx, req)
		if err != nil {
			return err
		}
		var resp struct {
			Response struct {
				Total any `json:"total"`
				Data  []struct {
					Period, Series, Units string
					Value                 any
				} `json:"data"`
			} `json:"response"`
		}
		if err := decodeJSON(body, &resp); err != nil {
			return fmt.Errorf("eia: %w", err)
		}
		t, _ := number(resp.Response.Total)
		total = int(t)
		if len(resp.Response.Data) == 0 {
			break
		}
		offset += len(resp.Response.Data)
		for _, r := range resp.Response.Data {
			s, ok := eiaSeries[r.Series]
			value, has := number(r.Value)
			observed, err := time.Parse("2006-01-02", r.Period)
			if !ok || !has || err != nil || r.Units != "MBBL" {
				continue
			}
			if err := put(Observation{Series: "eia.stocks." + s, ObservedAt: observed, Value: value, Unit: "thousand barrels",
				RetrievedAt: at, ContentHash: hash,
				Transforms: []string{fmt.Sprintf("value of the row for series %s, week ending %s", r.Series, r.Period)}}); err != nil {
				return err
			}
		}
	}
	return nil
}

// ── ECMWF open data ──────────────────────────────────────────────────────────────────────────────────────────────

// ecmwfCities are where heating and cooling move gas and power demand most: the largest consuming cities of the
// biggest EU gas markets and Britain, and of the US Northeast, Midwest and Texas.
var ecmwfCities = []struct {
	slug     string
	lat, lon float64
}{
	{"london", 51.507, -0.128}, {"paris", 48.857, 2.352}, {"berlin", 52.52, 13.405}, {"milan", 45.464, 9.19},
	{"madrid", 40.417, -3.704}, {"warsaw", 52.23, 21.012}, {"new-york", 40.713, -74.006}, {"chicago", 41.878, -87.63},
	{"houston", 29.76, -95.37},
}

// ecmwfSteps are the lead times read from each run: one, three, five and seven days ahead.
var ecmwfSteps = []int{24, 72, 120, 168}

// ecmwfKeeps is how long ECMWF's open-data portal keeps a run; there is nothing older to backfill.
const ecmwfKeeps = 4 * 24 * time.Hour

// ECMWF collects the 2 m temperature forecast of each day's 00 UTC run of the IFS at 0.25°, reading only that field
// from each file through its index.
type ECMWF struct{ Base string }

func (*ECMWF) Source() string  { return "ecmwf-open" }
func (*ECMWF) Setting() string { return "" }
func (e *ECMWF) Collect(ctx context.Context, f *Fetcher, since, now time.Time, put func(Observation) error) error {
	start := now.Add(-ecmwfKeeps)
	if since.After(start) {
		start = since
	}
	base := or(e.Base, "https://data.ecmwf.int/forecasts")
	for run := start.UTC().Truncate(24 * time.Hour); !run.After(now); run = run.Add(24 * time.Hour) {
		for _, step := range ecmwfSteps {
			file := fmt.Sprintf("%s/%s/00z/ifs/0p25/oper/%s000000-%dh-oper-fc", base, run.Format("20060102"), run.Format("20060102"), step)
			req, _ := http.NewRequest(http.MethodGet, file+".index", nil)
			index, _, _, err := f.Get(ctx, req)
			if errors.Is(err, ErrNotPublished) { // the run is not out yet, or no longer kept
				break
			}
			if err != nil {
				return err
			}
			offset, length, err := indexEntry(index, "2t")
			if err != nil {
				return fmt.Errorf("ecmwf %s: %w", file, err)
			}
			req, _ = http.NewRequest(http.MethodGet, file+".grib2", nil)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
			msg, hash, at, err := f.Get(ctx, req)
			if err != nil {
				return err
			}
			field, err := decodeGrib(msg)
			if err != nil {
				return fmt.Errorf("ecmwf %s: %w", file, err)
			}
			valid := run.Add(time.Duration(step) * time.Hour)
			for _, c := range ecmwfCities {
				k, glat, glon := field.nearest(c.lat, c.lon)
				if err := put(Observation{Series: fmt.Sprintf("ecmwf.t2m.%s.%dh", c.slug, step), ObservedAt: valid,
					Value: k - 273.15, Unit: "°C", RetrievedAt: at, ContentHash: hash, Transforms: []string{
						fmt.Sprintf("GRIB2 field 2t of %s.grib2 (run %s 00z, step %dh)", file[len(base)+1:], run.Format("2006-01-02"), step),
						fmt.Sprintf("value at the nearest 0.25° grid point, %.2f, %.2f", glat, glon),
						"kelvin to degrees Celsius: minus 273.15"}}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// indexEntry finds a parameter's byte range in an ECMWF open-data index (one JSON object a line).
func indexEntry(index []byte, param string) (offset, length int64, err error) {
	sc := bufio.NewScanner(bytes.NewReader(index))
	for sc.Scan() {
		var e struct {
			Param  string `json:"param"`
			Offset int64  `json:"_offset"`
			Length int64  `json:"_length"`
		}
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Param == param && e.Length > 0 {
			return e.Offset, e.Length, nil
		}
	}
	return 0, 0, fmt.Errorf("no %s field in the index", param)
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
