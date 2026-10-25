package ap2_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/ap2"
)

// published is the key a verifier reads from the signer's JWKS, as JSON, the way it is served.
func published(t *testing.T, s *ap2.Signer) ap2.JWK {
	t.Helper()
	raw, err := json.Marshal(s.JWKS())
	if err != nil {
		t.Fatal(err)
	}
	var set ap2.JWKS
	if err := json.Unmarshal(raw, &set); err != nil || len(set.Keys) != 1 {
		t.Fatalf("JWKS = %s, %v", raw, err)
	}
	return set.Keys[0]
}

// B45.28: an owner's approval is an AP2 v0.2 closed Payment Mandate: an SD-JWT, ES256, whose disclosures are exactly the
// ones its issuer signed, verifiable with the published key alone; one changed byte, or a disclosure added, fails.
func TestMandate_VerifiesAsAnSDJWTWithThePublishedKey(t *testing.T) {
	s := ap2.NewSigner(bytes.Repeat([]byte{7}, 32), "https://lens.test")
	tok, err := s.Issue(map[string]any{"iss": s.Issuer, "vct": ap2.VCTClosed}, map[string]any{
		"payment_amount": map[string]any{"currency": "LXC", "amount": 8_000_000},
		"payee":          map[string]any{"id": "agt_vendor", "name": "vendor"},
	})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, "~")
	if !strings.HasSuffix(tok, "~") || len(parts) != 4 { // the JWS, two disclosures, and the empty end (no key binding)
		t.Fatalf("not <jws>~<disclosure>~<disclosure>~: %q", tok)
	}
	header, _ := base64.RawURLEncoding.DecodeString(strings.Split(parts[0], ".")[0])
	var h map[string]string
	_ = json.Unmarshal(header, &h)
	if h["alg"] != "ES256" || h["typ"] != ap2.TypSDJWT || h["kid"] == "" {
		t.Errorf("header = %s", header)
	}

	key := published(t, s)
	claims, err := ap2.Verify(tok, key)
	if err != nil {
		t.Fatalf("verify with the published key: %v", err)
	}
	amount, _ := claims["payment_amount"].(map[string]any)
	if claims["vct"] != ap2.VCTClosed || amount["amount"] != json.Number("8000000") || amount["currency"] != "LXC" {
		t.Errorf("claims = %v", claims)
	}
	if _, left := claims["_sd"]; left {
		t.Errorf("the digests are left in the claims: %v", claims)
	}

	// A disclosure withheld is simply not shown: still valid.
	if c, err := ap2.Verify(parts[0]+"~"+parts[1]+"~", key); err != nil || len(c) != 3 {
		t.Errorf("one disclosure withheld = %v, %v", c, err)
	}
	// One changed byte in the signed part.
	jws := []byte(parts[0])
	i := strings.Index(parts[0], ".") + 5
	jws[i] ^= 1
	if _, err := ap2.Verify(string(jws)+"~"+strings.Join(parts[1:], "~"), key); err == nil {
		t.Error("a changed payload verified")
	}
	// A disclosure nobody signed: the amount, raised.
	forged := base64.RawURLEncoding.EncodeToString([]byte(`["c2FsdA","payment_amount",{"currency":"LXC","amount":1}]`))
	if _, err := ap2.Verify(parts[0]+"~"+forged+"~", key); err == nil {
		t.Error("a disclosure the issuer did not sign verified")
	}
	// Another signer's key.
	other := ap2.NewSigner(bytes.Repeat([]byte{8}, 32), "https://lens.test")
	if _, err := ap2.Verify(tok, published(t, other)); err == nil {
		t.Error("verified with another key")
	}
}

// B45.28: an agent's approval of another agent's request is Talyvor-signed evidence, a plain JWS of its own type.
func TestEvidence_VerifiesOnlyAsItsOwnTypeAndUnchanged(t *testing.T) {
	s := ap2.NewSigner(bytes.Repeat([]byte{9}, 32), "https://lens.test")
	tok, err := s.SignJWT(ap2.TypEvidence, map[string]any{"approver": map[string]any{"agent_id": "agt_a"}})
	if err != nil {
		t.Fatal(err)
	}
	key := published(t, s)
	c, err := ap2.VerifyJWT(tok, ap2.TypEvidence, key)
	if err != nil || c["approver"].(map[string]any)["agent_id"] != "agt_a" {
		t.Fatalf("verify = %v, %v", c, err)
	}
	if _, err := ap2.VerifyJWT(tok, ap2.TypSDJWT, key); err == nil {
		t.Error("evidence verified as a mandate")
	}
	b := []byte(tok)
	b[len(b)-3] ^= 1
	if _, err := ap2.VerifyJWT(string(b), ap2.TypEvidence, key); err == nil {
		t.Error("a changed signature verified")
	}
}
