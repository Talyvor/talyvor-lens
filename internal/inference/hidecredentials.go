package inference

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/talyvor/lens/internal/redact"
)

// credentialHeaders are the request headers a provider's setAuth writes its key into.
var credentialHeaders = []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "Api-Key", "X-Goog-Api-Key"}

// credentialParams are the query parameters a provider's key rides in (Google's ?key=).
var credentialParams = []string{"key", "api_key", "api-key"}

// HideCredentials returns an upstream error body with the key req was sent with, the upstream's host and
// every key-shaped string redact.Secrets knows cut out (B28.250). A provider that echoes the key it refused
// in its 401 — or a proxy in front of it that does — would otherwise hand it to the tenant verbatim.
func HideCredentials(body []byte, req *http.Request) []byte {
	if len(body) == 0 {
		return body
	}
	s := string(body)
	if req != nil {
		for _, v := range sentCredentials(req) {
			s = strings.ReplaceAll(s, v, redact.Mark)
		}
		if req.URL != nil && req.URL.Host != "" {
			s = strings.ReplaceAll(s, req.URL.Host, "[upstream]")
		}
	}
	return []byte(redact.Secrets(s))
}

// sentCredentials lists the secret values req carries, longest first so a key is cut before a prefix of it.
func sentCredentials(req *http.Request) []string {
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if i := strings.IndexByte(v, ' '); i > 0 && !strings.Contains(v[i+1:], " ") {
			v = v[i+1:] // "Bearer sk-…", "Basic …"
		}
		if len(v) >= 8 {
			out = append(out, v)
		}
	}
	for _, h := range credentialHeaders {
		for _, v := range req.Header.Values(h) {
			add(v)
		}
	}
	if req.URL != nil {
		q := req.URL.Query()
		for _, k := range credentialParams {
			for _, v := range q[k] {
				add(v)
			}
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && len(out[j]) > len(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ClientError is the text a tenant is shown for a failed upstream call (B28.250): the URL — host, path and any
// key in its query — is replaced by "[upstream]", and any key-shaped string is cut. The error itself keeps its
// address for the operator's logs.
func ClientError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) && ue.URL != "" {
		msg = strings.ReplaceAll(msg, `"`+ue.URL+`"`, "[upstream]")
		msg = strings.ReplaceAll(msg, ue.URL, "[upstream]")
		if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
			msg = strings.ReplaceAll(msg, u.Host, "[upstream]")
			msg = strings.ReplaceAll(msg, u.Hostname(), "[upstream]")
		}
	}
	return redact.Secrets(msg)
}
