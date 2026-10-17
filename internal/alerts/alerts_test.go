package alerts

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/pashagolub/pgxmock/v4"
)

func runEmbeddedNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := &natsserver.Options{
		Host:     "127.0.0.1",
		Port:     -1,
		StoreDir: t.TempDir(),
		NoLog:    true,
		NoSigs:   true,
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("natsserver.NewServer: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		srv.Shutdown()
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(func() {
		nc.Close()
		srv.Shutdown()
		srv.WaitForShutdown()
	})
	return nc
}

func newTestManager(t *testing.T, rules []SpendRule) (*AlertManager, *nats.Conn, pgxmock.PgxPoolIface) {
	t.Helper()
	nc := runEmbeddedNATS(t)
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return newAlertManager(pool, nc, rules), nc, pool
}

func TestRecordSpend_CostForGPT4o(t *testing.T) {
	mgr, _, pool := newTestManager(t, nil)

	// gpt-4o: input $2.50/M, output $10.00/M.
	// 100_000 in + 50_000 out = 0.25 + 0.50 = $0.75
	const wantCost = 0.75

	// Column order: workspace_id, provider, model, in, out, team, sprint_id,
	// feature, cost, prompt, session, request, modality, cost_estimated,
	// distill_method. This row is an image request → modality "image", estimated
	// true; not distilled → distill_method "".
	pool.ExpectExec(`INSERT INTO token_events`).
		WithArgs("ws-test", "openai", "gpt-4o", 100000, 50000, "core", "sprint-1", "search", wantCost, "p", "", "", "image", true, "", "", 0, 0, 0.0, "", "", "", (*float64)(nil), (*float64)(nil)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := mgr.RecordSpend(context.Background(), "ws-test", "core", "sprint-1", "search", "gpt-4o", 100000, 50000, "p", "", "", "image", true); err != nil {
		t.Fatalf("RecordSpend: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet pgxmock expectations: %v", err)
	}
}

func TestRecordSpend_CostForClaudeHaiku(t *testing.T) {
	mgr, _, pool := newTestManager(t, nil)

	// claude-haiku-4-5: input $1.00/M, output $5.00/M — published at
	// https://platform.claude.com/docs/en/about-claude/pricing (fetched 2026-07-26).
	// 1_000_000 in + 1_000_000 out = 1.00 + 5.00 = $6.00
	//
	// ⚠ This asserted $4.80 — that is HAIKU 3.5's rate (0.80/4.00), left behind when the id was
	// updated to 4.5 — so token_events under-recorded every Haiku 4.5 request's COGS by 20%.
	const wantCost = 6.00

	pool.ExpectExec(`INSERT INTO token_events`).
		WithArgs("ws-test", "anthropic", "claude-haiku-4-5", 1000000, 1000000, "core", "", "search", wantCost, "p", "", "", "text", false, "", "", 0, 0, 0.0, "", "", "", (*float64)(nil), (*float64)(nil)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := mgr.RecordSpend(context.Background(), "ws-test", "core", "", "search", "claude-haiku-4-5", 1000000, 1000000, "p", "", "", "text", false); err != nil {
		t.Fatalf("RecordSpend: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet pgxmock expectations: %v", err)
	}
}

// RecordSpendWithDistill writes the DISTILL method as the 15th token_events
// column so the savings story is auditable per request. A 'convert' row carries
// the distilled request's (lower) count; a 'vision_ocr' row is the OCR sub-call's
// own model-priced cost — never blended.
func TestRecordSpendWithDistill_TagsMethod(t *testing.T) {
	cases := []struct {
		name, method, modality string
		estimated              bool
	}{
		{"convert", "convert", "document", false},
		{"vision_ocr", "vision_ocr", "document", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, _, pool := newTestManager(t, nil)
			pool.ExpectExec(`INSERT INTO token_events`).
				WithArgs(
					pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
					pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
					pgxmock.AnyArg(), pgxmock.AnyArg(), tc.modality, tc.estimated, tc.method,
					"", 0, 0, 0.0, "", // tare_*: not a Tare-reduced request
					"",                                   // auth_method: no credential on this context
					"", (*float64)(nil), (*float64)(nil), // B27.32: no saving measured on this context
				).
				WillReturnResult(pgxmock.NewResult("INSERT", 1))

			if err := mgr.RecordSpendWithDistill(context.Background(), "ws-test", "core", "", "search", "claude-haiku-4-5", 1000, 40, "", "", "", tc.modality, tc.estimated, tc.method); err != nil {
				t.Fatalf("RecordSpendWithDistill: %v", err)
			}
			if err := pool.ExpectationsWereMet(); err != nil {
				t.Fatalf("distill_method %q not written as the 15th token_events column: %v", tc.method, err)
			}
		})
	}
}

// TestRecordSpend_PersistsWorkspaceID is the regression guard for the
// per-workspace spend-cap correctness fix. alerts.RecordSpend historically
// omitted workspace_id, so every token_events row fell back to the column
// default 'default' and the cap query (SUM(cost_usd) WHERE workspace_id=$1)
// summed ALL workspaces together. RecordSpend must now write the caller's
// real workspace_id as the FIRST token_events column so the cap — and the
// new workspace-scoped budgets — aggregate per workspace.
func TestRecordSpend_PersistsWorkspaceID(t *testing.T) {
	for _, ws := range []string{"alpha", "beta"} {
		mgr, _, pool := newTestManager(t, nil)
		// First positional arg MUST be the caller's workspace id, never 'default'.
		pool.ExpectExec(`INSERT INTO token_events`).
			WithArgs(ws,
				pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
				pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
				pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
				pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
				pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
				pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))

		if err := mgr.RecordSpend(context.Background(), ws, "core", "", "search", "gpt-4o", 10, 10, "", "", "", "text", false); err != nil {
			t.Fatalf("ws=%s RecordSpend: %v", ws, err)
		}
		if err := pool.ExpectationsWereMet(); err != nil {
			t.Fatalf("ws=%s: real workspace_id not persisted as first token_events column: %v", ws, err)
		}
	}
}

func TestRecordSpend_FiresWarningAlertOverThreshold(t *testing.T) {
	rules := []SpendRule{{
		ID: "r1", Team: "core", Feature: "search",
		WindowHours: 1, WarningUSD: 1.0, CriticalUSD: 10.0, CircuitUSD: 100.0,
	}}
	mgr, nc, pool := newTestManager(t, rules)

	// Subscribe before triggering so we don't miss the publish.
	sub, err := nc.SubscribeSync("lens.alerts.warning")
	if err != nil {
		t.Fatalf("SubscribeSync: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	pool.ExpectExec(`INSERT INTO token_events`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// Spend is above warning ($1) but below critical ($10).
	pool.ExpectQuery(`SUM\(cost_usd\)`).
		WithArgs("ws1", "core", "search").
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(float64(2.5)))

	if err := mgr.RecordSpend(context.Background(), "ws1", "core", "", "search", "gpt-4o", 1000, 1000, "", "", "", "text", false); err != nil {
		t.Fatalf("RecordSpend: %v", err)
	}

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("expected warning alert on NATS, got: %v", err)
	}
	var got Alert
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		t.Fatalf("unmarshal alert: %v", err)
	}
	if got.Level != AlertWarning {
		t.Errorf("Level = %q, want %q", got.Level, AlertWarning)
	}
	if got.Team != "core" || got.Feature != "search" {
		t.Errorf("Team/Feature = %q/%q, want core/search", got.Team, got.Feature)
	}
	if got.SpendUSD != 2.5 {
		t.Errorf("SpendUSD = %v, want 2.5", got.SpendUSD)
	}
}

