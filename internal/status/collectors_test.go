package status

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/ecbrate"
	"github.com/talyvor/lens/internal/scan"
)

type fakeCollectors []scan.CollectorStatus

func (f fakeCollectors) Status(context.Context) []scan.CollectorStatus { return f }

// B52.21: a collector without its API key shows as off with the setting it needs; one collecting shows when it last
// gave a figure; and neither moves the banner.
func TestStatus_DataCollectors(t *testing.T) {
	at := time.Date(2026, 11, 4, 6, 0, 0, 0, time.UTC)
	page := newStatusPage(&fakePinger{}, nil, nil, "test")
	page.UseCollectors(fakeCollectors{
		{Source: "eia", Name: "US EIA open data (weekly petroleum stocks)", Note: "off: needs its API key in the server setting LENS_SCAN_EIA_KEY"},
		{Source: "entsog", Name: "ENTSOG transparency platform (gas flows)", On: true, LastRetrieved: &at},
	})
	resp := page.Check(context.Background())
	if len(resp.Collectors) != 2 {
		t.Fatalf("collectors: %+v", resp.Collectors)
	}
	html := renderHTML(&resp)
	for _, want := range []string{"Data collectors", `<span class="pill">off</span>`, "LENS_SCAN_EIA_KEY",
		`<span class="pill good">collecting</span>`, "04 Nov 06:00"} {
		if !strings.Contains(html, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

type fakeECB ecbrate.Health

func (f fakeECB) Health(context.Context) (ecbrate.Health, error) { return ecbrate.Health(f), nil }

// B59.49: the ECB reference rates are a row of the data collectors: collecting with the latest ECB day held and when
// they were fetched, failing with the last error when none are held or the latest is more than four calendar days old.
func TestStatus_ECBReferenceRates(t *testing.T) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	fetched := today.Add(9 * time.Hour)
	day := func(ago int) *time.Time { d := today.AddDate(0, 0, -ago); return &d }
	for _, c := range []struct {
		name    string
		health  ecbrate.Health
		failing bool
		want    []string
	}{
		{"today's rates", ecbrate.Health{LatestDay: day(0), FetchedAt: &fetched}, false,
			[]string{`<span class="pill good">collecting</span>`, "latest ECB day " + today.Format("02 Jan 2006"), fetched.Format("02 Jan 15:04")}},
		{"four days old, a long weekend", ecbrate.Health{LatestDay: day(4), FetchedAt: &fetched}, false,
			[]string{`<span class="pill good">collecting</span>`, "latest ECB day " + day(4).Format("02 Jan 2006")}},
		{"none held", ecbrate.Health{LastError: "the ECB answered 503 Service Unavailable"}, true,
			[]string{`<span class="pill bad">failing</span>`, "no rates held", "last fetch failed: the ECB answered 503 Service Unavailable"}},
		{"only rates five days old", ecbrate.Health{LatestDay: day(5), FetchedAt: &fetched, LastError: "the ECB answered 503 Service Unavailable"}, true,
			[]string{`<span class="pill bad">failing</span>`, "latest ECB day " + day(5).Format("02 Jan 2006") + ", more than four days old",
				"last fetch failed: the ECB answered 503 Service Unavailable"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			page := newStatusPage(&fakePinger{}, nil, nil, "test")
			page.UseECBRates(fakeECB(c.health))
			resp := page.Check(context.Background())
			var row *scan.CollectorStatus
			for i := range resp.Collectors {
				if resp.Collectors[i].Name == "ECB reference rates" {
					row = &resp.Collectors[i]
				}
			}
			if row == nil {
				t.Fatalf("no ECB reference rates row among the collectors: %+v", resp.Collectors)
			}
			if !row.On || row.Failing != c.failing {
				t.Errorf("row %+v: want on, failing=%v", *row, c.failing)
			}
			html := renderHTML(&resp)
			for _, want := range append([]string{"Data collectors", "ECB reference rates"}, c.want...) {
				if !strings.Contains(html, want) {
					t.Errorf("the page lacks %q", want)
				}
			}
		})
	}
}
