package scan

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ── Company jets, a beta behind a switch that stays off (B52.30) ───────────────────────────────────────────────────

// SettingJets switches the beta on when "on"; anything else keeps it off. Turning it on is Nick's decision, with the
// FlightAware key and the two assessments in docs/scan/ approved.
const (
	SettingJets           = "LENS_SCAN_JETS"
	SettingFlightAwareKey = "LENS_SCAN_FLIGHTAWARE_KEY"
	// SettingJetsCompanies names the companies whose jets are counted, as the FAA registry writes the registrant's
	// name, separated by ";". Empty counts nothing: every company's fleet would be one AeroAPI request per aircraft.
	SettingJetsCompanies = "LENS_SCAN_JETS_COMPANIES"
)

// JetsDelay is how old a flight must be before it is counted: only UTC days that ended this long ago are aggregated.
const JetsDelay = 24 * time.Hour

// aeroWindow is how far back AeroAPI's /flights keeps flights.
// shortcut: no /history backfill beyond ten days; add it if a report needs a longer series.
const aeroWindow = 10 * 24 * time.Hour

// Exclusion reasons. An aircraft with any of them is never counted, and adding one deletes every stored figure the
// aircraft was part of.
var ExclusionReasons = []string{"erasure", "single-user", "ladd", "pia"}

var ErrExclusionReason = errors.New("scan: an exclusion needs a registration and one of erasure, single-user, ladd, pia")

// Registrant types the FAA registry gives a company (ardata.pdf: 3 corporation, 7 LLC, 8 non-citizen corporation).
// Individuals (1), partnerships (2), co-owners (4, 9) and governments (5) are never counted.
var companyRegistrant = map[string]bool{"3": true, "7": true, "8": true}

// Engine types that make a jet (4 turbo-jet, 5 turbo-fan).
var jetEngine = map[string]bool{"4": true, "5": true}

// A registrant whose name says it holds the aircraft in trust, for someone the registry does not name.
var trustName = regexp.MustCompile(`\b(TRUST|TRUSTS|TRUSTEE|TRUSTEES|TR|TRS|TTEE)\b`)

// Jets counts, per company and per UTC day, the flights its company-owned jets made, and arrivals per airport. It
// never stores an aircraft's registration in a figure: each figure is a company's day.
type Jets struct {
	On        bool
	Key       string
	Companies []string
	Store     *Store
	Registry  string // the FAA's releasable aircraft database (a zip holding MASTER.txt)
	Base      string // AeroAPI
}

func (*Jets) Source() string  { return "flightaware-standard" }
func (*Jets) Setting() string { return SettingFlightAwareKey }

// OffNote says why the collector is off when the switch, not the key, keeps it off.
func (j *Jets) OffNote() string {
	if !j.On {
		return "off: the company jets beta stays off until " + SettingJets + " is on"
	}
	return ""
}

// CompanyJets reads the FAA registry's MASTER.txt and returns, for each named company, its valid company-owned jets
// by registration. A trust, an individual, a withheld name or an aircraft in excluded is never returned.
func CompanyJets(master []byte, companies []string, excluded map[string]bool) (map[string][]string, error) {
	want := map[string]string{}
	for _, c := range companies {
		if n := registrantName(c); n != "" {
			want[n] = n
		}
	}
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(master, []byte("\ufeff"))))
	r.FieldsPerRecord, r.LazyQuotes = -1, true
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("faa registry: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.TrimSpace(h)] = i
	}
	for _, h := range []string{"N-NUMBER", "TYPE REGISTRANT", "NAME", "STATUS CODE", "TYPE ENGINE"} {
		if _, ok := col[h]; !ok {
			return nil, fmt.Errorf("faa registry: no %s column", h)
		}
	}
	out := map[string][]string{}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("faa registry: %w", err)
		}
		get := func(h string) string {
			if i := col[h]; i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		name := registrantName(get("NAME"))
		reg := Registration(get("N-NUMBER"))
		// shortcut: a name the owner asked the FAA to withhold is read as blank or "WITHHELD"; re-check against the
		// first real download before the switch goes on.
		if want[name] == "" || reg == "" || excluded[reg] || get("STATUS CODE") != "V" ||
			!companyRegistrant[get("TYPE REGISTRANT")] || !jetEngine[get("TYPE ENGINE")] ||
			strings.Contains(name, "WITHHELD") || trustName.MatchString(name) {
			continue
		}
		out[name] = append(out[name], reg)
	}
	for _, regs := range out {
		sort.Strings(regs)
	}
	return out, nil
}

func registrantName(s string) string { return strings.Join(strings.Fields(strings.ToUpper(s)), " ") }

