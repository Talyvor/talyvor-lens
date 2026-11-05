package status

import (
	"context"
	"strings"
	"testing"
	"time"

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
