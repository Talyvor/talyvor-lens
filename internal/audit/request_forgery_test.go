package audit

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/talyvor/lens/internal/safehttp"
)

type countingTransport struct{ n int }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n++
	return nil, errors.New("no network in tests")
}

// The audit webhook must refuse a metadata-endpoint URL before any request is built.
func TestExportWebhookRefusesMetadataURL(t *testing.T) {
	ct := &countingTransport{}
	e := newExporter(nil).WithHTTPClient(&http.Client{Transport: ct})
	err := e.ExportWebhook(context.Background(), "http://169.254.169.254/latest/meta-data", ExportFilter{})
	if !errors.Is(err, safehttp.ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
	if ct.n != 0 {
		t.Fatalf("%d request(s) sent to the metadata endpoint, want 0", ct.n)
	}
	_ = e.ExportWebhook(context.Background(), "https://hooks.example.com/x", ExportFilter{})
	if ct.n != 1 {
		t.Fatalf("control request count = %d, want 1", ct.n)
	}
}
