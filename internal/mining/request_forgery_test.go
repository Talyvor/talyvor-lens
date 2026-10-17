package mining

import (
	"errors"
	"net/http"
	"testing"
)

// countingTransport fails every request and counts how many reached it.
type countingTransport struct{ n int }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n++
	return nil, errors.New("no network in tests")
}

// A node URL pointing at the cloud metadata endpoint must never be requested by the verify probe.
func TestVerifyNodeAsyncRefusesMetadataURL(t *testing.T) {
	ct := &countingTransport{}
	cm := NewComputeMiner(nil, nil)
	cm.SetHTTPClient(&http.Client{Transport: ct})
	cm.verifyNodeAsync("n1", "http://169.254.169.254", "vllm")
	em := NewEmbeddingMiner(nil, nil)
	em.SetHTTPClient(&http.Client{Transport: ct})
	em.verifyNodeAsync("n2", "http://169.254.169.254", "m")
	if ct.n != 0 {
		t.Fatalf("%d probe(s) sent to the metadata endpoint, want 0", ct.n)
	}
	cm.verifyNodeAsync("n3", "https://node.example.com", "vllm") // control: a public URL is probed
	if ct.n != 1 {
		t.Fatalf("control probe count = %d, want 1", ct.n)
	}
}
