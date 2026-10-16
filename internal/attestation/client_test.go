package attestation

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/lens/internal/povi"
)

// B42.3: the node refuses an unsigned /attestation, so Lens's own sweep must sign with its challenge key.
func TestClientFetch_SignsWithLensKey(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req povi.AttestationRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil || povi.VerifyAttestationRequest(req, pub) != nil {
			http.Error(w, "unauthorized attestation request", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(povi.AttestationResponse{NodeID: "n", Nonce: req.Nonce})
	}))
	defer node.Close()

	resp, err := NewClient(priv, 0).Fetch(context.Background(), node.URL, 42)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if resp.Nonce != 42 {
		t.Fatalf("nonce = %d, want 42", resp.Nonce)
	}
	_, other, _ := ed25519.GenerateKey(nil)
	if _, err := NewClient(other, 0).Fetch(context.Background(), node.URL, 42); err == nil {
		t.Fatal("a request signed by another key must be refused")
	}
}