func TestRecordSpend_OpensCircuitOverThreshold(t *testing.T) {
	rules := []SpendRule{{
		ID: "r1", Team: "core", Feature: "search",
		WindowHours: 1, WarningUSD: 1.0, CriticalUSD: 10.0, CircuitUSD: 50.0,
	}}
	mgr, _, pool := newTestManager(t, rules)

	pool.ExpectExec(`INSERT INTO token_events`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// Spend above CircuitUSD.
	pool.ExpectQuery(`SUM\(cost_usd\)`).
		WithArgs("ws1", "core", "search").
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(float64(75.0)))

	if mgr.IsCircuitOpen("ws1", "core", "search") {
		t.Fatal("circuit should start closed")
	}

	if err := mgr.RecordSpend(context.Background(), "ws1", "core", "", "search", "gpt-4o", 1000, 1000, "", "", "", "text", false); err != nil {
		t.Fatalf("RecordSpend: %v", err)
	}

	if !mgr.IsCircuitOpen("ws1", "core", "search") {
		t.Fatal("expected circuit to be open after spend exceeded CircuitUSD")
	}
}

func TestIsCircuitOpen_FalseWhenClosed(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	if mgr.IsCircuitOpen("ws1", "any", "thing") {
		t.Error("IsCircuitOpen should be false when no circuit has been opened")
	}
}

