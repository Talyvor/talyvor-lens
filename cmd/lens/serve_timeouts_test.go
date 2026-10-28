package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/talyvor/lens/internal/config"
)

// B50.2: the server Lens listens with has every timeout set — headers, read, write and idle.
func TestLensServerSetsEveryTimeout(t *testing.T) {
	set, _ := startPlainHTTP(&config.Config{ListenAddr: "127.0.0.1:0"}, http.NotFoundHandler(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = set.shutdown(context.Background()) })
	for _, srv := range []*http.Server{set.main, newHTTPServer(":443", http.NotFoundHandler(), proxyWriteTimeout)} {
		if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 {
			t.Fatalf("server %s: header %v read %v write %v idle %v — every one must be set",
				srv.Addr, srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout)
		}
	}
}
