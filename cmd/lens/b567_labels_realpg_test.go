package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/status"
)

// b567Rails is every partner rail in test mode except the ones named live.
type b567Rails map[partners.Service]bool

func (r b567Rails) Rails() []partners.Rail {
	out := []partners.Rail{}
	for _, s := range partners.Services {
		mode := "test"
		if r[s] {
			mode = "live"
		}
		out = append(out, partners.Rail{Service: s, Mode: mode})
	}
	return out
}

// B56.7 — on the migrated schema: GET /v1/public/labels answers Sandbox for every money capability as production runs
// today, and changes only with the facts; /v1/wallets/capabilities labels each capability for the caller's workspace,
// its plan and whether it is synthetic; /status.json labels each rail's capabilities.
func TestB567_LabelsFollowTheFacts(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := economy.WithUseCountry(context.Background(), "GB") // the clearance below lists GB
	const free, enterprise, synthetic = "ws-b567-free", "ws-b567-ent", "s-b567-test"
	for _, ws := range []struct {
		id        string
		synthetic bool
	}{{free, false}, {enterprise, false}, {synthetic, true}} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, $2)`, ws.id, ws.synthetic); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_contracts (workspace_id, plan, set_by) VALUES ($1, 'enterprise', 'op-b567')`, enterprise); err != nil {
		t.Fatal(err)
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	get := func(h http.Handler, path, ws string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if ws != "" {
			req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws}))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	public := func(cards string) map[string]any {
		t.Helper()
		r := chi.NewRouter()
		r.Get("/v1/public/labels", publicLabelsHandler(store, cards))
		return get(r, "/v1/public/labels", "")
	}
	capabilities := func(ws string) map[string]string {
		t.Helper()
		r := chi.NewRouter()
		mountWalletCapabilityRoutes(r, store)
		out := map[string]string{}
		for _, c := range get(r, "/v1/wallets/capabilities", ws)["capabilities"].([]any) {
			m := c.(map[string]any)
			l, _ := m["label"].(string)
			out[m["capability"].(string)] = l
		}
		return out
	}

	// As production runs today: card payments test, every rail test, nothing cleared.
	store.SetLabelRails("test", b567Rails{})
	page := public("test")
	labels := page["labels"].(map[string]any)
	if len(labels) != len(economy.Capabilities)-2 || page["card_payments"] != "test" {
		t.Fatalf("public labels = %v; want every money capability and card_payments test", page)
	}
	for key, l := range labels {
		if l != economy.LabelSandbox {
			t.Errorf("%s = %v today; want sandbox", key, l)
		}
	}
	for _, key := range []string{"rules_approvals_statements_pots", economy.CapabilityRoomEquity} {
		if _, ok := labels[key]; ok {
			t.Errorf("%s moves no money and has a label", key)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, page["updated_at"].(string)); err != nil {
		t.Errorf("updated_at = %v: %v", page["updated_at"], err)
	}

	// Card payments live: GREEN capabilities are Live, RED ones stay Sandbox.
	store.SetLabelRails("live", b567Rails{})
	labels = public("live")["labels"].(map[string]any)
	if labels["spend_on_talyvor"] != economy.LabelLive || labels[economy.CapabilityFX] != economy.LabelSandbox {
		t.Fatalf("with card payments live: %v", labels)
	}

	// fx on a live rail is Live only once cleared, and Sandbox again once the clearance is revoked.
	store.SetLabelRails("live", b567Rails{partners.ServiceFX: true})
	if l := public("live")["labels"].(map[string]any)[economy.CapabilityFX]; l != economy.LabelSandbox {
		t.Fatalf("fx on a live rail with no clearance = %v; want sandbox", l)
	}
	if _, err := store.ClearCapability(ctx, economy.CapabilityFX, "op-b567", economy.ClearanceTerms{Reference: "ref-b567",
		Licence: "lic-b567", Partner: "fx-partner", Countries: []string{"GB"}, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if l := public("live")["labels"].(map[string]any)[economy.CapabilityFX]; l != economy.LabelLive {
		t.Fatalf("fx cleared on a live rail = %v; want live", l)
	}

	// For a workspace: fx is a B30 capability, Live only on a plan with live money; a synthetic workspace is all Sandbox.
	for _, tc := range []struct{ ws, spend, fx string }{
		{free, economy.LabelLive, economy.LabelSandbox},
		{enterprise, economy.LabelLive, economy.LabelLive},
		{synthetic, economy.LabelSandbox, economy.LabelSandbox},
	} {
		got := capabilities(tc.ws)
		if got["spend_on_talyvor"] != tc.spend || got[economy.CapabilityFX] != tc.fx || got["rules_approvals_statements_pots"] != "" {
			t.Errorf("%s: spend_on_talyvor %q, fx %q, rules %q; want %q, %q and none", tc.ws, got["spend_on_talyvor"],
				got[economy.CapabilityFX], got["rules_approvals_statements_pots"], tc.spend, tc.fx)
		}
	}

	// /status.json labels each rail's capabilities from the same facts.
	sp := status.New(nil, nil, nil, "test")
	sp.UseMoneyRails(b567Rails{partners.ServiceFX: true}, store)
	sp.UpdateCache(sp.Check(ctx))
	var st status.StatusResponse
	w := httptest.NewRecorder()
	sp.ServeJSON(w, httptest.NewRequest(http.MethodGet, "/status.json", nil))
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, r := range st.Rails {
		for _, c := range r.Capabilities {
			want := economy.LabelSandbox
			if c.Key == economy.CapabilityFX {
				want = economy.LabelLive
			}
			if c.Label != want {
				t.Errorf("/status.json %s %s = %q; want %q", r.Service, c.Key, c.Label, want)
			}
			seen++
		}
	}
	if seen == 0 {
		t.Fatal("/status.json lists no rail capabilities")
	}

	if err := store.RevokeClearance(ctx, economy.CapabilityFX, "op-b567", "revoked by B56.7's test"); err != nil {
		t.Fatal(err)
	}
	if l := public("live")["labels"].(map[string]any)[economy.CapabilityFX]; l != economy.LabelSandbox {
		t.Fatalf("fx after its clearance is revoked = %v; want sandbox", l)
	}

	// An invoice's pay page with no live transfer is Sandbox.
	r := chi.NewRouter()
	mountInvoicePayRoutes(r, b5013Pages{economy.Invoice{ID: "inv_b567", WorkspaceID: free, Status: economy.InvoiceSent, DueMinor: 100, Currency: "GBP"}},
		nil, func(string) bool { return false })
	if l := get(r, "/v1/pay/tok_b567", "")["label"]; l != economy.LabelSandbox {
		t.Fatalf("the pay page's label = %v; want sandbox", l)
	}
}
