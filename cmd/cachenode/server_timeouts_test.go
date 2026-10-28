package main

import (
	"context"
	"testing"
)

// B50.14: the node's server sets every timeout — headers, read, write and idle.
func TestServerSetsEveryTimeout(t *testing.T) {
	srv, err := NewCacheServer(nil, "s", CacheNodeConfig{}).ListenAndServe(0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatalf("header %v read %v write %v idle %v — every one must be set",
			srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout)
	}
}
