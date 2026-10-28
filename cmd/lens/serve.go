package main

// serve.go — TLS-aware HTTP server startup + graceful shutdown.
//
// Two modes:
//
//   Plain HTTP (TLSDomain == "")
//     Binds on cfg.ListenAddr (default 0.0.0.0:8080).
//     Intended for local development and deployments where TLS is
//     terminated upstream (nginx, Cloudflare, load balancer).
//
//   HTTPS via Let's Encrypt (TLSDomain != "")
//     Binds on :443 with a certificate provisioned automatically by
//     Let's Encrypt.  A second server on :80 handles ACME HTTP-01
//     challenges and issues a 301 redirect for every other request.
//     Certificates are cached in TLSCacheDir across restarts.
//
// TLS configuration:
//   - Minimum version: TLS 1.2 (TLS 1.0 and 1.1 are broken and rejected).
//   - TLS 1.3 is used whenever the client supports it; Go's crypto/tls
//     manages TLS 1.3 cipher suites automatically — they cannot and should
//     not be overridden.
//   - TLS 1.2 is restricted to ECDHE + AEAD suites (GCM-SHA256/384 and
//     ChaCha20-Poly1305).  This excludes CBC, RC4, 3DES, and static RSA.
//   - Curve preferences: X25519 first (fastest + most secure), then P-256.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/talyvor/lens/internal/config"
)

// Server timeout constants.
//
// ReadHeaderTimeout — applies to all three servers.  10 s is generous for
// any well-behaved client sending headers; keeps Slowloris (header-side)
// from pinning goroutines.
//
// proxyWriteTimeout — main proxy server only.  Streaming LLM responses can
// legitimately hold a connection open for 30 s–2 min; 5 min gives headroom
// while still evicting truly stuck connections.
//
// shortWriteTimeout — redirect server and plain-HTTP server.  Neither ever
// streams; a 30 s write cap is more than sufficient.
//
// serverIdleTimeout — applied to all three servers.  After 2 min of
// keep-alive inactivity the server closes the connection, reclaiming the
// goroutine from genuinely idle clients.
const (
	serverReadHeaderTimeout = 10 * time.Second
	proxyWriteTimeout       = 5 * time.Minute
	shortWriteTimeout       = 30 * time.Second
	serverIdleTimeout       = 120 * time.Second
)

// newHTTPServer sets every server timeout (B50.2). Reading a request may take as long as answering it:
// WriteTimeout already runs from the end of the headers, so ReadTimeout = write bounds a slow body without
// cutting anything the write timeout allowed.
func newHTTPServer(addr string, h http.Handler, write time.Duration) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		ReadTimeout:       write,
		WriteTimeout:      write,
		IdleTimeout:       serverIdleTimeout,
	}
}

// serverSet holds the live server(s) so the caller can shut them all down.
type serverSet struct {
	main     *http.Server
	redirect *http.Server // non-nil in TLS mode only
}

// shutdown stops both servers. The redirect server is stateless (it only
// issues 301s) so it gets a short bounded context — capped at 5 s — rather
// than context.Background(). This ensures a slow port-80 connection cannot
// consume time from the main server's drain window (drainCtx).
func (s *serverSet) shutdown(drainCtx context.Context) error {
	if s.redirect != nil {
		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer rcancel()
		_ = s.redirect.Shutdown(rctx)
	}
	return s.main.Shutdown(drainCtx)
}

// startServers builds and launches the correct server set for the given
// configuration, returning a handle for shutdown and a channel that receives
// at most one fatal startup error.
//
// The error channel is closed (without a value) on clean server exit
// (http.ErrServerClosed), so the caller can use it in a select alongside
// ctx.Done() without needing a nil-check on the read value.
func startServers(cfg *config.Config, handler http.Handler, logger *slog.Logger) (*serverSet, <-chan error) {
	if cfg.TLSDomain == "" {
		return startPlainHTTP(cfg, handler, logger)
	}
	return startTLS(cfg, handler, logger)
}

// ── plain HTTP ────────────────────────────────────────────────────────────────

