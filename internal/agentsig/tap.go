package agentsig

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// tap.go — B62.6: the agent recognition signature of Visa's Trusted Agent Protocol, as its merchant specification
// publishes it (developer.visa.com/capabilities/trusted-agent-protocol/trusted-agent-protocol-specifications): an
// RFC 9421 signature over the merchant's "@authority" and "@path", with created, expires, keyid, alg, nonce and tag
// all set, valid for at most 8 minutes, its nonce used once, its tag "agent-browser-auth" while the agent browses and
// "agent-payer-auth" when it pays. Talyvor signs one for its own agents (internal/kyatap) and checks it here; the key
// is the caller's to find, because the merchant's request never reaches Lens.

// The two tags the specification gives a trusted agent's signature.
const (
	TAPTagBrowse = "agent-browser-auth"
	TAPTagPay    = "agent-payer-auth"
)

// TAPMaxLife is the longest a signature lives, created to expires: the specification's 8 minutes.
const TAPMaxLife = 8 * time.Minute

// TAPLabel is the label Talyvor signs with: the specification's examples use sig2.
const TAPLabel = "sig2"

// TAPSignature is one signature's parameters.
type TAPSignature struct {
	Tag     string    `json:"tag"`
	KeyID   string    `json:"keyid"`
	Nonce   string    `json:"nonce"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}

// TAPKeys finds the public key a signature names. An unknown one is ErrUnknownKey.
type TAPKeys func(ctx context.Context, keyID string) (ed25519.PublicKey, error)

func tapTarget(authority, path string) (*http.Request, *url.URL, error) {
	authority = strings.ToLower(strings.TrimSpace(authority))
	if authority == "" || strings.ContainsAny(authority, "/?#@ ") {
		return nil, nil, fmt.Errorf("the authority is a host, like shop.example, not %q", authority)
	}
	if !strings.HasPrefix(path, "/") {
		return nil, nil, fmt.Errorf("the path starts with /, not %q", path)
	}
	u, err := url.Parse("https://" + authority + path)
	if err != nil {
		return nil, nil, fmt.Errorf("the path: %v", err)
	}
	return &http.Request{Method: http.MethodGet, URL: u, Header: http.Header{}}, &url.URL{Scheme: "https", Host: authority}, nil
}

// SignTAP is s's signature over authority and path with priv: the Signature-Input and Signature headers.
func SignTAP(priv ed25519.PrivateKey, authority, path string, s TAPSignature) (signatureInput, signature string, err error) {
	if s.Tag != TAPTagBrowse && s.Tag != TAPTagPay {
		return "", "", fmt.Errorf("the tag is %q or %q", TAPTagBrowse, TAPTagPay)
	}
	r, pub, err := tapTarget(authority, path)
	if err != nil {
		return "", "", err
	}
	inner := fmt.Sprintf(`("@authority" "@path");created=%d;expires=%d;keyid=%q;alg="Ed25519";nonce=%q;tag=%q`,
		s.Created.Unix(), s.Expires.Unix(), s.KeyID, s.Nonce, s.Tag)
	members, err := parseSignatureInput(TAPLabel + "=" + inner)
	if err != nil {
		return "", "", err
	}
	base, err := signatureBase(r, pub, members[0])
	if err != nil {
		return "", "", err
	}
	return TAPLabel + "=" + inner, TAPLabel + "=:" + base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(base))) + ":", nil
}

// VerifyTAP checks a Trusted Agent Protocol signature on a request to authority and path, as the specification's
// five steps do: a trusted agent's tag, every parameter there, created past and expires to come at most 8 minutes
// apart, the key found, the signature verified over the rebuilt base, and only then the nonce used once.
func VerifyTAP(ctx context.Context, authority, path, signatureInput, signature string, keys TAPKeys, nonces Nonces,
	now time.Time) (TAPSignature, error) {
	var out TAPSignature
	members, err := parseSignatureInput(signatureInput)
	if err != nil {
		return out, &Error{Reason: "Signature-Input: " + err.Error()}
	}
	var m *member
	for i := range members {
		if t := members[i].params["tag"]; t == TAPTagBrowse || t == TAPTagPay {
			m = &members[i]
			break
		}
	}
	if m == nil {
		return out, &Error{Reason: fmt.Sprintf("no signature carries tag %q or %q", TAPTagBrowse, TAPTagPay)}
	}
	for _, p := range []string{"created", "expires", "keyid", "alg", "nonce"} {
		if m.params[p] == "" {
			return out, &Error{Reason: "the signature has no " + p}
		}
	}
	if alg := m.params["alg"]; alg != "Ed25519" && alg != "ed25519" {
		return out, &Error{Reason: fmt.Sprintf("alg %q is not supported; sign with Ed25519", alg)}
	}
	var authorityCovered, pathCovered bool
	for _, c := range m.components {
		switch c.name {
		case "@authority":
			authorityCovered = true
		case "@path":
			pathCovered = true
		default:
			return out, &Error{Reason: "a Trusted Agent Protocol signature covers @authority and @path, not " + c.raw}
		}
	}
	if !authorityCovered || !pathCovered {
		return out, &Error{Reason: "a Trusted Agent Protocol signature covers @authority and @path"}
	}
	created, err1 := strconv.ParseInt(m.params["created"], 10, 64)
	expires, err2 := strconv.ParseInt(m.params["expires"], 10, 64)
	if err1 != nil || err2 != nil {
		return out, &Error{Reason: "created and expires are integers"}
	}
	out = TAPSignature{Tag: m.params["tag"], KeyID: m.params["keyid"], Nonce: m.params["nonce"],
		Created: time.Unix(created, 0).UTC(), Expires: time.Unix(expires, 0).UTC()}
	switch {
	case out.Created.After(now):
		return out, &Error{Reason: "the signature was created in the future"}
	case !now.Before(out.Expires):
		return out, &Error{Reason: "the signature expired at " + out.Expires.Format(time.RFC3339)}
	case out.Expires.Sub(out.Created) > TAPMaxLife:
		return out, &Error{Reason: "a Trusted Agent Protocol signature lives at most 8 minutes"}
	}
	pub, err := keys(ctx, out.KeyID)
	if errors.Is(err, ErrUnknownKey) {
		return out, &Error{Reason: "key " + out.KeyID + " is not known"}
	} else if err != nil {
		return out, err
	}
	r, u, err := tapTarget(authority, path)
	if err != nil {
		return out, &Error{Reason: err.Error()}
	}
	base, err := signatureBase(r, u, *m)
	if err != nil {
		return out, &Error{Reason: err.Error()}
	}
	raw, err := signatureOf(signature, m.label)
	if err != nil {
		return out, &Error{Reason: "Signature: " + err.Error()}
	}
	if !ed25519.Verify(pub, []byte(base), raw) {
		return out, &Error{Reason: "the signature does not verify"}
	}
	fresh, err := nonces.Use(ctx, out.KeyID, out.Nonce, out.Expires)
	if err != nil {
		return out, err
	}
	if !fresh {
		return out, &Error{Reason: "the nonce was already used"}
	}
	return out, nil
}

// CheckTAPTarget refuses an authority that is not a host or a path that does not start with /.
func CheckTAPTarget(authority, path string) error {
	_, _, err := tapTarget(authority, path)
	return err
}
