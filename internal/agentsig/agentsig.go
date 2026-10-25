// Package agentsig checks the HTTP message signatures (RFC 9421) outside agents sign their requests to Talyvor with
// (B45.24, docs/agent-signatures.md): Web Bot Auth on the agent-facing endpoints, and Talyvor's own stricter profile
// on money endpoints. Every request comes out at one of three trust levels. An unsigned request is never refused here
// — it is an "Unverified agent", and what that may do is the endpoint's decision — but a signature that is present
// and does not hold is refused.
package agentsig

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Level is how far Talyvor trusts who is asking.
type Level string

const (
	// Verified — the signature holds and its domain belongs to a business Talyvor has checked.
	Verified Level = "verified"
	// Signed — the signature holds: the domain published the key, and that is all it proves.
	Signed Level = "signed"
	// Unverified — no signature.
	Unverified Level = "unverified"
)

// Profile is the set of rules a signature must meet.
type Profile int

const (
	// WebBotAuth is the IETF Web Bot Auth draft: tag "web-bot-auth", the authority (or the full URL) and the
	// Signature-Agent signed, an expiry of at most 24 hours.
	WebBotAuth Profile = iota
	// Money is Talyvor's profile for money endpoints: the method, the full URL, the body's digest and the
	// Signature-Agent signed, valid for at most 5 minutes, with a nonce that is accepted once.
	Money
)

func (p Profile) String() string {
	if p == Money {
		return "money"
	}
	return "web-bot-auth"
}

const (
	webBotAuthMaxLife = 24 * time.Hour  // the Web Bot Auth draft's recommended ceiling
	moneyMaxLife      = 5 * time.Minute // B45.24: a money signature expires within 5 minutes
)

// Result is who Talyvor takes the caller to be.
type Result struct {
	Level    Level  `json:"level"`
	Label    string `json:"label"` // the words an approval card shows
	Domain   string `json:"domain,omitempty"`
	Business string `json:"business,omitempty"`
	KeyID    string `json:"key_id,omitempty"`
	Profile  string `json:"profile"`
}

// Error is a signature that is present and does not hold: the caller is refused, and Reason says why.
type Error struct{ Reason string }

func (e *Error) Error() string { return "agent signature refused: " + e.Reason }

func refuse(format string, a ...any) (Result, error) {
	return Result{}, &Error{Reason: fmt.Sprintf(format, a...)}
}

// ErrUnknownKey is a key id the agent's directory does not publish.
var ErrUnknownKey = errors.New("the key is not in the agent's directory")

// Directory finds a key an agent domain publishes, by its RFC 7638 thumbprint.
type Directory interface {
	Key(ctx context.Context, origin, keyID string) (ed25519.PublicKey, error)
}

// Nonces records a nonce until the signature carrying it expires; fresh=false means it was seen before.
type Nonces interface {
	Use(ctx context.Context, keyID, nonce string, until time.Time) (fresh bool, err error)
}

// Businesses names the business Talyvor has checked for a signing domain.
type Businesses interface {
	Business(ctx context.Context, domain string) (name string, ok bool, err error)
}

// Verifier checks a request's signature.
type Verifier struct {
	// PublicURL is where the world reaches Lens: @target-uri and @authority are computed from it, never from the
	// request's Host or X-Forwarded-* headers, which the caller controls.
	PublicURL  *url.URL
	Keys       Directory
	Nonces     Nonces
	Businesses Businesses
	Now        func() time.Time
}

