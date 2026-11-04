package scan

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestRecordTankPilot re-reads the tank pilot's scenes from Planetary Computer, as pilot/cushing.plan.txt says, and
// rewrites pilot/cushing-scenes.json. It runs only with SCAN_RECORD_TANKS=1 (about a gigabyte of radar tiles).
func TestRecordTankPilot(t *testing.T) {
	if os.Getenv("SCAN_RECORD_TANKS") != "1" {
		t.Skip("set SCAN_RECORD_TANKS=1 to re-record from Planetary Computer")
	}
	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Minute}
	var tanks struct {
		Tanks []struct {
			Lat, Lon float64
			Diameter float64 `json:"diameter_m"`
		} `json:"tanks"`
	}
	readJSON(t, "pilot/cushing-tanks.json", &tanks)
	items, err := searchS1(ctx, client, time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), tankTestTo)
	if err != nil {
		t.Fatal(err)
	}
	rec := tankScenes{Tanks: len(tanks.Tanks), Source: "Microsoft Planetary Computer sentinel-1-grd, relative orbit 34, VV, " +
		"1 Jan 2023 to 30 Jun 2025, read by TestRecordTankPilot. Contains modified Copernicus Sentinel data."}
	seen := map[time.Time]bool{}
	var token string
	var tokenAt time.Time
	for _, it := range items {
		at := it.Properties.Datetime.UTC().Truncate(time.Minute)
		if seen[at] { // a slice of a scene already read
			continue
		}
		if time.Since(tokenAt) > 30*time.Minute { // a token lasts about 45 minutes, and asking often is rate-limited
			if token, err = s1Token(ctx, client); err != nil {
				t.Fatal(err)
			}
			tokenAt = time.Now()
		}
		f, gcps, err := openS1(ctx, client, it.Assets["vv"].Href+"?"+token)
		if err != nil {
			t.Logf("%s: %v", it.ID, err)
			continue
		}
		var sum float64
		read := 0
		for _, tk := range tanks.Tanks {
			v, ok, err := tankReading(ctx, f, gcps, tk.Lat, tk.Lon, tk.Diameter)
			if err != nil {
				t.Fatalf("%s: %v", it.ID, err)
			}
			if ok {
				sum, read = sum+v, read+1
			}
		}
		if float64(read) < 0.9*float64(len(tanks.Tanks)) {
			t.Logf("%s: %d of %d tanks readable, left out", it.ID, read, len(tanks.Tanks))
			continue
		}
		seen[at] = true
		s := tankScene{Item: it.ID, At: it.Properties.Datetime.UTC(), Tanks: read, Platform: it.Properties.Platform,
			Index: math.Round(sum/float64(read)*1e5) / 1e5}
		t.Logf("%s: %d tanks, index %.5f", s.Item, s.Tanks, s.Index)
		rec.Scenes = append(rec.Scenes, s)
	}
	b, _ := json.MarshalIndent(rec, "", " ")
	if err := os.WriteFile("pilot/cushing-scenes.json", append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

type s1Item struct {
	ID         string `json:"id"`
	Properties struct {
		Datetime time.Time `json:"datetime"`
		Platform string    `json:"platform"`
	} `json:"properties"`
	Assets map[string]struct {
		Href string `json:"href"`
	} `json:"assets"`
}

const pcBase = "https://planetarycomputer.microsoft.com/api"

func searchS1(ctx context.Context, client *http.Client, since, until time.Time) ([]s1Item, error) {
	body, _ := json.Marshal(map[string]any{
		"collections": []string{"sentinel-1-grd"},
		"intersects":  map[string]any{"type": "Point", "coordinates": []float64{-96.76, 35.96}},
		"datetime":    since.Format(time.RFC3339) + "/" + until.Format(time.RFC3339),
		"query":       map[string]any{"sat:relative_orbit": map[string]int{"eq": 34}, "sar:instrument_mode": map[string]string{"eq": "IW"}},
		"limit":       100,
		"sortby":      []map[string]string{{"field": "properties.datetime", "direction": "asc"}},
	})
	method, target := http.MethodPost, pcBase+"/stac/v1/search"
	var out []s1Item
	for page := 0; page < 50; page++ {
		req, _ := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		var res struct {
			Features []s1Item `json:"features"`
			Links    []struct {
				Rel, Href, Method string
				Body              json.RawMessage
			} `json:"links"`
		}
		err = json.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, res.Features...)
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
			return out, nil
		}
		target = next
	}
	return nil, fmt.Errorf("too many pages")
}

// s1Token is Planetary Computer's anonymous read token for the collection (no account needed for sentinel-1-grd).
func s1Token(ctx context.Context, client *http.Client) (string, error) {
	var resp *http.Response
	for try := 1; ; try++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, pcBase+"/sas/v1/token/sentinel-1-grd", nil)
		var err error
		if resp, err = client.Do(req); err != nil {
			return "", err
		}
		if resp.StatusCode != http.StatusTooManyRequests || try == 6 {
			break
		}
		resp.Body.Close()
		time.Sleep(time.Duration(try) * 30 * time.Second)
	}
	defer resp.Body.Close()
	var tok struct{ Token string }
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.Token == "" {
		return "", fmt.Errorf("sas token: HTTP %d %v", resp.StatusCode, err)
	}
	return tok.Token, nil
}

