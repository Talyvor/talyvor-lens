package audit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// B50.39 — a webhook that cannot be reached must not come back as an error quoting its URL: SIEM and chat
// webhooks carry their secret in the path or query, and the error is logged.
func TestB5039_WebhookErrorDoesNotQuoteTheURL(t *testing.T) {
	pool := newPool(t)
	pool.ExpectQuery(`FROM token_events`).WithArgs(10000).WillReturnRows(auditRows())
	srv := httptest.NewServer(http.NotFoundHandler())
	hook := srv.URL + "/hook/SECRETPATH?token=SECRETQ"
	client := srv.Client()
	srv.Close() // the POST fails at the dial, which net/http reports as a *url.Error quoting the URL

	err := newExporter(pool).WithHTTPClient(client).ExportWebhook(context.Background(), hook, ExportFilter{})
	if err == nil {
		t.Fatal("ExportWebhook to a closed server returned nil")
	}
	if s := err.Error(); strings.Contains(s, "SECRETPATH") || strings.Contains(s, "SECRETQ") {
		t.Fatalf("webhook error quotes the URL's secret: %s", s)
	}
}