// Check answers the caller's trust level under profile p. body is the request's body, already read.
func (v *Verifier) Check(ctx context.Context, r *http.Request, body []byte, p Profile) (Result, error) {
	in, sig := strings.Join(r.Header.Values("Signature-Input"), ", "), strings.Join(r.Header.Values("Signature"), ", ")
	if in == "" && sig == "" {
		return Result{Level: Unverified, Label: "Unverified agent", Profile: p.String()}, nil
	}
	if in == "" || sig == "" {
		return refuse("Signature and Signature-Input come together")
	}
	members, err := parseSignatureInput(in)
	if err != nil {
		return refuse("Signature-Input: %v", err)
	}
	var m *member
	for i := range members {
		if members[i].params["tag"] == "web-bot-auth" {
			m = &members[i]
			break
		}
	}
	if m == nil {
		return refuse(`no signature carries tag="web-bot-auth"`)
	}
	if alg, ok := m.params["alg"]; ok && alg != "ed25519" {
		return refuse("alg %q is not supported; sign with ed25519", alg)
	}
	keyID := m.params["keyid"]
	if keyID == "" {
		return refuse("the signature names no keyid")
	}
	created, err1 := strconv.ParseInt(m.params["created"], 10, 64)
	expires, err2 := strconv.ParseInt(m.params["expires"], 10, 64)
	if err1 != nil || err2 != nil {
		return refuse("the signature needs integer created and expires")
	}
	now, exp := v.Now(), time.Unix(expires, 0)
	maxLife := webBotAuthMaxLife
	if p == Money {
		maxLife = moneyMaxLife
	}
	switch {
	case !now.Before(exp):
		return refuse("the signature expired at %s", exp.UTC().Format(time.RFC3339))
	case expires < created:
		return refuse("the signature expires before it was created")
	case time.Duration(expires-created)*time.Second > maxLife || exp.Sub(now) > maxLife:
		return refuse("a %s signature is valid for at most %s", p, maxLife)
	}
	nonce := m.params["nonce"]
	if p == Money && nonce == "" {
		return refuse("a money signature carries a nonce")
	}

	covered := map[string]component{}
	for _, c := range m.components {
		covered[c.name] = c
	}
	need := []string{"signature-agent"}
	if p == Money {
		need = append(need, "@method", "@target-uri", "content-digest")
	} else if _, ok := covered["@target-uri"]; !ok {
		need = append(need, "@authority")
	}
	for _, n := range need {
		if _, ok := covered[n]; !ok {
			return refuse("a %s signature covers %q", p, n)
		}
	}
	origin, domain, err := agentOrigin(r.Header.Values("Signature-Agent"), covered["signature-agent"])
	if err != nil {
		return refuse("Signature-Agent: %v", err)
	}
	if _, ok := covered["content-digest"]; ok {
		if err := checkDigest(r.Header.Get("Content-Digest"), body); err != nil {
			return refuse("Content-Digest: %v", err)
		}
	}
	pub, err := v.Keys.Key(ctx, origin, keyID)
	if err != nil {
		return refuse("key %s from %s: %v", keyID, domain, err)
	}
	base, err := signatureBase(r, v.PublicURL, *m)
	if err != nil {
		return refuse("%v", err)
	}
	raw, err := signatureOf(sig, m.label)
	if err != nil {
		return refuse("Signature: %v", err)
	}
	if !ed25519.Verify(pub, []byte(base), raw) {
		return refuse("the signature does not verify")
	}
	// Only after the signature holds, so nobody can spend another agent's nonces with forged requests.
	if nonce != "" {
		fresh, err := v.Nonces.Use(ctx, keyID, nonce, exp)
		if err != nil {
			return Result{}, err
		}
		if !fresh {
			return refuse("nonce %q was already used", nonce)
		}
	}
	res := Result{Level: Signed, Label: "Signed by " + domain + " (business not checked)", Domain: domain, KeyID: keyID, Profile: p.String()}
	// shortcut: only businesses Talyvor has checked count; a card network's agent directory joins when a partner
	// provides one through internal/partners.
	name, ok, err := v.Businesses.Business(ctx, domain)
	if err != nil {
		return Result{}, err
	}
	if ok {
		res.Level, res.Label, res.Business = Verified, "Verified agent of "+name, name
	}
	return res, nil
}

type ctxKey struct{}

// FromContext is the trust level Middleware found for this request.
func FromContext(ctx context.Context) (Result, bool) {
	r, ok := ctx.Value(ctxKey{}).(Result)
	return r, ok
}

const maxSignedBody = 1 << 20

// Middleware checks every request under p: a refused signature is 401 with its reason; otherwise the request goes
// on with its Result in the context and in the Talyvor-Agent-Trust response header.
func (v *Verifier) Middleware(p Profile) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body []byte
			if r.Body != nil && (r.Header.Get("Signature") != "" || r.Header.Get("Signature-Input") != "") {
				b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSignedBody))
				if err != nil {
					writeErr(w, http.StatusRequestEntityTooLarge, "a signed body is at most 1 MiB")
					return
				}
				body, r.Body = b, io.NopCloser(bytes.NewReader(b))
			}
			res, err := v.Check(r.Context(), r, body, p)
			var e *Error
			switch {
			case errors.As(err, &e):
				writeErr(w, http.StatusUnauthorized, e.Error())
				return
			case err != nil:
				writeErr(w, http.StatusInternalServerError, "could not check the agent's signature")
				return
			}
			w.Header().Set("Talyvor-Agent-Trust", string(res.Level))
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, res)))
		})
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ─── RFC 9421 ────────────────────────────────────────────────────────────────────────────────────────────────────

// component is one covered component: its identifier as the signer wrote it, its name, and its key parameter.
type component struct{ raw, name, key string }

// member is one signature in Signature-Input: label, the inner list as sent (it is the @signature-params line),
// the covered components and the parameters, string values unquoted.
type member struct {
	label      string
	raw        string
	components []component
	params     map[string]string
}

