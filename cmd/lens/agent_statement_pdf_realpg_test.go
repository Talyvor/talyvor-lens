package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B28.328 — the owner downloads an agent's statement for any period as a PDF, and its opening and closing balances
// are the JSON statement's for the same period.
func TestAgentStatementRoutes_PDFForAnyPeriod(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b28328-routes"
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, w.Code, w.Body.String())
		}
		return w
	}
	a, err := store.CreateAgent(ctx, ws, "Research", "owner")
	if err != nil {
		t.Fatal(err)
	}
	// Two fundings before the period, so it opens above zero, then a funding and two spends of odd amounts in it.
	for _, p := range []struct {
		kind, at string
		ulxc     int64
	}{
		{"fund", "2026-08-03T10:00:00Z", 12_000_000}, {"fund", "2026-08-20T10:00:00Z", 3_500_001},
		{"fund", "2026-09-02T10:00:00Z", 2_000_000}, {"spend", "2026-09-10T10:00:00Z", -1_234}, {"spend", "2026-09-29T23:59:00Z", -400_000},
	} {
		if _, err := pool.Exec(ctx, `WITH e AS (SELECT gen_random_uuid() AS id)
			INSERT INTO agent_postings (entry_id, workspace_id, account, amount_ulxc, kind, ref, created_at)
			SELECT e.id, $1, l.account, l.amount, $4, 'r', $5::timestamptz
			  FROM e CROSS JOIN (VALUES ('agent:' || $2, $3::bigint), ('workspace', -$3::bigint)) AS l(account, amount)`,
			ws, a.ID, p.ulxc, p.kind, p.at); err != nil {
			t.Fatal(err)
		}
	}
	base := "/v1/workspaces/" + ws + "/agents/" + a.ID + "/statement?from=2026-09-01&to=2026-10-01"
	var st economy.Statement
	if err := json.Unmarshal(get(base).Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	w := get(base + "&format=pdf")
	if w.Header().Get("Content-Type") != "application/pdf" || !strings.HasPrefix(w.Body.String(), "%PDF-") ||
		w.Header().Get("Content-Disposition") != `attachment; filename="agent-statement-2026-09-01-2026-10-01.pdf"` {
		t.Fatalf("the PDF statement: %q, %q, %.40q", w.Header().Get("Content-Type"), w.Header().Get("Content-Disposition"), w.Body.String())
	}
	// Each cell is one string shown in the page's content, in row order: an opening or closing row is its date, its
	// account, the word, two empty cells, then the balance, one per account in the JSON's order.
	var cells []string
	for _, m := range regexp.MustCompile(`\(((?:[^()\\]|\\.)*)\) Tj`).FindAllStringSubmatch(w.Body.String(), -1) {
		cells = append(cells, strings.NewReplacer(`\(`, "(", `\)`, ")").Replace(m[1]))
	}
	balances := map[string][]string{}
	for i := 0; i+3 < len(cells); i++ {
		if cells[i] == "opening" || cells[i] == "closing" {
			balances[cells[i]] = append(balances[cells[i]], cells[i+3])
		}
	}
	if len(balances["opening"]) != len(st.Accounts) || len(balances["closing"]) != len(st.Accounts) {
		t.Fatalf("the PDF's balances %v are not one opening and one closing for each of the JSON's %+v", balances, st.Accounts)
	}
	agentAccount := false
	for i, acc := range st.Accounts {
		agentAccount = agentAccount || acc.Account == "agent:"+a.ID && acc.OpeningULXC == 15_500_001 && acc.ClosingULXC == 17_098_767
		if balances["opening"][i] != economy.LXC(acc.OpeningULXC) || balances["closing"][i] != economy.LXC(acc.ClosingULXC) {
			t.Fatalf("the PDF's balances %v are not the JSON statement's %+v", balances, acc)
		}
	}
	if !agentAccount {
		t.Fatalf("the JSON statement's accounts %+v have no agent account opening at 15.500001 LXC and closing at 17.098767", st.Accounts)
	}
}
