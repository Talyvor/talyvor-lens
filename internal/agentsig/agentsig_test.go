package agentsig

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/kya"
)

// The fakes stand in for the three things a check asks the world: an agent domain's key directory, the nonces
// already seen, and the businesses Talyvor has checked. The real ones are HTTPDirectory and Store.

type fakeDirectory map[string]map[string]ed25519.PublicKey // origin → keyid → key

func (d fakeDirectory) Key(_ context.Context, origin, keyID string) (ed25519.PublicKey, error) {
	if k, ok := d[origin][keyID]; ok {
		return k, nil
	}
	return nil, ErrUnknownKey
}

type fakeNonces map[string]bool

func (n fakeNonces) Use(_ context.Context, keyID, nonce string, _ time.Time) (bool, error) {
	if n[keyID+" "+nonce] {
		return false, nil
	}
	n[keyID+" "+nonce] = true
	return true, nil
}

type fakeBusinesses map[string]string

func (b fakeBusinesses) Business(_ context.Context, domain string) (string, bool, error) {
	name, ok := b[domain]
	return name, ok, nil
}

// agentKey is one outside agent's key: what its domain publishes, and what it signs with.
type agentKey struct {
	priv  ed25519.PrivateKey
	kid   string
	agent string // the Signature-Agent the request names
}

func newAgentKey(t *testing.T, agent string) agentKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return agentKey{priv: priv, kid: kya.JWKOf(pub).Kid, agent: agent}
}

type sigOpts struct {
	components []string
	created    time.Time
	expires    time.Time
	nonce      string
}

// sign signs req the way an outside agent would, building the RFC 9421 signature base itself, so the test does not
// lean on the code it checks.
func (k agentKey) sign(t *testing.T, req *http.Request, body string, o sigOpts) {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	req.Header.Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":")
	req.Header.Set("Signature-Agent", `"`+k.agent+`"`)
	quoted := make([]string, len(o.components))
	for i, c := range o.components {
		quoted[i] = `"` + c + `"`
	}
	params := "(" + strings.Join(quoted, " ") + ")" + fmt.Sprintf(";created=%d;expires=%d;keyid=%q;alg=\"ed25519\"", o.created.Unix(), o.expires.Unix(), k.kid)
	if o.nonce != "" {
		params += fmt.Sprintf(";nonce=%q", o.nonce)
	}
	params += `;tag="web-bot-auth"`
	var base strings.Builder
	for _, c := range o.components {
		var v string
		switch c {
		case "@method":
			v = req.Method
		case "@target-uri":
			v = "https://lens.example" + req.URL.RequestURI()
		case "@authority":
			v = "lens.example"
		default:
			v = req.Header.Get(c)
		}
		base.WriteString(`"` + c + `": ` + v + "\n")
	}
	base.WriteString(`"@signature-params": ` + params)
	req.Header.Set("Signature-Input", "sig1="+params)
	req.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(ed25519.Sign(k.priv, []byte(base.String())))+":")
}

var moneyComponents = []string{"@method", "@target-uri", "content-digest", "signature-agent"}

type rig struct {
	v     *Verifier
	now   time.Time
	acme  agentKey // published by https://agent.acme.example
	other agentKey // published by https://bots.other.example
}

func newRig(t *testing.T) *rig {
	t.Helper()
	now := time.Unix(1_790_000_000, 0)
	acme, other := newAgentKey(t, "https://agent.acme.example"), newAgentKey(t, "https://bots.other.example")
	dir := fakeDirectory{
		"https://agent.acme.example": {acme.kid: acme.priv.Public().(ed25519.PublicKey)},
		"https://bots.other.example": {other.kid: other.priv.Public().(ed25519.PublicKey)},
	}
	u, _ := url.Parse("https://lens.example")
	return &rig{
		v:    &Verifier{PublicURL: u, Keys: dir, Nonces: fakeNonces{}, Businesses: fakeBusinesses{}, Now: func() time.Time { return now }},
		now:  now,
		acme: acme, other: other,
	}
}

const moneyBody = `{"amount_minor":1250,"currency":"GBP","reference":"INV-7"}`

