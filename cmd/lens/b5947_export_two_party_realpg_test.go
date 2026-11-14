package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/support"
)

// B59.47 — a payment between two workspaces is in both parties' exports, and neither export names the other's
// workspace id: the other party is its display name, as its own statement names it. A third workspace's export holds
// none of it, and a table with two workspaces that is not a payment both see stays out of both.
func TestB5947_TheExportCarriesTwoPartyPaymentsWithTheOtherPartyByName(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	stamp := time.Now().Format("150405.000000")
	wsA, wsB, wsC := "ws-b5947-a-"+stamp, "ws-b5947-b-"+stamp, "ws-b5947-c-"+stamp
	names := map[string]string{wsA: "Alpha Robotics", wsB: "Bravo Labs", wsC: "Charlie Works"}
	for ws, name := range names {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type, money_kind) VALUES ($1, $2, $1, 'business', 'fiat')`,
			ws, name); err != nil {
			t.Fatal(err)
		}
	}
	// A pays B.
	if _, err := pool.Exec(ctx, `INSERT INTO agent_transfers (id, entry_id, from_workspace_id, from_agent_id, to_workspace_id, to_agent_id,
		amount_ulxc, memo, class) VALUES ($1, gen_random_uuid(), $2, 'agent-a', $3, 'agent-b', 2500000, 'b5947 invoice', 'AMBER')`,
		"xfer_b5947_"+stamp, wsA, wsB); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO distill_serve_attribution (owner_workspace_id, requester_workspace_id, content_hash)
		VALUES ($1, $2, 'b5947')`, wsA, wsB); err != nil {
		t.Fatal(err)
	}

	export := func(ws string) (string, map[string][]map[string]any) {
		t.Helper()
		var buf bytes.Buffer
		if err := support.NewStore(pool).WriteExport(ctx, &buf, ws); err != nil {
			t.Fatalf("export %s: %v", ws, err)
		}
		var dump struct {
			Tables map[string][]map[string]any `json:"tables"`
		}
		if err := json.Unmarshal(buf.Bytes(), &dump); err != nil {
			t.Fatalf("export %s: %v: %.300s", ws, err, buf.String())
		}
		return buf.String(), dump.Tables
	}
	for _, side := range []struct{ ws, other, own, theirs string }{
		{wsA, wsB, "from_workspace", "to_workspace"},
		{wsB, wsA, "to_workspace", "from_workspace"},
	} {
		raw, tables := export(side.ws)
		rows := tables["agent_transfers"]
		if len(rows) != 1 {
			t.Fatalf("%s's export holds %d transfers; want the one it is a party to", side.ws, len(rows))
		}
		row := rows[0]
		if row["id"] != "xfer_b5947_"+stamp || row["memo"] != "b5947 invoice" || row["amount_ulxc"] != float64(2500000) {
			t.Fatalf("%s's transfer row = %v", side.ws, row)
		}
		if row[side.own+"_id"] != side.ws {
			t.Fatalf("%s's transfer row names its own side %v; want %s", side.ws, row[side.own+"_id"], side.ws)
		}
		if row[side.theirs+"_name"] != names[side.other] {
			t.Fatalf("%s's transfer row names the other party %v; want %q", side.ws, row[side.theirs+"_name"], names[side.other])
		}
		if n := len(tables["distill_serve_attribution"]); n != 0 {
			t.Fatalf("%s's export holds %d serve attributions; want none, it is no payment both parties see", side.ws, n)
		}
		if _, ok := row[side.theirs+"_id"]; ok || strings.Contains(raw, side.other) {
			t.Fatalf("%s's export names the other party's workspace id %s", side.ws, side.other)
		}
	}
	if _, tables := export(wsC); len(tables["agent_transfers"]) != 0 {
		t.Fatalf("a third workspace's export holds %d transfers; want none", len(tables["agent_transfers"]))
	}
}