func TestIsCircuitOpen_TrueWhenOpen(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	mgr.openCircuitForTest("ws1", "core", "search")
	if !mgr.IsCircuitOpen("ws1", "core", "search") {
		t.Error("IsCircuitOpen should be true after circuit was opened")
	}
}

func TestGetDowngradeModel(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	if got := mgr.GetDowngradeModel("openai", "gpt-4o"); got != "gpt-4o-mini" {
		t.Errorf("openai downgrade = %q, want gpt-4o-mini", got)
	}
	if got := mgr.GetDowngradeModel("anthropic", "claude-opus-4-5"); got != "claude-haiku-4-5" {
		t.Errorf("anthropic downgrade = %q, want claude-haiku-4-5", got)
	}
}

func TestResetCircuit_ClosesOpenCircuit(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	mgr.openCircuitForTest("ws1", "core", "search")
	if !mgr.IsCircuitOpen("ws1", "core", "search") {
		t.Fatal("setup: circuit should be open")
	}

	mgr.ResetCircuit("ws1", "core", "search")

	if mgr.IsCircuitOpen("ws1", "core", "search") {
		t.Error("ResetCircuit should close an open circuit")
	}
}

// Sanity-check the cost arithmetic with tiny inputs so float rounding is exact.
func TestCostUSD_KnownValues(t *testing.T) {
	cases := []struct {
		model     string
		inT, outT int
		want      float64
	}{
		{"gpt-4o", 1_000_000, 0, 2.50},
		{"gpt-4o", 0, 1_000_000, 10.00},
		{"gpt-4o-mini", 1_000_000, 1_000_000, 0.75},
		{"gpt-4.1-nano", 1_000_000, 1_000_000, 0.50},
		{"claude-opus-4-5", 1_000_000, 1_000_000, 30.00}, // CORRECTED: 5+25, not the 15+75 pinned here before
		{"claude-sonnet-4-5", 1_000_000, 1_000_000, 18.00},
		{"claude-haiku-4-5", 1_000_000, 1_000_000, 6.00}, // CORRECTED: 1+5, not Haiku 3.5's 0.8+4
		{"unknown-model", 999, 999, 0},
	}
	for _, tc := range cases {
		got := costUSD(tc.model, tc.inT, tc.outT)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("costUSD(%s, %d, %d) = %v, want %v", tc.model, tc.inT, tc.outT, got, tc.want)
		}
	}
}

func TestCostUSD_ClaudeOpus46(t *testing.T) {
	// claude-opus-4-6: input $5/M, output $25/M — published at
	// https://platform.claude.com/docs/en/about-claude/pricing (fetched 2026-07-26).
	// 1M in + 1M out = 5.00 + 25.00 = 30.00
	//
	// ⚠ THIS TEST ASSERTED 90.00 (i.e. 15/75, which is OPUS 4.1's rate) and passed for months while
	// Lens over-billed Opus 4.6 by 3x. It was one of SIX tests across three files pinning at least one
	// of five wrong rates. A test that encodes whatever the code currently does cannot detect that the
	// code is wrong — see internal/catalog/published_rates_test.go, which pins rates to a cited URL and
	// is now the authority for every number here.
	got := CostUSD("claude-opus-4-6", 1_000_000, 1_000_000)
	if math.Abs(got-30.00) > 1e-9 {
		t.Errorf("CostUSD(claude-opus-4-6, 1M, 1M) = %v, want 30.00", got)
	}
}

func TestCostUSD_GPT54(t *testing.T) {
	// gpt-5.4: input $2.50/M, output $15/M — published at
	// https://developers.openai.com/api/docs/pricing (fetched 2026-07-26).
	// 100K in + 1M out = 0.25 + 15.00 = 15.25 (above 272K in, its B38.3 long-prompt rates apply instead).
	//
	// ⚠ Previously asserted 25.00 (5/20), over-billing input 2x. Same failure as Opus 4.6 above.
	got := CostUSD("gpt-5.4", 100_000, 1_000_000)
	if math.Abs(got-15.25) > 1e-9 {
		t.Errorf("CostUSD(gpt-5.4, 100K, 1M) = %v, want 15.25", got)
	}
}
