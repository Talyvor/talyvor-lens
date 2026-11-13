package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// B54.45 — A DKIM KEY FOR A RESERVED .test DOMAIN, SO A TESTER CAN CONFIRM A DEAL BY EMAIL WITHOUT REAL DNS.
//
// One more operator route, registered with the other synthetic ones ONLY when LENS_SYNTHETIC_KEY is set:
//
//	POST /v1/synthetic/dkim-keys   {"domain":"acme.test","selector":"s1","key_type":"ed25519","public_key":"<base64>"}
//	                               — publishes that key at s1._domainkey.acme.test for an hour (201). The domain must
//	                               be under .test (RFC 6761), anything else is 400; key_type is rsa (the default, a
//	                               PKIX public key) or ed25519 (its 32 bytes).
//
// The inbound email route (B54.8) then answers a ._domainkey name under .test from these keys and every other name
// from DNS as before. A .test name never resolves in public DNS, so no real counterparty's mail can be vouched for
// this way; and without the synthetic key nothing here exists and the route reads DNS alone.

const (
	syntheticDKIMTTL = time.Hour
	// syntheticDKIMSuffix is the reserved TLD (RFC 6761) a published key's domain must be under.
	syntheticDKIMSuffix = ".test"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type syntheticDKIMKey struct {
	txt     string
	expires time.Time
}

// syntheticDKIMKeys holds the published keys, each for an hour. Nil when LENS_SYNTHETIC_KEY is unset.
type syntheticDKIMKeys struct {
	mu   sync.Mutex
	keys map[string]syntheticDKIMKey // by its TXT name, s1._domainkey.acme.test
	now  func() time.Time
}

func newSyntheticDKIMKeys() *syntheticDKIMKeys {
	return &syntheticDKIMKeys{keys: map[string]syntheticDKIMKey{}, now: time.Now}
}

// resolver answers a ._domainkey name under .test from the published keys, and every other name from next.
func (k *syntheticDKIMKeys) resolver(next func(ctx context.Context, name string) ([]string, error)) func(ctx context.Context, name string) ([]string, error) {
	if k == nil {
		return next
	}
	return func(ctx context.Context, name string) ([]string, error) {
		n := strings.TrimSuffix(strings.ToLower(name), ".")
		if !strings.HasSuffix(n, syntheticDKIMSuffix) || !strings.Contains(n, "._domainkey.") {
			return next(ctx, name)
		}
		k.mu.Lock()
		key, ok := k.keys[n]
		k.mu.Unlock()
		if !ok || !k.now().Before(key.expires) {
			return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
		}
		return []string{key.txt}, nil
	}
}

// testDomain reports whether d is a host name under .test, with at least one label of its own.
func testDomain(d string) bool {
	labels := strings.Split(d, ".")
	if len(labels) < 2 || labels[len(labels)-1] != "test" {
		return false
	}
	for _, l := range labels {
		if !dnsLabel.MatchString(l) {
			return false
		}
	}
	return true
}

func (k *syntheticDKIMKeys) publish(w http.ResponseWriter, r *http.Request) (int, string) {
	var in struct {
		Domain    string `json:"domain"`
		Selector  string `json:"selector"`
		KeyType   string `json:"key_type"`
		PublicKey string `json:"public_key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return 0, "error: bad request"
	}
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(in.Domain)), ".")
	selector := strings.ToLower(strings.TrimSpace(in.Selector))
	if !testDomain(domain) {
		writeJSONErr(w, http.StatusBadRequest, "domain must be a name under .test (RFC 6761), which never resolves in public DNS")
		return 0, "error: not a .test domain"
	}
	if !dnsLabel.MatchString(selector) {
		writeJSONErr(w, http.StatusBadRequest, "selector must be one DNS label")
		return 0, "error: bad selector"
	}
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(in.PublicKey))
	switch keyType := strings.ToLower(in.KeyType); {
	case err != nil:
		writeJSONErr(w, http.StatusBadRequest, "public_key must be base64")
		return 0, "error: bad key"
	case keyType == "" || keyType == "rsa":
		in.KeyType = "rsa"
		if pub, perr := x509.ParsePKIXPublicKey(der); perr != nil {
			err = perr
		} else if _, ok := pub.(*rsa.PublicKey); !ok {
			err = x509.ErrUnsupportedAlgorithm
		}
	case keyType == "ed25519":
		in.KeyType = "ed25519"
		if len(der) != ed25519.PublicKeySize {
			err = x509.ErrUnsupportedAlgorithm
		}
	default:
		writeJSONErr(w, http.StatusBadRequest, "key_type must be rsa or ed25519")
		return 0, "error: bad key type"
	}
	if err != nil {
		writeJSONErr(w, http.StatusBadRequest, "public_key must be an RSA public key (PKIX) or an Ed25519 key's 32 bytes, as key_type says")
		return 0, "error: bad key"
	}

	name := selector + "._domainkey." + domain
	now := k.now()
	k.mu.Lock()
	for n, key := range k.keys {
		if !now.Before(key.expires) {
			delete(k.keys, n)
		}
	}
	k.keys[name] = syntheticDKIMKey{txt: "v=DKIM1; k=" + in.KeyType + "; p=" + base64.StdEncoding.EncodeToString(der), expires: now.Add(syntheticDKIMTTL)}
	k.mu.Unlock()
	writeJSONOK(w, http.StatusCreated, map[string]any{"name": name, "expires_at": now.Add(syntheticDKIMTTL).UTC().Format(time.RFC3339)})
	return 1, "ok"
}
