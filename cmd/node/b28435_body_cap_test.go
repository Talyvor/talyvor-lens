package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// B28.435: /inference refuses a body over maxInferenceBody with 413 before it reaches the model.
func TestInferenceServer_OversizedBodyIs413(t *testing.T) {
	ts := httptest.NewServer(NewInferenceServer(&stubProvider{infer: InferResponse{Text: "hi"}}, "real-secret", NodeConfig{}).Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/inference", bytes.NewReader(make([]byte, maxInferenceBody+1)))
	req.Header.Set("X-Node-Secret", "real-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized /inference = %d, want 413", resp.StatusCode)
	}
}
