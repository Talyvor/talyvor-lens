package benchmark

import (
	"math"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/brand"
)

func TestParseBenchmarkOutput_ParsesValidLine(t *testing.T) {
	out := `BenchmarkExactCacheHit-8   500000   2341 ns/op   200 B/op   12 allocs/op`
	got := ParseBenchmarkOutput(out)
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	r := got[0]
	if r.Name != "BenchmarkExactCacheHit" {
		t.Errorf("Name = %q, want BenchmarkExactCacheHit (the -8 suffix is stripped)", r.Name)
	}
	if r.NsPerOp != 2341 {
		t.Errorf("NsPerOp = %d, want 2341", r.NsPerOp)
	}
	if r.AllocsPerOp != 12 {
		t.Errorf("AllocsPerOp = %d, want 12", r.AllocsPerOp)
	}
	if r.BytesPerOp != 200 {
		t.Errorf("BytesPerOp = %d, want 200", r.BytesPerOp)
	}
}

func TestParseBenchmarkOutput_NsToMsConversion(t *testing.T) {
	// 1_000_000 ns/op = 1 ms/op.
	out := `BenchmarkX-4   100   1000000 ns/op`
	got := ParseBenchmarkOutput(out)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if math.Abs(got[0].MsPerOp-1.0) > 1e-9 {
		t.Errorf("MsPerOp = %v, want 1.0 (1e6 ns/op)", got[0].MsPerOp)
	}
}

func TestParseBenchmarkOutput_OpsPerSecCalculation(t *testing.T) {
	// 1000 ns/op = 1,000,000 ops/sec.
	out := `BenchmarkY-1   1   1000 ns/op`
	got := ParseBenchmarkOutput(out)
	if math.Abs(got[0].OpsPerSec-1_000_000) > 1e-3 {
		t.Errorf("OpsPerSec = %v, want 1,000,000", got[0].OpsPerSec)
	}
}

func TestParseBenchmarkOutput_SkipsNonBenchmarkLines(t *testing.T) {
	out := `PASS
ok      github.com/talyvor/lens/benchmarks   1.234s
BenchmarkOne-4    100   500 ns/op
=== RUN   TestSomething
goos: linux
goarch: amd64
BenchmarkTwo-4    50    1500 ns/op  100 B/op  2 allocs/op
--- PASS: TestSomething (0.00s)
`
	got := ParseBenchmarkOutput(out)
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2 (PASS/ok/=== RUN/goos lines must be skipped)", len(got))
	}
	if got[0].Name != "BenchmarkOne" || got[1].Name != "BenchmarkTwo" {
		t.Errorf("names = %v, want [BenchmarkOne BenchmarkTwo]", []string{got[0].Name, got[1].Name})
	}
}

func TestParseBenchmarkOutput_HandlesLineWithoutAllocsOrBytes(t *testing.T) {
	// Output without -benchmem produces only iterations + ns/op.
	out := `BenchmarkRoute-8   1000000   234 ns/op`
	got := ParseBenchmarkOutput(out)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if got[0].AllocsPerOp != 0 || got[0].BytesPerOp != 0 {
		t.Errorf("allocs/bytes should be 0 when not reported; got %+v", got[0])
	}
}

func TestGenerateMarkdown_ProducesValidTableWithHeader(t *testing.T) {
	results := []BenchmarkResult{
		{Name: "BenchmarkExactCacheHit", NsPerOp: 2341, MsPerOp: 0.002341, OpsPerSec: 427169, AllocsPerOp: 12, BytesPerOp: 200},
		{Name: "BenchmarkModelRouting", NsPerOp: 100, MsPerOp: 0.0001, OpsPerSec: 10_000_000, AllocsPerOp: 0, BytesPerOp: 0},
	}
	md := GenerateMarkdown(results, "go1.25", "linux/amd64")

	// Header columns mentioned in spec.
	for _, want := range []string{"Benchmark", "ops/sec", "ms/op", "allocs/op"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing header column %q", want)
		}
	}
	// At least one data row present.
	if !strings.Contains(md, "BenchmarkExactCacheHit") {
		t.Errorf("markdown missing first benchmark row")
	}
	if !strings.Contains(md, "BenchmarkModelRouting") {
		t.Errorf("markdown missing second benchmark row")
	}
	// Table marker (pipe-separator row).
	if !strings.Contains(md, "|---") && !strings.Contains(md, "| ---") {
		t.Errorf("markdown missing separator row; got:\n%s", md)
	}
	// Platform metadata included.
	if !strings.Contains(md, "go1.25") || !strings.Contains(md, "linux/amd64") {
		t.Errorf("markdown missing platform metadata")
	}
}

func TestGenerateHTML_ContainsCompetitiveComparisonAndDarkTheme(t *testing.T) {
	results := []BenchmarkResult{
		{Name: "BenchmarkX", NsPerOp: 100, MsPerOp: 0.0001, OpsPerSec: 1e7},
	}
	html := GenerateHTML(results, "go1.25", "linux/amd64")
	for _, want := range []string{
		"<!doctype html>",
		"TALYVOR LENS",
		"BenchmarkX",
		// Competitive comparison column mandated by spec.
		"LiteLLM",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
}

// The page wears brand v4: the tokens and the symbol, the name written
// in words rather than TALYVOR set in a font, and none of the old amber theme.
func TestGenerateHTML_WearsTheBrand(t *testing.T) {
	html := GenerateHTML([]BenchmarkResult{{Name: "BenchmarkX", NsPerOp: 100}}, "go1.25", "linux/amd64")
	if !strings.Contains(html, brand.TokensCSS) || !strings.Contains(html, brand.Mark) {
		t.Error("HTML missing the brand tokens or the mark")
	}
	if !strings.Contains(html, "<h1>Talyvor Lens</h1>") {
		t.Error("HTML heading should name Talyvor Lens in words beside the mark")
	}
	for _, old := range []string{"#f0a030", "#0c0e12", "#13161c", `class="accent"`} {
		if strings.Contains(html, old) {
			t.Errorf("HTML still carries the pre-brand %s", old)
		}
	}
}