func (r *rig) moneyRequest(t *testing.T, k agentKey, nonce string) (*http.Request, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/money/payment-requests?ws=1", strings.NewReader(moneyBody))
	k.sign(t, req, moneyBody, sigOpts{components: moneyComponents, created: r.now.Add(-30 * time.Second), expires: r.now.Add(4 * time.Minute), nonce: nonce})
	return req, moneyBody
}

func refused(t *testing.T, what string, err error) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("%s: want the signature refused, got err=%v", what, err)
	}
}

// B45.24 — the DONE line, one behaviour each.

func TestGoodSignatureVerifies(t *testing.T) {
	r := newRig(t)
	req, body := r.moneyRequest(t, r.acme, "n-1")
	res, err := r.v.Check(context.Background(), req, []byte(body), Money)
	if err != nil {
		t.Fatalf("a good money-profile signature was refused: %v", err)
	}
	if res.Level != Signed || res.Domain != "agent.acme.example" || res.KeyID != r.acme.kid {
		t.Fatalf("got %+v", res)
	}
}

func TestChangedBodyFails(t *testing.T) {
	r := newRig(t)
	req, _ := r.moneyRequest(t, r.acme, "n-1")
	_, err := r.v.Check(context.Background(), req, []byte(strings.Replace(moneyBody, "1250", "9250", 1)), Money)
	refused(t, "a changed body under the signed digest", err)

	// The same change with a fresh digest: the digest is covered, so the signature no longer matches.
	req, _ = r.moneyRequest(t, r.acme, "n-2")
	changed := strings.Replace(moneyBody, "1250", "9250", 1)
	sum := sha256.Sum256([]byte(changed))
	req.Header.Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":")
	_, err = r.v.Check(context.Background(), req, []byte(changed), Money)
	refused(t, "a changed body with a recomputed digest", err)
}

func TestExpiredSignatureFails(t *testing.T) {
	r := newRig(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/money/payment-requests", strings.NewReader(moneyBody))
	r.acme.sign(t, req, moneyBody, sigOpts{components: moneyComponents, created: r.now.Add(-4 * time.Minute), expires: r.now.Add(-time.Second), nonce: "n-1"})
	_, err := r.v.Check(context.Background(), req, []byte(moneyBody), Money)
	refused(t, "an expired signature", err)
}

func TestReplayedNonceFails(t *testing.T) {
	r := newRig(t)
	req, body := r.moneyRequest(t, r.acme, "n-once")
	if _, err := r.v.Check(context.Background(), req, []byte(body), Money); err != nil {
		t.Fatal(err)
	}
	again, body := r.moneyRequest(t, r.acme, "n-once")
	_, err := r.v.Check(context.Background(), again, []byte(body), Money)
	refused(t, "a replayed nonce", err)
}

func TestKeyFromAnotherDomainFails(t *testing.T) {
	r := newRig(t)
	// Signed with bots.other.example's key while naming agent.acme.example as the agent.
	impostor := r.other
	impostor.agent = r.acme.agent
	req, body := r.moneyRequest(t, impostor, "n-1")
	_, err := r.v.Check(context.Background(), req, []byte(body), Money)
	refused(t, "a key another domain published", err)
}

func TestThreeTrustLevels(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	unsigned := httptest.NewRequest(http.MethodPost, "/v1/money/payment-requests", strings.NewReader(moneyBody))
	res, err := r.v.Check(ctx, unsigned, []byte(moneyBody), Money)
	if err != nil || res.Level != Unverified || res.Label != "Unverified agent" {
		t.Fatalf("unsigned: %+v %v", res, err)
	}

	req, body := r.moneyRequest(t, r.acme, "n-1")
	res, err = r.v.Check(ctx, req, []byte(body), Money)
	if err != nil || res.Level != Signed || res.Label != "Signed by agent.acme.example (business not checked)" || res.Business != "" {
		t.Fatalf("signed, business not checked: %+v %v", res, err)
	}

	r.v.Businesses = fakeBusinesses{"agent.acme.example": "Acme Inc."}
	req, body = r.moneyRequest(t, r.acme, "n-2")
	res, err = r.v.Check(ctx, req, []byte(body), Money)
	if err != nil || res.Level != Verified || res.Label != "Verified agent of Acme Inc." || res.Business != "Acme Inc." {
		t.Fatalf("verified: %+v %v", res, err)
	}
}

// The money profile is stricter than Web Bot Auth: method, full URL and body digest signed, at most five minutes,
// and a nonce. The same signature that Web Bot Auth accepts on a read is refused on money.
func TestMoneyProfileRequiresMethodURLBodyNonceAndFiveMinutes(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	cases := map[string]sigOpts{
		"no body digest signed": {components: []string{"@method", "@target-uri", "signature-agent"}, created: r.now, expires: r.now.Add(time.Minute), nonce: "a"},
		"no method signed":      {components: []string{"@target-uri", "content-digest", "signature-agent"}, created: r.now, expires: r.now.Add(time.Minute), nonce: "b"},
		"valid for six minutes": {components: moneyComponents, created: r.now, expires: r.now.Add(6 * time.Minute), nonce: "c"},
		"no nonce":              {components: moneyComponents, created: r.now, expires: r.now.Add(time.Minute)},
	}
	for name, o := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/money/payment-requests", strings.NewReader(moneyBody))
		r.acme.sign(t, req, moneyBody, o)
		_, err := r.v.Check(ctx, req, []byte(moneyBody), Money)
		refused(t, name, err)
	}

	read := httptest.NewRequest(http.MethodGet, "/v1/a2a/signature-check", nil)
	r.acme.sign(t, read, "", sigOpts{components: []string{"@authority", "signature-agent"}, created: r.now, expires: r.now.Add(time.Hour)})
	if res, err := r.v.Check(ctx, read, nil, WebBotAuth); err != nil || res.Level != Signed {
		t.Fatalf("a Web Bot Auth signature on a read was refused: %+v %v", res, err)
	}
}