// splitTop splits s at sep outside quoted strings and parentheses.
func splitTop(s string, sep byte) []string {
	var out []string
	depth, quoted, start := 0, false, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quoted && c == '\\':
			i++
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == sep && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// unquote reads an RFC 8941 string.
func unquote(s string) (string, error) {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", fmt.Errorf("%q is not a quoted string", s)
	}
	var b strings.Builder
	for i := 1; i < len(s)-1; i++ {
		if s[i] == '\\' {
			i++
			if i == len(s)-1 || (s[i] != '"' && s[i] != '\\') {
				return "", fmt.Errorf("bad escape in %q", s)
			}
		} else if s[i] == '"' {
			return "", fmt.Errorf("stray quote in %q", s)
		}
		b.WriteByte(s[i])
	}
	return b.String(), nil
}

func parseParams(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range splitTop(s, ';')[1:] {
		k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
		if k == "" {
			return nil, errors.New("an empty parameter")
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("parameter %s twice", k)
		}
		if strings.HasPrefix(v, `"`) {
			u, err := unquote(v)
			if err != nil {
				return nil, err
			}
			v = u
		}
		out[k] = v
	}
	return out, nil
}

func parseSignatureInput(s string) ([]member, error) {
	var out []member
	for _, part := range splitTop(s, ',') {
		label, rest, ok := strings.Cut(strings.TrimSpace(part), "=")
		rest = strings.TrimSpace(rest)
		if !ok || label == "" || !strings.HasPrefix(rest, "(") {
			return nil, fmt.Errorf("%q is not label=(components);params", part)
		}
		// The parameters may hold a quoted ')' — find the list's close outside quotes.
		end := -1
		for i, quoted := 0, false; i < len(rest); i++ {
			if quoted && rest[i] == '\\' {
				i++
			} else if rest[i] == '"' {
				quoted = !quoted
			} else if !quoted && rest[i] == ')' {
				end = i
				break
			}
		}
		if end < 0 {
			return nil, fmt.Errorf("unclosed component list in %q", part)
		}
		m := member{label: label, raw: rest}
		seen := map[string]bool{}
		for _, item := range strings.Fields(rest[1:end]) {
			nameRaw, cp, _ := strings.Cut(item, ";")
			name, err := unquote(nameRaw)
			if err != nil || name == "" || name != strings.ToLower(name) {
				return nil, fmt.Errorf("component %s is not a lowercase quoted name", item)
			}
			c := component{raw: item, name: name}
			if cp != "" {
				ps, err := parseParams(";" + cp)
				if err != nil {
					return nil, err
				}
				for k := range ps {
					if k != "key" {
						return nil, fmt.Errorf("component parameter %s is not supported", k)
					}
				}
				c.key = ps["key"]
			}
			if seen[item] {
				return nil, fmt.Errorf("component %s twice", item)
			}
			seen[item] = true
			m.components = append(m.components, c)
		}
		params, err := parseParams(rest[end+1:])
		if err != nil {
			return nil, err
		}
		m.params = params
		out = append(out, m)
	}
	return out, nil
}