// Registration is an N-number as AeroAPI writes it: upper case with its N.
func Registration(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "N") {
		s = "N" + s
	}
	return s
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func companySlug(name string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

type aeroFlight struct {
	Registration string `json:"registration"`
	Blocked      bool   `json:"blocked"`
	Destination  *struct {
		ICAO string `json:"code_icao"`
	} `json:"destination"`
	ActualOn *time.Time `json:"actual_on"`
}

// Collect reads the registry, then each company jet's flights, and puts one figure per company and complete UTC day
// that ended at least JetsDelay ago: its flights, and its arrivals at each airport. A blocked flight (LADD) or one
// that does not carry the registration asked for (a PIA address) is never counted.
func (j *Jets) Collect(ctx context.Context, f *Fetcher, since, now time.Time, put func(Observation) error) error {
	if !j.On || j.Key == "" || len(j.Companies) == 0 {
		return nil
	}
	excluded, err := j.Store.JetExclusions(ctx)
	if err != nil {
		return err
	}
	req, _ := http.NewRequest(http.MethodGet, or(j.Registry, "https://registry.faa.gov/database/ReleasableAircraft.zip"), nil)
	// shortcut: the zip is read whole under the Fetcher's 64 MiB cap; stream it if the FAA's download outgrows that.
	body, registryHash, _, err := f.Get(ctx, req)
	if err != nil {
		return err
	}
	master, err := masterTxt(body)
	if err != nil {
		return err
	}
	fleets, err := CompanyJets(master, j.Companies, excluded)
	if err != nil {
		return err
	}
	end := now.UTC().Add(-JetsDelay).Truncate(24 * time.Hour)
	start := since.UTC().Truncate(24 * time.Hour)
	if floor := end.Add(-aeroWindow); start.Before(floor) {
		start = floor
	}
	if !start.Before(end) {
		return nil
	}
	names := make([]string, 0, len(fleets))
	for n := range fleets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		type dayKey struct {
			day  time.Time
			dest string // "" for all flights
		}
		counts := map[dayKey]float64{}
		flown := map[dayKey]map[string]bool{}
		hashes := []string{registryHash}
		var retrieved time.Time
		for _, reg := range fleets[name] {
			flights, hash, at, err := j.flights(ctx, f, reg, start, end)
			if err != nil {
				return err
			}
			hashes, retrieved = append(hashes, hash...), at
			for _, fl := range flights {
				if fl.Blocked || Registration(fl.Registration) != reg || fl.ActualOn == nil {
					continue
				}
				on := fl.ActualOn.UTC()
				if on.Before(start) || !on.Before(end) {
					continue
				}
				keys := []dayKey{{day: on.Truncate(24 * time.Hour)}}
				if fl.Destination != nil && fl.Destination.ICAO != "" {
					keys = append(keys, dayKey{day: keys[0].day, dest: strings.ToUpper(fl.Destination.ICAO)})
				}
				for _, k := range keys {
					counts[k]++
					if flown[k] == nil {
						flown[k] = map[string]bool{}
					}
					flown[k][reg] = true
				}
			}
		}
		sum := sha256.Sum256([]byte(strings.Join(hashes, "\n")))
		contentHash := hex.EncodeToString(sum[:])
		transforms := []string{
			"FAA registry: valid jets registered to " + name + " as a corporation or LLC; trusts, individuals, withheld names and excluded aircraft left out",
			"AeroAPI flights by registration; blocked flights and flights not carrying the registration left out",
			"count of the company's flights landed on the UTC day, read at least 24 hours after the day ended",
			"content hash: SHA-256 over the registry's and every AeroAPI response's SHA-256, in registration order",
		}
		slug := companySlug(name)
		for d := start; d.Before(end); d = d.Add(24 * time.Hour) {
			counts[dayKey{day: d}] += 0 // a day without flights is a figure too
		}
		keys := make([]dayKey, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(a, b int) bool {
			return keys[a].day.Before(keys[b].day) || keys[a].day.Equal(keys[b].day) && keys[a].dest < keys[b].dest
		})
		for _, k := range keys {
			series, unit, tf := "jets."+slug+".flights", "flights", transforms
			if k.dest != "" {
				series, unit = "jets."+slug+".arrivals."+strings.ToLower(k.dest), "arrivals"
				tf = append(append([]string{}, transforms...), "only flights landing at "+k.dest)
			}
			regs := make([]string, 0, len(flown[k]))
			for r := range flown[k] {
				regs = append(regs, r)
			}
			// Linked first and checked after: an exclusion is committed before it deletes, so one made while this
			// figure is stored either finds it through its links or is seen by the check.
			if err := j.Store.linkJets(ctx, series, k.day, regs); err != nil {
				return err
			}
			if err := put(Observation{Series: series, ObservedAt: k.day, Value: counts[k], Unit: unit,
				RetrievedAt: retrieved, ContentHash: contentHash, Transforms: tf}); err != nil {
				return err
			}
			if err := j.Store.dropIfExcluded(ctx, j.Source(), series, k.day, regs); err != nil {
				return err
			}
		}
	}
	return nil
}

