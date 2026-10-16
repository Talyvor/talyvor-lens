package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/povi"
)

// fakeProvider returns a fixed response so the handler test is deterministic.
type fakeProvider struct{ resp InferResponse }

func (f fakeProvider) Health(context.Context) error                 { return nil }
func (f fakeProvider) ListModels(context.Context) ([]string, error) { return []string{"llama"}, nil }
func (f fakeProvider) Infer(context.Context, InferRequest) (InferResponse, error) {
	return f.resp, nil
}

// The node's /inference handler must attach a signed, verifiable receipt that
// commits to the response — the substrate-meets-reality hook Parts 2/3 build on.
func TestHandleInference_ProducesVerifiableReceipt(t *testing.T) {
	pub, priv, _ := povi.GenerateNodeKey()
	signer, err := newReceiptSigner(NodeState{
		NodeID:      "node-x",
		WorkspaceID: "ws-x",
		Ed25519Priv: base64.StdEncoding.EncodeToString(priv),
	})
	if err != nil || signer == nil {
		t.Fatalf("newReceiptSigner: signer=%v err=%v", signer, err)
	}

	srv := NewInferenceServer(
		fakeProvider{resp: InferResponse{Text: "hello", InputTokens: 3, OutputTokens: 5}},
		"real-secret",
		NodeConfig{},
	)
	srv.SetReceiptSigner(signer, nil) // nil lens → no async submit

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/inference",
		strings.NewReader(`{"model":"llama","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Request-ID", "req-test")
	req.Header.Set("X-Node-Secret", "real-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /inference: %v", err)
	}
	defer resp.Body.Close()

	var out InferResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if out.Receipt == nil {
		t.Fatal("response carried no receipt")
	}
	// Signature verifies against the node's registered public key.
	if err := povi.VerifyReceipt(*out.Receipt, pub); err != nil {
		t.Errorf("receipt failed to verify: %v", err)
	}
	// Receipt fields reflect the request + node identity + response.
	if out.Receipt.RequestID != "req-test" || out.Receipt.NodeID != "node-x" ||
		out.Receipt.WorkspaceID != "ws-x" || out.Receipt.Model != "llama" || out.Receipt.OutputTokens != 5 {
		t.Errorf("receipt fields wrong: %+v", out.Receipt)
	}
	// MerkleRoot commits to the response trace (one leaf per output rune).
	exp := povi.NewTraceBuilder()
	for _, r := range "hello" {
		exp.AddStep([]byte(string(r)))
	}
	if out.Receipt.MerkleRoot != exp.Root() {
		t.Error("receipt MerkleRoot does not match the response trace")
	}
	// The trace was RETAINED so a later challenge can produce sampled paths.
	proofs, err := signer.traces.SampledPaths("req-test", []int{0})
	if err != nil {
		t.Fatalf("trace not retained for challenge: %v", err)
	}
	if !povi.VerifyPath(out.Receipt.MerkleRoot, []byte("h"), proofs[0]) {
		t.Error("retained trace's sampled path does not verify against the receipt root")
	}
}

// With no signer (a node without a signing key), the response carries no
// receipt and the handler still works.
func TestHandleInference_NoSignerNoReceipt(t *testing.T) {
	srv := NewInferenceServer(
		fakeProvider{resp: InferResponse{Text: "ok", OutputTokens: 1}},
		"", NodeConfig{},
	)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/inference", "application/json",
		strings.NewReader(`{"model":"llama","messages":[]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var out InferResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Receipt != nil {
		t.Error("a node without a signing key must not attach a receipt")
	}
}
