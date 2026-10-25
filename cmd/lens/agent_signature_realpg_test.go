package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/agentsig"
	"github.com/talyvor/lens/internal/kya"
)

type oneKeyDirectory struct {
	origin string
	kid    string
	pub    ed25519.PublicKey
}

func (d oneKeyDirectory) Key(_ context.Context, origin, kid string) (ed25519.PublicKey, error) {
	if origin == d.origin && kid == d.kid {
		return d.pub, nil
	}
	return nil, agentsig.ErrUnknownKey
}

// B45.24 — through the real routes on a migrated schema: an outside agent's money-profile signature is "Signed by"
// until the operator records the business behind its domain, then "Verified agent of"; the same nonce twice is 401;
// unsigned is "Unverified agent"; the A2A card routes refuse a signature that does not hold.
func TestAgentSignatures_TrustLevelsAndReplayOnRealPostgres(t *testing.T) {
	pool := agentRoutesDB(t)
	store := agentsig.Store{Pool: pool}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	kid := kya.JWKOf(pub).Kid
	lensURL, _ := url.Parse("https://lens.example")
	v := &agentsig.Verifier{PublicURL: lensURL, Keys: oneKeyDirectory{"https://agent.acme.example", kid, pub},
		Nonces: store, Businesses: store, Now: time.Now}
	r := chi.NewRouter()
	mountAgentSignatureRoutes(r, v)
	r.Put("/v1/admin/agent-operators/{domain}", newCheckedOperatorSetHandler(store)) // step-up is requireStepUp's, tested there

	const body = `{"amount_minor":1250,"currency":"GBP"}`
	signed := func(nonce string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/a2a/signature-check/money", strings.NewReader(body))
		sum := sha256.Sum256([]byte(body))
		req.Header.Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":")
		req.Header.Set("Signature-Agent", `"https://agent.acme.example"`)
		now := time.Now()
		params := fmt.Sprintf(`("@method" "@target-uri" "content-digest" "signature-agent");created=%d;expires=%d;keyid=%q;nonce=%q;tag="web-bot-auth"`,
			now.Unix(), now.Add(2*time.Minute).Unix(), kid, nonce)
		base := "\"@method\": POST\n\"@target-uri\": https://lens.example/v1/a2a/signature-check/money\n" +
			"\"content-digest\": " + req.Header.Get("Content-Digest") + "\n\"signature-agent\": \"https://agent.acme.example\"\n" +
			"\"@signature-params\": " + params
		req.Header.Set("Signature-Input", "sig1="+params)
		req.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(base)))+":")
		return req
	}
	check := func(req *http.Request, wantCode int) agentsig.Result {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != wantCode {
			t.Fatalf("%s %s = %d %s, want %d", req.Method, req.URL, w.Code, w.Body, wantCode)
		}
		var res agentsig.Result
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		return res
	}

	if res := check(httptest.NewRequest(http.MethodPost, "/v1/a2a/signature-check/money", strings.NewReader(body)), 200); res.Label != "Unverified agent" {
		t.Fatalf("unsigned: %+v", res)
	}
	if res := check(signed("n-1"), 200); res.Label != "Signed by agent.acme.example (business not checked)" {
		t.Fatalf("signed, not checked: %+v", res)
	}
	check(signed("n-1"), http.StatusUnauthorized) // the same nonce again

	put := httptest.NewRequest(http.MethodPut, "/v1/admin/agent-operators/Agent.Acme.Example", strings.NewReader(`{"business":"Acme Inc."}`))
	put.Header.Set(moderatorOperatorHeader, "ops@talyvor.com")
	check(put, 200)
	if res := check(signed("n-2"), 200); res.Level != agentsig.Verified || res.Label != "Verified agent of Acme Inc." {
		t.Fatalf("verified: %+v", res)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_signature_nonces WHERE key_id = $1`, kid).Scan(&n); err != nil || n != 2 {
		t.Fatalf("nonces stored = %d %v, want 2", n, err)
	}

	// The A2A routes run under the same check: a tampered signature on Talyvor's own card is refused.
	cards := chi.NewRouter()
	cards.Use(v.Middleware(agentsig.WebBotAuth))
	cards.Get("/.well-known/agent-card.json", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	bad := httptest.NewRequest(http.MethodGet, "/.well-known/agent-card.json", nil)
	bad.Header.Set("Signature-Agent", `"https://agent.acme.example"`)
	bad.Header.Set("Signature-Input", fmt.Sprintf(`sig1=("@authority" "signature-agent");created=%d;expires=%d;keyid=%q;tag="web-bot-auth"`,
		time.Now().Unix(), time.Now().Add(time.Hour).Unix(), kid))
	bad.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(make([]byte, 64))+":")
	w := httptest.NewRecorder()
	cards.ServeHTTP(w, bad)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a signature that does not hold on an A2A route = %d, want 401", w.Code)
	}
}