func (j *Jets) flights(ctx context.Context, f *Fetcher, reg string, start, end time.Time) ([]aeroFlight, []string, time.Time, error) {
	base := or(j.Base, "https://aeroapi.flightaware.com/aeroapi")
	next := "/flights/" + url.PathEscape(reg) + "?" + url.Values{"ident_type": {"registration"},
		"start": {start.Format(time.RFC3339)}, "end": {end.Format(time.RFC3339)}}.Encode()
	var out []aeroFlight
	var hashes []string
	var at time.Time
	for page := 0; next != "" && page < 50; page++ {
		req, _ := http.NewRequest(http.MethodGet, base+next, nil)
		req.Header.Set("x-apikey", j.Key)
		body, hash, got, err := f.Get(ctx, req)
		if err != nil {
			return nil, nil, at, err
		}
		var resp struct {
			Flights []aeroFlight `json:"flights"`
			Links   *struct {
				Next string `json:"next"`
			} `json:"links"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, nil, at, fmt.Errorf("aeroapi: %w", err)
		}
		out, hashes, at, next = append(out, resp.Flights...), append(hashes, hash), got, ""
		if resp.Links != nil && strings.HasPrefix(resp.Links.Next, "/") {
			next = resp.Links.Next
		}
	}
	return out, hashes, at, nil
}

func masterTxt(z []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(z), int64(len(z)))
	if err != nil {
		return nil, fmt.Errorf("faa registry: %w", err)
	}
	for _, f := range zr.File {
		if strings.EqualFold(f.Name, "MASTER.txt") {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, 512<<20))
		}
	}
	return nil, errors.New("faa registry: no MASTER.txt in the download")
}

// JetExclusions is every excluded registration.
func (s *Store) JetExclusions(ctx context.Context) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT registration FROM scan_jet_exclusions`)
	if err != nil {
		return nil, err
	}
	regs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	out := map[string]bool{}
	for _, r := range regs {
		out[r] = true
	}
	return out, err
}

func (s *Store) linkJets(ctx context.Context, series string, at time.Time, regs []string) error {
	if len(regs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO scan_jet_aircraft (series, observed_at, registration)
		SELECT $1, $2, r FROM unnest($3::text[]) r ON CONFLICT DO NOTHING`, series, at, regs)
	return err
}

func (s *Store) dropIfExcluded(ctx context.Context, source, series string, at time.Time, regs []string) error {
	if len(regs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM scan_points WHERE kind = 'observed' AND source_id = $1 AND series = $2
		AND observed_at = $3 AND EXISTS (SELECT 1 FROM scan_jet_exclusions WHERE registration = ANY($4::text[]))`,
		source, series, at, regs)
	if err != nil {
		return err
	}
	// A link this run wrote after the exclusion had cleared that aircraft's links.
	_, err = s.pool.Exec(ctx, `DELETE FROM scan_jet_aircraft WHERE registration IN
		(SELECT registration FROM scan_jet_exclusions WHERE registration = ANY($1::text[]))`, regs)
	return err
}

// ExcludeAircraft keeps an aircraft out of every future count and deletes every stored figure it was part of, with
// every figure derived from those, and the record of which figures it was in. An erasure request is reason "erasure".
// It returns how many figures went.
func (s *Store) ExcludeAircraft(ctx context.Context, registration, reason string) (int64, error) {
	reg := Registration(registration)
	ok := false
	for _, r := range ExclusionReasons {
		ok = ok || r == reason
	}
	if reg == "" || !ok {
		return 0, ErrExclusionReason
	}
	// Committed before the delete, so a collector storing a figure meanwhile sees it (Jets.Collect).
	if _, err := s.pool.Exec(ctx, `INSERT INTO scan_jet_exclusions (registration, reason) VALUES ($1, $2)
		ON CONFLICT (registration) DO NOTHING`, reg, reason); err != nil {
		return 0, err
	}
	var n int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE scan_erased ON COMMIT DROP AS WITH RECURSIVE tree(id) AS (
				SELECT p.id FROM scan_points p JOIN scan_jet_aircraft a ON a.series = p.series AND a.observed_at = p.observed_at
				WHERE p.kind = 'observed' AND p.source_id = 'flightaware-standard' AND a.registration = $1
				UNION SELECT i.point_id FROM scan_point_inputs i JOIN tree t ON i.input_id = t.id)
			SELECT id FROM tree`, reg); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM scan_point_inputs WHERE point_id IN (SELECT id FROM scan_erased)
			OR input_id IN (SELECT id FROM scan_erased)`); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM scan_points WHERE id IN (SELECT id FROM scan_erased)`)
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		_, err = tx.Exec(ctx, `DELETE FROM scan_jet_aircraft WHERE registration = $1`, reg)
		return err
	})
	return n, err
}
