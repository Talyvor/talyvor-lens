package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/agentsig"
	"github.com/talyvor/lens/internal/attestation"
	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/povi"
	"github.com/talyvor/lens/internal/safehttp"
)

// attestationNodeSource lists active, receipt-keyed nodes for the attestation sweep. Read-only over
// inference_nodes; no mint/ledger touch.
type attestationNodeSource struct{ pool *pgxpool.Pool }

func (s attestationNodeSource) ActiveNodes(ctx context.Context) ([]attestation.NodeInfo, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, url, ed25519_pubkey FROM inference_nodes WHERE active AND ed25519_pubkey IS NOT NULL AND url <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []attestation.NodeInfo
	for rows.Next() {
		var id, url, pubB64 string
		if err := rows.Scan(&id, &url, &pubB64); err != nil {
			return nil, err
		}
		pub, err := povi.DecodePublicKey(pubB64)
		if err != nil {
			continue // a node with an unparseable key is skipped, not fatal
		}
		out = append(out, attestation.NodeInfo{ID: id, URL: url, Pub: pub})
	}
	return out, rows.Err()
}

// startAttestationVerify wires the Proof-of-Confidential-Compute VERIFY sweep (step b) — INERT unless the
// capability flag is on AND the pinned NVIDIA root CA is configured. Flag-off ⇒ this returns immediately, no
// scheduler, no node dials, no writes. Mints nothing (records a verified hardware class only).
func startAttestationVerify(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, lensChallengePriv ed25519.PrivateKey, leader interface {
	Run(ctx context.Context, name string, ttl time.Duration, fn func(context.Context))
}) {
	if !cfg.NodeAttestationVerifyEnabled {
		return // inert default
	}
	pem := os.Getenv("LENS_NVIDIA_ROOT_CA_PEM")
	if pem == "" {
		log.Printf("node-attestation-verify: LENS_NODE_ATTESTATION_VERIFY_ENABLED set but LENS_NVIDIA_ROOT_CA_PEM empty — sweep stays inert (no pinned root to verify against)")
		return
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(pem)) {
		log.Printf("node-attestation-verify: LENS_NVIDIA_ROOT_CA_PEM did not parse — sweep stays inert")
		return
	}
	jwks := attestation.NewHTTPJWKS(os.Getenv("LENS_NVIDIA_JWKS_URL"), nil, time.Now)
	verifier := attestation.NewVerifier(roots, jwks, time.Now)
	sweep := attestation.NewSweep(attestationNodeSource{pool: pool}, newAttestationClient(cfg, lensChallengePriv), verifier, attestation.NewStore(pool))
	log.Printf("🔐 node-attestation-verify enabled (NVIDIA EAT verification; records hardware class, mints nothing — the mint is step c)")
	go leader.Run(ctx, "node-attestation-verify", 30*time.Second, func(lctx context.Context) {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-lctx.Done():
				return
			case <-ticker.C:
				if _, err := sweep.RunOnce(lctx); err != nil {
					log.Printf("node-attestation-verify sweep: %v", err)
				}
			}
		}
	})
}

// newAttestationClient dials registered node URLs through the same SSRF-guarded client every other node call
// uses (B50.2): a node cannot point Lens at loopback, link-local or the cloud metadata address.
func newAttestationClient(cfg *config.Config, lensChallengePriv ed25519.PrivateKey) *attestation.Client {
	allow, _ := safehttp.ParseCIDRAllowlist(cfg.NodePrivateCIDRAllowlist) // validated, fatally, at startup
	c := attestation.NewClient(lensChallengePriv, 10*time.Second)
	c.SetHTTPClient(newNodeHTTPClient(cfg.NodeTLSSkipVerify, 10*time.Second, allow))
	return c
}

// newAgentKeyDirectory reads a calling agent's key directory from the origin the agent names, so only through
// safehttp (B50.2).
func newAgentKeyDirectory() *agentsig.HTTPDirectory {
	return &agentsig.HTTPDirectory{Client: safehttp.Client(5 * time.Second)}
}
