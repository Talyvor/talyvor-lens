package povi

import (
	"crypto/ed25519"
	"errors"
	"fmt"
)

// attestation.go — the node-identity WRAP around a hardware attestation (Proof-of-Confidential-Compute,
// step a). A confidential-compute node answers a gateway nonce challenge with an NVIDIA-signed EAT (Entity
// Attestation Token) proving it runs on genuine secure hardware. That EAT is OPAQUE to this package — its
// NVIDIA signature is verified GATEWAY-SIDE (step b) against NVIDIA's JWKS/root CA. What this package adds
// is the second, orthogonal binding: the NODE signs (node_id | nonce | eat) with its own ed25519 receipt
// key so the gateway knows the EAT was relayed by THIS registered node (not a valid EAT replayed from
// another CC box). The gateway verifies BOTH: this ed25519 wrap (node identity, here) AND the EAT's NVIDIA
// signature (hardware, step b). Mirrors the challenge.go / nodeauth.go signed-handshake shape.

// AttestationRequest is the gateway→node nonce challenge (mirrors ChallengeRequest's nonce-carrying shape).
// The node feeds Nonce into the NVIDIA attestation so NRAS echoes it as the EAT's eat_nonce (anti-replay).
// Signature is Lens's ed25519 (the challenge key) over the nonce; the node refuses an unsigned request,
// as it does a /challenge, so only Lens can make it spend a hardware attestation.
type AttestationRequest struct {
	Nonce     int64  `json:"nonce"`
	Signature []byte `json:"signature"`
}

// attestationRequestPayload is domain-separated from the challenge and node-token payloads so a signature
// Lens made for one can never be replayed as another.
func attestationRequestPayload(nonce int64) []byte {
	return []byte(fmt.Sprintf("talyvor-attestation-request|%d", nonce))
}

// SignAttestationRequest is Lens's side: sign the nonce with its challenge key.
func SignAttestationRequest(lensPriv ed25519.PrivateKey, nonce int64) AttestationRequest {
	return AttestationRequest{Nonce: nonce, Signature: ed25519.Sign(lensPriv, attestationRequestPayload(nonce))}
}

// VerifyAttestationRequest is the node's side: the request must be signed by Lens's pinned key.
func VerifyAttestationRequest(req AttestationRequest, lensPub ed25519.PublicKey) error {
	if len(lensPub) != ed25519.PublicKeySize {
		return errors.New("povi: invalid lens public key length")
	}
	if len(req.Signature) != ed25519.SignatureSize {
		return errors.New("povi: invalid attestation request signature length")
	}
	if !ed25519.Verify(lensPub, attestationRequestPayload(req.Nonce), req.Signature) {
		return errors.New("povi: attestation request signature verification failed")
	}
	return nil
}

// AttestationResponse is the node→gateway reply: the NVIDIA-signed EAT bound to this node by the node's own
// ed25519 signature over (node_id | nonce | eat).
type AttestationResponse struct {
	NodeID    string `json:"node_id"`
	Nonce     int64  `json:"nonce"`
	EAT       string `json:"eat"`       // the NVIDIA NRAS Entity Attestation Token (JWT) — opaque here, verified gateway-side
	Signature []byte `json:"signature"` // node ed25519 over attestationCanonicalPayload(node_id, nonce, eat)
}

// attestationCanonicalPayload is the exact byte string the node signs and the gateway verifies. A '|'
// separator over the fixed (node_id, nonce, eat) triple — deterministic, no ambiguity.
func attestationCanonicalPayload(nodeID string, nonce int64, eat string) []byte {
	return []byte(fmt.Sprintf("%s|%d|%s", nodeID, nonce, eat))
}

// SignAttestation binds an NVIDIA EAT to nodeID+nonce with the node's ed25519 key. The EAT itself is passed
// through verbatim (its NVIDIA signature is checked gateway-side, not here).
func SignAttestation(nodePriv ed25519.PrivateKey, nodeID string, nonce int64, eat string) AttestationResponse {
	resp := AttestationResponse{NodeID: nodeID, Nonce: nonce, EAT: eat}
	resp.Signature = ed25519.Sign(nodePriv, attestationCanonicalPayload(nodeID, nonce, eat))
	return resp
}

// VerifyAttestation checks ONLY the node-identity ed25519 wrap against the node's registered pubkey — NOT
// the EAT's NVIDIA signature (that is a separate gateway-side step against NVIDIA's JWKS). A caller that
// wants full trust must ALSO verify the EAT with NVIDIA's chain and that its eat_nonce == the sent nonce.
func VerifyAttestation(resp AttestationResponse, nodePub ed25519.PublicKey) error {
	if len(nodePub) != ed25519.PublicKeySize {
		return errors.New("povi: invalid node public key length")
	}
	if len(resp.Signature) != ed25519.SignatureSize {
		return errors.New("povi: invalid attestation signature length")
	}
	if !ed25519.Verify(nodePub, attestationCanonicalPayload(resp.NodeID, resp.Nonce, resp.EAT), resp.Signature) {
		return errors.New("povi: attestation node-signature verification failed")
	}
	return nil
}
