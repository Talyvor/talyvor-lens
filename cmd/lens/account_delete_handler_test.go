package main

import (
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// B52.37 — a replica that deletes an account tells every other one, which drops its cached keys of it.
func TestB5237_AccountDeletionReachesEveryReplica(t *testing.T) {
	srv, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	connect := func() *nats.Conn {
		nc, err := nats.Connect(srv.ClientURL())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(nc.Close)
		return nc
	}
	here, there := connect(), connect()
	evicted := make(chan string, 1)
	if err := subscribeAccountDeletions(there, func(ws string) { evicted <- ws }); err != nil {
		t.Fatal(err)
	}
	if err := there.Flush(); err != nil {
		t.Fatal(err)
	}
	var local string
	evictEverywhere(func(ws string) { local = ws }, here)("u-deleted")
	if local != "u-deleted" {
		t.Fatalf("this replica evicted %q", local)
	}
	select {
	case ws := <-evicted:
		if ws != "u-deleted" {
			t.Fatalf("the other replica evicted %q", ws)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the other replica was never told")
	}
}
