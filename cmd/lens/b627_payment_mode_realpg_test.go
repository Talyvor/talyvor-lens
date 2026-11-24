package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/economy"
)

// B62.7 — each payment's record says whether it was Sandbox or Live, kept when it is written from the label of what
// moved it at that moment. So the partner's view no longer refuses once something is Live: in Sandbox it shows the
// Sandbox payments, in Live the Live ones, each row with its mode, and never the two mixed.
func TestB627_EachPaymentKeepsItsModeAndThePartnerSeesOneModeAtATime(t *testing.T) {
	w := newB621World(t, "ws-b627")
	ctx := context.Background()
	store := w.b.store

	// In Sandbox: €50.00 from the agent's euro account, sent inside the rules, and €120.00 waiting for the owner.
	if code := w.pay(t, "k-sandbox", w.same, 5_000); code != http.StatusOK {
		t.Fatalf("pay €50.00: %d %v", code, w.lastBody)
	}
	if code := w.pay(t, "k-waiting", w.same, 12_000); code != http.StatusAccepted {
		t.Fatalf("pay €120.00: %d %v", code, w.lastBody)
	}

	// Card payments go live: moving LXC between one owner's own agents is now Live, the euro accounts stay Sandbox.
	store.SetLabelRails("live", nil)
	if _, err := w.pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 50000000, 50000000)
		ON CONFLICT (workspace_id) DO UPDATE SET balance = 50000000, cash_backed_ulxc = 50000000`, w.ws); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, w.ws, w.payer, 10_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PayAgent(ctx, w.ws, w.payer, w.same, 2_000_000, "Live LXC"); err != nil {
		t.Fatalf("pay 2 LXC: %v", err)
	}

	view := func(h http.Handler, path string) (int, []byte) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code, rec.Body.Bytes()
	}
	rowsOf := func(body []byte) (string, []map[string]any) {
		t.Helper()
		var out struct {
			Mode     string           `json:"mode"`
			Payments []map[string]any `json:"payments"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		return out.Mode, out.Payments
	}

	// Something is Live: the view answers in Live, with the Live payment alone.
	code, body := view(newPartnerActivityHandler(store), "/v1/partner/agent-activity")
	if code != http.StatusOK {
		t.Fatalf("the partner's view with one rail live = %d %s, want 200", code, body)
	}
	mode, rows := rowsOf(body)
	if mode != economy.LabelLive || len(rows) != 1 || rows[0]["mode"] != economy.LabelLive || rows[0]["currency"] != "LXC" {
		t.Fatalf("the view in Live = %s %v, want the LXC payment alone, live", mode, rows)
	}
	code, body = view(newPartnerActivitySummaryHandler(store), "/v1/partner/agent-activity/summary")
	var summary struct {
		Mode string               `json:"mode"`
		Days []economy.PartnerDay `json:"days"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &summary) != nil || summary.Mode != economy.LabelLive || len(summary.Days) != 1 ||
		summary.Days[0].Count != 1 {
		t.Fatalf("the summary in Live = %d %s, want one payment", code, body)
	}

	// Each record kept its mode as it was written.
	modeOf := func(sql string, args ...any) string {
		t.Helper()
		var m string
		if err := w.pool.QueryRow(ctx, sql, args...).Scan(&m); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return m
	}
	if m := modeOf(`SELECT mode FROM payment_proofs WHERE agent_id = $1 AND sent->>'currency' = 'EUR'`, w.payer); m != economy.LabelSandbox {
		t.Fatalf("the euro payment's record = %q, want sandbox", m)
	}
	if m := modeOf(`SELECT mode FROM payment_proofs WHERE agent_id = $1 AND sent->>'currency' = 'LXC'`, w.payer); m != economy.LabelLive {
		t.Fatalf("the LXC payment's record = %q, want live", m)
	}
	if m := modeOf(`SELECT mode FROM agent_approvals WHERE id = $1`, w.approvalOf["k-waiting"]); m != economy.LabelSandbox {
		t.Fatalf("the waiting approval = %q, want sandbox", m)
	}

	// Back in Sandbox: the Sandbox payment and the waiting one, never the Live one.
	store.SetLabelRails("test", nil)
	code, body = view(newPartnerActivityHandler(store), "/v1/partner/agent-activity")
	mode, rows = rowsOf(body)
	if code != http.StatusOK || mode != economy.LabelSandbox || len(rows) != 2 {
		t.Fatalf("the view in Sandbox = %d %s, want two payments", code, body)
	}
	for _, row := range rows {
		if row["mode"] != economy.LabelSandbox || row["currency"] != "EUR" {
			t.Fatalf("a row in Sandbox = %v, want a sandbox euro payment", row)
		}
	}
	code, body = view(newPartnerActivityExportHandler(store), "/v1/partner/agent-activity/export")
	lines, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	modeCol := slices.Index(economy.PartnerPaymentFields, "mode")
	if code != http.StatusOK || err != nil || len(lines) != 3 || modeCol < 0 || lines[1][modeCol] != economy.LabelSandbox ||
		lines[2][modeCol] != economy.LabelSandbox {
		t.Fatalf("the CSV in Sandbox = %d %v %v, want two sandbox lines", code, err, lines)
	}
}
