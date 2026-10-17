package localrouter

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

type countingTransport struct{ n int }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n++
	return nil, errors.New("no network in tests")
}

// The health probe must never request an endpoint registered at the cloud metadata address.
func TestProbeRefusesMetadataURL(t *testing.T) {
	ct := &countingTransport{}
	r := NewRouter(nil)
	r.SetHTTPClient(&http.Client{Transport: ct})
	if r.probe(context.Background(), &LocalEndpoint{URL: "http://169.254.169.254", Provider: "vllm"}) {
		t.Fatal("metadata endpoint reported healthy")
	}
	if ct.n != 0 {
		t.Fatalf("%d probe(s) sent to the metadata endpoint, want 0", ct.n)
	}
	r.probe(context.Background(), &LocalEndpoint{URL: "https://node.example.com", Provider: "vllm"})
	if ct.n != 1 {
		t.Fatalf("control probe count = %d, want 1", ct.n)
	}
}