// dictMember is the raw value of key in an RFC 8941 dictionary header.
func dictMember(header, key string) (string, bool) {
	for _, part := range splitTop(header, ',') {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		if k == key {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

func signatureOf(header, label string) ([]byte, error) {
	v, ok := dictMember(header, label)
	if !ok || len(v) < 2 || v[0] != ':' || v[len(v)-1] != ':' {
		return nil, fmt.Errorf("no :byte sequence: for %s", label)
	}
	return base64.StdEncoding.DecodeString(v[1 : len(v)-1])
}

// signatureBase is RFC 9421 §2.5's signature base for m over r, as reached at pub.
func signatureBase(r *http.Request, pub *url.URL, m member) (string, error) {
	var b strings.Builder
	for _, c := range m.components {
		var v string
		switch c.name {
		case "@method":
			v = r.Method
		case "@target-uri":
			v = pub.Scheme + "://" + pub.Host + r.URL.RequestURI()
		case "@authority":
			v = strings.ToLower(pub.Host)
		case "@scheme":
			v = pub.Scheme
		case "@path":
			v = r.URL.EscapedPath()
			if v == "" {
				v = "/"
			}
		case "@query":
			v = "?" + r.URL.RawQuery
		default:
			if strings.HasPrefix(c.name, "@") {
				return "", fmt.Errorf("component %s is not supported", c.name)
			}
			vals := r.Header.Values(c.name)
			if len(vals) == 0 {
				return "", fmt.Errorf("the signed header %s is missing", c.name)
			}
			for i := range vals {
				vals[i] = strings.TrimSpace(vals[i])
			}
			v = strings.Join(vals, ", ")
			if c.key != "" {
				dv, ok := dictMember(v, c.key)
				if !ok {
					return "", fmt.Errorf("the signed header %s has no member %s", c.name, c.key)
				}
				v = dv
			}
		}
		b.WriteString(c.raw + ": " + v + "\n")
	}
	b.WriteString(`"@signature-params": ` + m.raw)
	return b.String(), nil
}

// agentOrigin reads the Signature-Agent header — an RFC 8941 string, or with ;key=, that member of a dictionary —
// as an https origin, and its host as the signing domain.
func agentOrigin(vals []string, c component) (origin, domain string, err error) {
	v := strings.TrimSpace(strings.Join(vals, ", "))
	if v == "" {
		return "", "", errors.New("missing: it names the agent's key directory")
	}
	if c.key != "" {
		var ok bool
		if v, ok = dictMember(v, c.key); !ok {
			return "", "", fmt.Errorf("no member %s", c.key)
		}
	}
	if s, err := unquote(v); err == nil {
		v = s
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return "", "", fmt.Errorf("%q is not an https URL", v)
	}
	return "https://" + strings.ToLower(u.Host), strings.ToLower(u.Hostname()), nil
}

// checkDigest holds the body to its RFC 9530 Content-Digest: every sha-256 or sha-512 member must match, and at
// least one must be there.
func checkDigest(header string, body []byte) error {
	s256, s512 := sha256.Sum256(body), sha512.Sum512(body)
	sums := map[string][]byte{"sha-256": s256[:], "sha-512": s512[:]}
	matched := false
	for alg, sum := range sums {
		v, ok := dictMember(header, alg)
		if !ok {
			continue
		}
		if len(v) < 2 || v[0] != ':' || v[len(v)-1] != ':' {
			return fmt.Errorf("%s is not a :byte sequence:", alg)
		}
		got, err := base64.StdEncoding.DecodeString(v[1 : len(v)-1])
		if err != nil || subtle.ConstantTimeCompare(got, sum) != 1 {
			return errors.New("the body does not match its digest")
		}
		matched = true
	}
	if !matched {
		return errors.New("needs sha-256 or sha-512")
	}
	return nil
}

// ─── the agent's key directory ───────────────────────────────────────────────────────────────────────────────────

// HTTPDirectory reads https://<agent>/.well-known/http-message-signatures-directory, a JWKS of Ed25519 keys, and
// finds a key by its RFC 7638 thumbprint. Client must be safehttp's in production: the origin is the caller's.
type HTTPDirectory struct {
	Client *http.Client

	mu    sync.Mutex
	cache map[string]dirEntry
}

type dirEntry struct {
	keys map[string]ed25519.PublicKey
	at   time.Time
}

// shortcut: a directory is cached for 5 minutes and a newly published key waits that long; the cache is cleared
// when it holds 1024 origins. Upgrade to Cache-Control-driven expiry when an agent needs faster rotation.
const (
	dirTTL     = 5 * time.Minute
	dirMaxSize = 1024
)

func (d *HTTPDirectory) Key(ctx context.Context, origin, keyID string) (ed25519.PublicKey, error) {
	d.mu.Lock()
	e, ok := d.cache[origin]
	d.mu.Unlock()
	if !ok || time.Since(e.at) > dirTTL {
		keys, err := d.fetch(ctx, origin)
		if err != nil {
			return nil, err
		}
		e = dirEntry{keys: keys, at: time.Now()}
		d.mu.Lock()
		if d.cache == nil || len(d.cache) >= dirMaxSize {
			d.cache = map[string]dirEntry{}
		}
		d.cache[origin] = e
		d.mu.Unlock()
	}
	if k, ok := e.keys[keyID]; ok {
		return k, nil
	}
	return nil, ErrUnknownKey
}

func (d *HTTPDirectory) fetch(ctx context.Context, origin string) (map[string]ed25519.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/.well-known/http-message-signatures-directory", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/http-message-signatures-directory+json, application/json")
	resp, err := d.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not read the key directory: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the key directory answered %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct{ Kty, Crv, X string } `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&set); err != nil {
		return nil, fmt.Errorf("the key directory is not a JWKS: %w", err)
	}
	keys := map[string]ed25519.PublicKey{}
	for _, k := range set.Keys {
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if k.Kty != "OKP" || k.Crv != "Ed25519" || err != nil || len(x) != ed25519.PublicKeySize {
			continue
		}
		keys[thumbprint(x)] = ed25519.PublicKey(x)
	}
	return keys, nil
}

// thumbprint is the RFC 7638 JWK thumbprint of an Ed25519 key, the keyid Web Bot Auth signs with.
func thumbprint(x []byte) string {
	sum := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + base64.RawURLEncoding.EncodeToString(x) + `"}`))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