func startPlainHTTP(cfg *config.Config, handler http.Handler, logger *slog.Logger) (*serverSet, <-chan error) {
	srv := newHTTPServer(cfg.ListenAddr, handler, shortWriteTimeout)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server listening (HTTP — set LENS_TLS_DOMAIN to enable HTTPS)",
			slog.String("addr", cfg.ListenAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	return &serverSet{main: srv}, errCh
}

// ── TLS / Let's Encrypt ───────────────────────────────────────────────────────

func startTLS(cfg *config.Config, handler http.Handler, logger *slog.Logger) (*serverSet, <-chan error) {
	certManager := &autocert.Manager{
		// AcceptTOS must be set — Let's Encrypt requires accepting their ToS.
		Prompt: autocert.AcceptTOS,

		// HostWhitelist restricts cert provisioning to the configured domain.
		// Without this, autocert would accept any domain, making it possible
		// for an attacker to trigger cert requests for arbitrary hostnames and
		// burn your Let's Encrypt rate-limit quota.
		HostPolicy: autocert.HostWhitelist(cfg.TLSDomain),

		// DirCache persists the certificate and private key to disk so a
		// restart doesn't trigger a new ACME round-trip.  The directory must
		// be writable by the Lens process and should be backed by durable
		// storage (not a tmpfs/ramdisk).
		Cache: autocert.DirCache(cfg.TLSCacheDir),
	}

	tlsCfg := &tls.Config{
		// GetCertificate resolves the Let's Encrypt certificate at TLS
		// handshake time, serving the cached cert or triggering ACME if none
		// is cached yet.
		GetCertificate: certManager.GetCertificate,

		// Reject TLS 1.0 and 1.1 — both are deprecated (RFC 8996) and have
		// known protocol-level weaknesses (BEAST, POODLE, etc.).
		MinVersion: tls.VersionTLS12,

		// Prefer X25519 (fastest, 128-bit equivalent security) then P-256.
		// This controls the key-agreement step of the TLS handshake.
		CurvePreferences: []tls.CurveID{
			tls.X25519,
			tls.CurveP256,
		},

		// Restrict TLS 1.2 to ECDHE + AEAD suites only.
		// TLS 1.3 cipher suites are managed automatically by Go's crypto/tls
		// and are not configurable here — they are always strong.
		// CBC-mode suites are excluded (BEAST/Lucky-13 mitigations).
		// RSA key-exchange suites are excluded (no forward secrecy).
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		},
	}

	// WriteTimeout is proxyWriteTimeout (5 min) rather than shortWriteTimeout
	// because this server proxies streaming LLM responses which can
	// legitimately hold a connection for minutes.
	mainSrv := newHTTPServer(":443", hstsMiddleware(handler), proxyWriteTimeout)
	mainSrv.TLSConfig = tlsCfg

	// Port 80 serves two purposes:
	//   1. Let's Encrypt ACME HTTP-01 challenge (/.well-known/acme-challenge/…)
	//      — autocert.Manager.HTTPHandler handles this automatically.
	//   2. Permanent redirect for all other requests to the HTTPS equivalent.
	// The ACME challenge must remain reachable on port 80 even after initial
	// provisioning because Let's Encrypt renews certificates every 90 days.
	redirectSrv := newHTTPServer(":80", certManager.HTTPHandler(httpsRedirectHandler(cfg.TLSDomain)), shortWriteTimeout)

	// errCh is declared before the redirect goroutine so the goroutine can
	// send a fatal error if port 80 is unavailable on a fresh deployment.
	errCh := make(chan error, 1)

	go func() {
		logger.Info("http redirect server listening", slog.String("addr", ":80"))
		if err := redirectSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			if certIsCached(cfg.TLSCacheDir, cfg.TLSDomain) {
				// Cert is already on disk — TLS handshakes will continue to
				// succeed from the cache.  Port 80 will be needed again at the
				// 90-day renewal; the operator has time to fix it.
				logger.Warn("http redirect server error (cert cached — TLS still works; fix port 80 before renewal)",
					slog.String("err", err.Error()))
			} else {
				// Fresh deployment — no cert on disk.  autocert's ACME HTTP-01
				// challenge requires port 80; without it every incoming TLS
				// handshake will fail.  Treat this as fatal so the operator
				// sees a clear error at startup rather than confusing handshake
				// failures on port 443.
				logger.Error("http redirect server failed on fresh deployment — ACME HTTP-01 needs port 80 to provision the first cert; TLS will not work",
					slog.String("err", err.Error()))
				errCh <- fmt.Errorf("redirect server: port 80 bind failed with no cached cert (ACME HTTP-01 provisioning impossible): %w", err)
			}
		}
	}()

	go func() {
		logger.Info("server listening (TLS / Let's Encrypt)",
			slog.String("addr", ":443"),
			slog.String("domain", cfg.TLSDomain),
			slog.String("cert_cache", cfg.TLSCacheDir),
		)
		// Empty cert/key paths: TLSConfig.GetCertificate supplies the cert.
		if err := mainSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	return &serverSet{main: mainSrv, redirect: redirectSrv}, errCh
}

// httpsRedirectHandler issues a 301 Moved Permanently redirect from the
// HTTP URL to its HTTPS equivalent on domain, preserving the path and query string.
// The target host is always the configured TLS domain, never the request's Host
// header: a forged Host must not turn port 80 into an open redirect.
func httpsRedirectHandler(domain string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+domain+r.URL.RequestURI(), http.StatusMovedPermanently) // nosemgrep: go.lang.security.injection.open-redirect.open-redirect -- the host is the configured TLS domain; only the path comes from the request
	})
}

// certIsCached reports whether autocert has already written a certificate for
// domain into dir.  autocert.DirCache stores each cert under the hostname as
// the filename (e.g. dir/example.com).  This is used to distinguish a fresh
// deployment (no cert yet — port 80 is critical for ACME HTTP-01) from a
// restarted one (cert on disk — port 80 is only needed for renewal).
func certIsCached(dir, domain string) bool {
	_, err := os.Stat(filepath.Join(dir, domain))
	return err == nil
}

// checkTLSCacheDir verifies that dir exists (creating it if needed) and is
// writable by the current process.  It must be called before the autocert
// manager is initialised so a misconfigured cache is caught at startup rather
// than silently at first handshake time.
//
// Why this matters: autocert.DirCache defers os.MkdirAll until the first
// cert write, which happens at the TLS handshake.  A non-writable location
// (e.g. a read-only filesystem in a locked-down container) causes every cert
// fetch to fail, and each restart triggers a fresh ACME round-trip that burns
// Let's Encrypt rate-limit quota (5 certs/domain/week).  Failing fast here is
// cheaper than debugging a LE rate-limit hit in production.
func checkTLSCacheDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("tls cache dir %q: cannot create: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".lens-tls-probe-*")
	if err != nil {
		return fmt.Errorf("tls cache dir %q is not writable: %w", dir, err)
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return nil
}

// hstsMiddleware wraps a handler and stamps Strict-Transport-Security on
// every response.  It is applied only to the TLS server (port 443) —
// never to the plain-HTTP redirect server — so browsers only pin the
// header over a connection they already know is secure.
//
// max-age=63072000 = 2 years (the recommended minimum for HSTS preloading).
// includeSubDomains covers any sub-domain served by the same instance.
// preload is intentionally omitted until the domain is submitted to the
// HSTS preload list (https://hstspreload.org) — it cannot be undone easily.
func hstsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}