// gcp is a ground-control point: image column and row at a longitude and latitude.
type gcp struct{ col, row, lon, lat float64 }

// openS1 opens a GRD measurement COG through a tile cache and reads its ground-control points (GeoTIFF tag 33922).
func openS1(ctx context.Context, client *http.Client, href string) (*cogFile, []gcp, error) {
	cache := map[int64][]byte{}
	get := func(ctx context.Context, off, n int64) ([]byte, error) {
		if b, ok := cache[off]; ok && int64(len(b)) >= n {
			return b[:n], nil
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, href, nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusPartialContent {
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, n))
		if err == nil {
			cache[off] = b
		}
		return b, err
	}
	f, err := openCOG(ctx, get)
	if err != nil {
		return nil, nil, err
	}
	head, err := get(ctx, 0, cogHeaderBytes)
	if err != nil {
		return nil, nil, err
	}
	le := binary.LittleEndian
	ifd := int64(le.Uint32(head[4:]))
	n := int64(le.Uint16(head[ifd:]))
	for i := int64(0); i < n; i++ {
		e := head[ifd+2+i*12:]
		if le.Uint16(e) != 33922 || le.Uint16(e[2:]) != 12 {
			continue
		}
		count, off := int64(le.Uint32(e[4:])), int64(le.Uint32(e[8:]))
		b, err := get(ctx, off, count*8)
		if err != nil {
			return nil, nil, err
		}
		var out []gcp
		for j := int64(0); j+6 <= count; j += 6 {
			v := func(k int64) float64 { return math.Float64frombits(le.Uint64(b[(j+k)*8:])) }
			out = append(out, gcp{col: v(0), row: v(1), lon: v(3), lat: v(4)})
		}
		return f, out, nil
	}
	return nil, nil, fmt.Errorf("no ground-control points")
}

// imageAt places a longitude and latitude in the image by a least-squares affine fit to the six nearest GCPs.
func imageAt(gcps []gcp, lon, lat float64) (col, row float64) {
	near := append([]gcp(nil), gcps...)
	d := func(g gcp) float64 { return math.Hypot((g.lon-lon)*math.Cos(lat*math.Pi/180), g.lat-lat) }
	sort.Slice(near, func(i, j int) bool { return d(near[i]) < d(near[j]) })
	near = near[:min(6, len(near))]
	// Normal equations for [1, lon, lat] → col and → row.
	var a [3][3]float64
	var bc, br [3]float64
	for _, g := range near {
		x := [3]float64{1, g.lon - lon, g.lat - lat}
		for i := range 3 {
			for j := range 3 {
				a[i][j] += x[i] * x[j]
			}
			bc[i] += x[i] * g.col
			br[i] += x[i] * g.row
		}
	}
	return solve3(a, bc)[0], solve3(a, br)[0] // the intercept is the value at (lon, lat)
}

func solve3(a [3][3]float64, b [3]float64) [3]float64 {
	for c := range 3 {
		p := c
		for r := c + 1; r < 3; r++ {
			if math.Abs(a[r][c]) > math.Abs(a[p][c]) {
				p = r
			}
		}
		a[c], a[p], b[c], b[p] = a[p], a[c], b[p], b[c]
		for r := range 3 {
			if r != c && a[c][c] != 0 {
				f := a[r][c] / a[c][c]
				for k := range 3 {
					a[r][k] -= f * a[c][k]
				}
				b[r] -= f * b[c]
			}
		}
	}
	var x [3]float64
	for i := range 3 {
		x[i] = b[i] / a[i][i]
	}
	return x
}

// tankReading is one tank's reading: ln(far half's VV power ÷ near half's) over the rows within 0.6 radius of the
// centre and the columns from one radius plus 30 m before it to one radius plus 30 m after. GRD columns run from
// near range to far, 10 m apart. ok is false when the window leaves the image or holds a no-data pixel.
func tankReading(ctx context.Context, f *cogFile, gcps []gcp, lat, lon, diameter float64) (float64, bool, error) {
	pc, lc := imageAt(gcps, lon, lat)
	r := diameter / 2 / 10
	x0, x1 := int(math.Floor(pc-r-3)), int(math.Ceil(pc+r+3))
	y0, y1 := int(math.Round(lc-0.6*r)), int(math.Round(lc+0.6*r))+1
	lv := f.levels[0]
	if x0 < 0 || y0 < 0 || x1 > lv.width || y1 > lv.height {
		return 0, false, nil
	}
	px, _, err := f.window(ctx, 0, x0, y0, x1, y1)
	if err != nil {
		return 0, false, err
	}
	var near, far float64
	w := x1 - x0
	for i, dn := range px {
		if dn == 0 {
			return 0, false, nil
		}
		p := float64(dn) * float64(dn)
		if float64(x0+i%w)+0.5 < pc {
			near += p
		} else {
			far += p
		}
	}
	return math.Log(far / near), true, nil
}
