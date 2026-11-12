package benchprobe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/safehttp"
)

// TokenSigner produces the #242 gateway node-auth token for a probe /inference call. Injected (main.go
// wraps povi.SignNodeAuthToken with the gateway challenge key) so this measurement package imports NO
// povi/mining — the import-guard stays clean.
type TokenSigner func(nodeID, requestID, bodySHA256 string, exp int64) (string, error)

// NodeURLLookup resolves a node's base URL from its id (inference_nodes.url).
type NodeURLLookup func(ctx context.Context, nodeID string) (string, error)

// nodeInferReq / nodeInferResp mirror the node's /inference wire shapes (same as the gateway proxy's
// auto-route, #242). The probe body is shape-identical to normal inference traffic — a node cannot
// tell a probe from a real request (node-blind), and carries the INPUT only, never the ground truth.
type nodeInferReq struct {
	Model    string         `json:"model"`
	Messages []nodeInferMsg `json:"messages"`
}
type nodeInferMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type nodeInferResp struct {
	Text string `json:"text"`
}

// maxNodeAnswerBytes bounds the read of a node's /inference answer; a longer one fails to decode.
const maxNodeAnswerBytes = 8 << 20

// HTTPDelivery is the live ProbeDelivery: it POSTs the input to the node's /inference with a
// gateway-signed node-auth token (#242) bound to the probe request_id, and returns the node's answer.
type HTTPDelivery struct {
	sign    TokenSigner
	nodeURL NodeURLLookup
	client  *http.Client
}

// NewHTTPDelivery wires the injected token-signer + node-URL lookup + http client.
func NewHTTPDelivery(sign TokenSigner, nodeURL NodeURLLookup, client *http.Client) *HTTPDelivery {
	if client == nil {
		client = safehttp.Client(30 * time.Second) // SSRF-guarded fallback (node URL is caller-supplied)
	}
	return &HTTPDelivery{sign: sign, nodeURL: nodeURL, client: client}
}

// Deliver sends the node-blind probe to nodeID's /inference and returns the answer text. The token is
// bound to {nodeID, requestID, body_sha256} so the node accepts it (#242); X-Request-ID = requestID
// so an honest node echoes it into the receipt it submits.
func (d *HTTPDelivery) Deliver(ctx context.Context, nodeID, requestID string, req ProbeRequest) (string, error) {
	url, err := d.nodeURL(ctx, nodeID)
	if err != nil {
		return "", fmt.Errorf("benchprobe: node url for %q: %w", nodeID, err)
	}
	body, err := json.Marshal(nodeInferReq{Model: req.Model, Messages: []nodeInferMsg{{Role: "user", Content: req.Input}}})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	token, err := d.sign(nodeID, requestID, hex.EncodeToString(sum[:]), time.Now().Add(30*time.Second).Unix())
	if err != nil {
		return "", fmt.Errorf("benchprobe: sign node-auth token: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(url, "/")+"/inference", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Lens-Node-Token", token)
	httpReq.Header.Set("X-Request-ID", requestID)
	resp, err := d.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("benchprobe: deliver to %q: %w", nodeID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("benchprobe: node %q /inference status %d", nodeID, resp.StatusCode)
	}
	var nr nodeInferResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxNodeAnswerBytes)).Decode(&nr); err != nil {
		return "", fmt.Errorf("benchprobe: decode node answer: %w", err)
	}
	return nr.Text, nil
}