// RFC 9421 Appendix B.2.6: signing a request with Ed25519 — the RFC's own key, request and signature.
func TestRFC9421Ed25519TestVector(t *testing.T) {
	pub, _ := base64.StdEncoding.DecodeString("JrQLj5P/89iXES9+vFgrIy29clF9CC/oPPsw3c5D0bs=")
	req := httptest.NewRequest(http.MethodPost, "/foo?param=Value&Pet=dog", strings.NewReader(`{"hello": "world"}`))
	req.Header.Set("Date", "Tue, 20 Apr 2021 02:07:55 GMT")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Length", "18")
	u, _ := url.Parse("https://example.com")
	in := `sig-b26=("date" "@method" "@path" "@authority" "content-type" "content-length");created=1618884473;keyid="test-key-ed25519"`
	sig := `sig-b26=:wqcAqbmYJ2ji2glfAMaRy4gruYYnx2nEFN2HN6jrnDnQCK1u02Gb04v9EDgwUPiu4A0w6vuQv5lIp5WPpBKRCw==:`
	members, err := parseSignatureInput(in)
	if err != nil || len(members) != 1 {
		t.Fatalf("parse: %v %+v", err, members)
	}
	base, err := signatureBase(req, u, members[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, err := signatureOf(sig, "sig-b26")
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, []byte(base), raw) {
		t.Fatalf("the RFC's signature does not verify over our base:\n%s", base)
	}
}

func TestHTTPDirectoryFindsAKeyByItsThumbprint(t *testing.T) {
	k := newAgentKey(t, "")
	jwk := kya.JWKOf(k.priv.Public().(ed25519.PublicKey))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/.well-known/http-message-signatures-directory" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/http-message-signatures-directory+json")
		fmt.Fprintf(w, `{"keys":[{"kty":"OKP","crv":"Ed25519","x":%q}]}`, jwk.X)
	}))
	defer srv.Close()
	d := &HTTPDirectory{Client: srv.Client()}
	got, err := d.Key(context.Background(), srv.URL, k.kid)
	if err != nil || !got.Equal(k.priv.Public()) {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := d.Key(context.Background(), srv.URL, "not-a-published-kid"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("an unpublished kid: %v", err)
	}
}
