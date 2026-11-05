package scan

import (
	"math"
	"os"
	"testing"
)

// The fixture is ECMWF's open-data 2 m temperature, run 3 Nov 2026 00z, step 24 h, as recorded from data.ecmwf.int.
// The expected figures are eccodes' decoding of the same bytes.
func TestDecodeGribMatchesEccodes(t *testing.T) {
	b, err := os.ReadFile("testdata/ecmwf-20261103-00z-24h-2t.grib2")
	if err != nil {
		t.Fatal(err)
	}
	f, err := decodeGrib(b)
	if err != nil {
		t.Fatal(err)
	}
	sum := 0.0
	for _, v := range f.values {
		sum += v
	}
	if len(f.values) != 1038240 || math.Abs(sum-289832893.39208984) > 1e-3 {
		t.Fatalf("decoded %d values summing to %f, want 1038240 summing to 289832893.392", len(f.values), sum)
	}
	for _, c := range []struct{ lat, lon, want, glat, glon float64 }{
		{52.52, 13.405, 277.84910583496094, 52.5, 13.5},
		{51.5, -0.13, 281.59910583496094, 51.5, -0.25},
		{40.71, -74.01, 289.59910583496094, 40.75, -74},
		{41.88, -87.63, 293.09910583496094, 42, -87.75},
	} {
		v, glat, glon := f.nearest(c.lat, c.lon)
		if v != c.want || glat != c.glat || glon != c.glon {
			t.Errorf("nearest(%v, %v) = %v at %v, %v; want %v at %v, %v", c.lat, c.lon, v, glat, glon, c.want, c.glat, c.glon)
		}
	}
}
