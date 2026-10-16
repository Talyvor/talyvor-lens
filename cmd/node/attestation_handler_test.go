package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/povi"
)

// stubAttestor stands in for the real NVIDIA producer: it echoes the gateway nonce into a MOCK EAT's
// eat_nonce, so the handler test can prove the nonce is threaded through to the attestor.
type stubAttestor struct {
	err error
}

func (s stubAttestor) Report(_ context.Context, nonce int64) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return mockEAT(nonce), nil
}

// mockEAT builds a JWT-SHAPED token with the NVIDIA NRAS EAT claim shape. ⚠ It is a MOCK: the signature
// segment is a placeholder, NOT an NVIDIA signature — see TestAttestation_TestArtifact_MockOnly. The claim
// NAMES mirror the real EAT so the gateway (step b) can swap in real NVIDIA-JWKS verification unchanged.
func mockEAT(nonce int64) string {
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	header := b64(`{"alg":"ES384","typ":"JWT"}`)
	payload := b64(fmt.Sprintf(`{"iss":"NRAS","exp":9999999999,"eat_nonce":%d,`+
		`"x-nvidia-gpu-attestation-report-cc-mode":true,"x-nvidia-gpu-identity-cert":"MOCK-GPU-CERT",`+
		`"measres":"comparison-successful"}`, nonce))
	return header + "." + payload + ".MOCK-SIGNATURE-NOT-NVIDIA"
}

func eatClaims(t *testing.T, eat string) map[string]any {
	t.Helper()
	parts := strings.Split(eat, ".")
	if len(parts) != 3 {
		t.Fatalf("EAT is not a 3-segment JWT: %q", eat)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// (proof 1) NONCE ROUND-TRIP: the gateway's nonce is threaded to the attestor and echoed in the returned
// EAT's eat_nonce — proving anti-replay binding end-to-end through the handler.
func TestAttestation_NonceRoundTrip(t *testing.T) {
	_, priv, err := povi.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	srv := NewInferenceServer(nil, "secret", NodeConfig{})
	srv.signer = &receiptSigner{priv: priv, nodeID: "node-1"}
	srv.attestor = stubAttestor{}
	lensPriv := pinLensKey(t, srv)
	const nonce = int64(1234567)

	body, _ := json.Marshal(povi.SignAttestationRequest(lensPriv, nonce))
	rr := httptest.NewRecorder()
	srv.handleAttestation(rr, httptest.NewRequest(http.MethodPost, "/attestation", strings.NewReader(string(body))))

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp povi.AttestationResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Nonce != nonce {
		t.Fatalf("response nonce = %d, want %d", resp.Nonce, nonce)
	}
	got, ok := eatClaims(t, resp.EAT)["eat_nonce"].(float64) // JSON numbers decode to float64
	if !ok || int64(got) != nonce {
		t.Fatalf("EAT eat_nonce = %v, want %d — the gateway nonce must bind into the attestor's report", got, nonce)
	}
}

// pinLensKey pins a fresh Lens challenge key on srv and returns its private half.
func pinLensKey(t *testing.T, srv *InferenceServer) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetChallengePubKey(pub)
	return priv
}

// B42.3: /attestation refuses a request Lens did not sign (unsigned, or signed by another key), as
// /challenge does, so an arbitrary caller cannot make the node spend a hardware attestation.
func TestAttestation_RefusesUnsignedRequest(t *testing.T) {
	_, priv, err := povi.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	srv := NewInferenceServer(nil, "secret", NodeConfig{})
	srv.signer = &receiptSigner{priv: priv, nodeID: "node-1"}
	srv.attestor = stubAttestor{}
	pinLensKey(t, srv)
	_, otherPriv, _ := ed25519.GenerateKey(nil)

	for name, req := range map[string]povi.AttestationRequest{
		"unsigned":        {Nonce: 7},
		"signed by other": povi.SignAttestationRequest(otherPriv, 7),
	} {
		body, _ := json.Marshal(req)
		rr := httptest.NewRecorder()
		srv.handleAttestation(rr, httptest.NewRequest(http.MethodPost, "/attestation", strings.NewReader(string(body))))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s: want 401, got %d: %s", name, rr.Code, rr.Body.String())
		}
	}
}

