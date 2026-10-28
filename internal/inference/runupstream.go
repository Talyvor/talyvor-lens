// Package inference holds the provider-calling LEAF of the Lens gateway — the upstream round-trip and
// the per-provider request/response translation + signing + usage parsing. It was extracted from
// internal/proxy (PR-3b, step A′) so a background scorer (proof-of-routing-prediction, step c) can run a
// model on an input WITHOUT importing the serve handler. It imports NO internal/proxy (cycle-free) and
// reaches no ledger/mint path.
//
// This is the minimal (A′) cut: only the round-trip (RunUpstream) + the leaf helpers (gemini/bedrock
// translate, AWS signing, usage extraction) moved. The proxy KEEPS its providerConfig type,
// configForProvider, applyKey, and the serve hot path UNCHANGED — its closures now call into here.
package inference

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/talyvor/lens/internal/retry"
)

// RunUpstream performs the upstream provider round-trip — MOVED VERBATIM from proxy.forward's body
// (header-copy skipping Host, then setAuth which OVERWRITES any forwarded Authorization, then otel
// inject, wrapped in retry.Do over httpClient.Do). The two characterization tests in package proxy
// (forward_authheaders_test.go, forward_retry_test.go) pin exactly this behavior; they stay UNEDITED and
// must remain green, so this body is byte-for-byte the original round-trip.
//
// The caller (proxy.forward) keeps its deferred metrics.RecordUpstream and passes the already-resolved
// upstreamURL + the provider's setAuth closure + the inbound request headers (extraHeaders) — so the
// ProviderConfig type does NOT move (the A′ point). A scorer passes nil extraHeaders (no inbound request).
func RunUpstream(ctx context.Context, httpClient *http.Client, rc retry.Config, upstreamURL string, setAuth func(*http.Request), body []byte, extraHeaders http.Header) (resp *http.Response, respBody []byte, attempts int, err error) {
	result := retry.Do(ctx, rc, func(c context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(c, http.MethodPost, upstreamURL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("build upstream request: %w", err)
		}
		for name, values := range extraHeaders {
			// Skip Host (rewritten per-upstream). Skip Accept-Encoding too: forwarding the client's
			// Accept-Encoding disables Go's transport transparent gzip decoding, so resp.Body stays
			// compressed while the proxy re-serves it as application/json with no Content-Encoding —
			// header/body mismatch. Dropping it lets the transport request + transparently decode gzip,
			// so respBody is always the plain body we cache, score, and hand back.
			if strings.EqualFold(name, "Host") || strings.EqualFold(name, "Accept-Encoding") {
				continue
			}
			for _, v := range values {
				req.Header.Add(name, v)
			}
		}
		setAuth(req)
		// Inject our current span context as traceparent on the upstream request (harmless metadata until
		// the provider surfaces OTel itself).
		otel.GetTextMapPropagator().Inject(c, propagation.HeaderCarrier(req.Header))
		return httpClient.Do(req)
	})
	if result.LastError != nil {
		return nil, nil, result.Attempts, HideURLQuery(result.LastError)
	}
	resp = result.Response
	defer resp.Body.Close()

	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, result.Attempts, fmt.Errorf("read upstream response: %w", err)
	}
	return resp, respBody, result.Attempts, nil
}

// HideURLQuery drops the query from a *url.Error's URL, in place. Google's key rides in ?key=, and
// http.Client writes the whole URL into the error text the proxy hands its caller (B50.3).
func HideURLQuery(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if i := strings.IndexByte(ue.URL, '?'); i >= 0 {
			ue.URL = ue.URL[:i] + "?…"
		}
	}
	return err
}
