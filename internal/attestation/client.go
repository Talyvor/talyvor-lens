package attestation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/talyvor/lens/internal/povi"
)

// Client POSTs a fresh nonce to a node's /attestation endpoint and returns the node's wrapped EAT response.
// Mirrors povi.ChallengeClient's gateway→node POST shape.
// Each request is signed with Lens's challenge key, which the node verifies before attesting.
type Client struct {
	http     *http.Client
	lensPriv ed25519.PrivateKey
}

func NewClient(lensPriv ed25519.PrivateKey, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{http: &http.Client{Timeout: timeout}, lensPriv: lensPriv}
}

// SetHTTPClient swaps the transport (tests inject an httptest client).
func (c *Client) SetHTTPClient(h *http.Client) { c.http = h }

// Fetch sends {nonce} and decodes the node's AttestationResponse (the NVIDIA EAT + the node's ed25519 wrap).
func (c *Client) Fetch(ctx context.Context, nodeURL string, nonce int64) (povi.AttestationResponse, error) {
	var zero povi.AttestationResponse
	if nodeURL == "" {
		return zero, fmt.Errorf("attestation: node has no URL")
	}
	body, _ := json.Marshal(povi.SignAttestationRequest(c.lensPriv, nonce))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, trimSlash(nodeURL)+"/attestation", bytes.NewReader(body))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("attestation: node status %d: %s", resp.StatusCode, string(raw))
	}
	var out povi.AttestationResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, fmt.Errorf("attestation: decode response: %w", err)
	}
	return out, nil
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