// (proof 2) INERT: a nil attestor ⇒ /attestation returns 501 (the default posture — nothing produces an
// attestation until an operator wires NODE_ATTESTATION_CMD). Also: a non-POST ⇒ 405.
func TestAttestation_Inert_NilAttestor501(t *testing.T) {
	srv := NewInferenceServer(nil, "secret", NodeConfig{}) // no attestor, no signer
	rr := httptest.NewRecorder()
	srv.handleAttestation(rr, httptest.NewRequest(http.MethodPost, "/attestation", strings.NewReader(`{"nonce":1}`)))
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("nil attestor must return 501, got %d", rr.Code)
	}
	rr2 := httptest.NewRecorder()
	srv.handleAttestation(rr2, httptest.NewRequest(http.MethodGet, "/attestation", nil))
	if rr2.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET must return 405, got %d", rr2.Code)
	}
}

// (proof 3) ED25519 WRAP: the response is signed by the node's key and verifies against the node's pubkey
// (the gateway's node-identity binding); a tampered EAT fails.
func TestAttestation_Ed25519Wrap_Verifies(t *testing.T) {
	pub, priv, err := povi.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	srv := NewInferenceServer(nil, "secret", NodeConfig{})
	srv.signer = &receiptSigner{priv: priv, nodeID: "node-1"}
	srv.attestor = stubAttestor{}
	lensPriv := pinLensKey(t, srv)

	body, _ := json.Marshal(povi.SignAttestationRequest(lensPriv, 55))
	rr := httptest.NewRecorder()
	srv.handleAttestation(rr, httptest.NewRequest(http.MethodPost, "/attestation", strings.NewReader(string(body))))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	var resp povi.AttestationResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)

	if err := povi.VerifyAttestation(resp, pub); err != nil {
		t.Fatalf("handler response must verify under the node pubkey, got %v", err)
	}
	resp.EAT = "tampered"
	if povi.VerifyAttestation(resp, pub) == nil {
		t.Fatal("tampered EAT must fail the node-signature wrap")
	}
}

// (proof 4) THE TEST-ARTIFACT QUESTION, answered honestly. There is NO real NVIDIA-published sample EAT in
// this repo and none is fetched/embedded by this daemon-only PR — so step (a)'s EAT handling is proven
// against a MOCK EAT (a self-constructed JWT with the real claim SHAPE). Real NVIDIA-SIGNATURE verification
// (against NVIDIA's JWKS/root CA) is the GATEWAY's job in step (b) and requires either NVIDIA's published
// test vectors or a real CC-hardware-produced token. This test asserts the mock carries the fields step (b)
// will verify, so the shape is pinned now.
func TestAttestation_TestArtifact_MockOnly(t *testing.T) {
	claims := eatClaims(t, mockEAT(777))
	// eat_nonce — the anti-replay field (load-bearing).
	if fmt.Sprint(claims["eat_nonce"]) != "777" {
		t.Fatalf("mock EAT missing eat_nonce: %v", claims["eat_nonce"])
	}
	// iss = NRAS, exp present (freshness), CC-mode true, GPU identity cert present, measurements matched.
	if claims["iss"] != "NRAS" {
		t.Errorf("iss = %v, want NRAS", claims["iss"])
	}
	if claims["exp"] == nil {
		t.Error("exp (freshness) missing")
	}
	if claims["x-nvidia-gpu-attestation-report-cc-mode"] != true {
		t.Errorf("cc-mode claim = %v, want true", claims["x-nvidia-gpu-attestation-report-cc-mode"])
	}
	if claims["x-nvidia-gpu-identity-cert"] == nil {
		t.Error("GPU identity cert claim missing")
	}
	// The signature segment is explicitly a placeholder — NOT an NVIDIA signature.
	if !strings.HasSuffix(mockEAT(1), ".MOCK-SIGNATURE-NOT-NVIDIA") {
		t.Error("mock EAT signature segment should be an explicit placeholder")
	}
	t.Log("PROVEN-AGAINST: MOCK EAT only (no real NVIDIA-signed vector in-tree/fetched); real signature " +
		"verification is step (b) gateway-side against NVIDIA JWKS + needs CC hardware / NVIDIA test vectors.")
}
