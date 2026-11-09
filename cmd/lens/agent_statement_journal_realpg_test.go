package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B28.329 — the owner exports a period as a double-entry journal with cost centres, for Xero or QuickBooks: its
// debits equal its credits, and each of the statement's accounts is debited its in_ulxc and credited its out_ulxc.
func TestAgentStatementRoutes_JournalBalancesToTheStatement(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b28329-routes"
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
	research, err := store.CreateAgent(ctx, ws, "Research", "owner")
	if err != nil {
		t.Fatal(err)
	}
	support, err := store.CreateAgent(ctx, ws, "Support", "owner")
	if err != nil {
		t.Fatal(err)
	}
	// A funding before the period, then in it: a funding of each agent, a spend with three postings, and a transfer
	// to another workspace's agent, whose other side the workspace's statement does not hold.
	post := func(kind, at string, legs ...any) {
		t.Helper()
		args, values := []any{at, kind}, []string{}
		for i := 0; i < len(legs); i += 3 {
			n := len(args)
			values = append(values, fmt.Sprintf("($%d, $%d, $%d::bigint)", n+1, n+2, n+3))
			args = append(args, legs[i:i+3]...)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO agent_postings (entry_id, workspace_id, account, amount_ulxc, kind, ref, created_at)
			SELECT md5($1)::uuid, l.ws, l.account, l.amount, $2, 'r-'||$2, $1::timestamptz
			  FROM (VALUES `+strings.Join(values, ", ")+`) AS l(ws, account, amount)`, args...); err != nil {
			t.Fatal(err)
		}
	}
	a, b := "agent:"+research.ID, "agent:"+support.ID
	post("fund", "2026-08-03T10:00:00Z", ws, a, int64(12_000_000), ws, "workspace", int64(-12_000_000))
	post("fund", "2026-09-02T10:00:00Z", ws, a, int64(2_000_001), ws, "workspace", int64(-2_000_001))
	post("fund", "2026-09-03T10:00:00Z", ws, b, int64(700_000), ws, "workspace", int64(-700_000))
	post("spend", "2026-09-10T10:00:00Z", ws, a, int64(-1_234), ws, "spend", int64(1_200), ws, "workspace", int64(34))
	post("pay", "2026-09-29T23:59:00Z", ws, a, int64(-400_000), "ws-b28329-other", "agent:other-company", int64(400_000))

	for _, c := range []struct {
		path    string
		entries int
	}{
		{"/v1/workspaces/" + ws + "/agents/" + research.ID + "/statement", 3},
		{"/v1/workspaces/" + ws + "/agents/statement", 4},
	} {
		base := c.path + "?from=2026-09-01&to=2026-10-01"
		var st economy.Statement
		if err := json.Unmarshal(get(base).Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		w := get(base + "&format=journal")
		if w.Header().Get("Content-Type") != "text/csv; charset=utf-8" ||
			w.Header().Get("Content-Disposition") != `attachment; filename="agent-journal-2026-09-01-2026-10-01.csv"` {
			t.Fatalf("%s: the journal is %q, %q", c.path, w.Header().Get("Content-Type"), w.Header().Get("Content-Disposition"))
		}
		rows, err := csv.NewReader(w.Body).ReadAll()
		if err != nil || len(rows) < 2 || strings.Join(rows[0], ",") != "journal_no,date,account,description,debit_lxc,credit_lxc,cost_centre,ref" {
			t.Fatalf("%s: the journal CSV %v (%v)", c.path, rows, err)
		}
		ulxc := func(v string) int64 {
			if v == "" {
				return 0
			}
			n, err := strconv.ParseInt(strings.Replace(v, ".", "", 1), 10, 64)
			if err != nil || !strings.Contains(v, ".") {
				t.Fatalf("%s: amount %q is not LXC to the sixth decimal", c.path, v)
			}
			return n
		}
		// Every entry here moved Research's money, but for Support's funding.
		supportJournals := map[string]bool{}
		for _, row := range rows[1:] {
			supportJournals[row[0]] = supportJournals[row[0]] || row[2] == b
		}
		debits, credits, perJournal := map[string]int64{}, map[string]int64{}, map[string]int64{}
		var debit, credit int64
		for _, row := range rows[1:] {
			d, cr := ulxc(row[4]), ulxc(row[5])
			debits[row[2]] += d
			credits[row[2]] += cr
			perJournal[row[0]] += d - cr
			debit, credit = debit+d, credit+cr
			if want := map[bool]string{false: research.ID, true: support.ID}[supportJournals[row[0]]]; row[6] != want {
				t.Fatalf("%s: line %v has cost centre %q, want %q", c.path, row, row[6], want)
			}
		}
		if debit != credit || debit == 0 {
			t.Fatalf("%s: the journal debits %d and credits %d", c.path, debit, credit)
		}
		for j, sum := range perJournal {
			if sum != 0 {
				t.Fatalf("%s: journal %s does not balance: %d", c.path, j, sum)
			}
		}
		if len(perJournal) != c.entries {
			t.Fatalf("%s: %d journals, want %d: one per entry in the period", c.path, len(perJournal), c.entries)
		}
		for _, acc := range st.Accounts {
			if debits[acc.Account] != acc.InULXC || credits[acc.Account] != acc.OutULXC {
				t.Fatalf("%s: %s is debited %d and credited %d; the statement has it in %d and out %d",
					c.path, acc.Account, debits[acc.Account], credits[acc.Account], acc.InULXC, acc.OutULXC)
			}
		}
	}
}
