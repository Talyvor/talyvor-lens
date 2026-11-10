package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/tenant"
)

// B28.51 — the owner searches and filters an agent's statement and pages past its newest lines: ?kind= keeps only
// those kinds, ?q= only the lines whose words hold it, and page 2 continues below page 1 under the same filter.
func TestAgentStatementRoutes_FilterByKindSearchAndPage(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b2851-routes"
	store := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	read := func(path string) economy.Statement {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, w.Code, w.Body.String())
		}
		var st economy.Statement
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	a, err := store.CreateAgent(ctx, ws, "Research", "owner")
	if err != nil {
		t.Fatal(err)
	}
	// Five fundings and five spends, a minute apart and alternating, so every page of one kind skips the other.
	if _, err := pool.Exec(ctx, `WITH e AS (SELECT gen_random_uuid() AS id, g FROM generate_series(1, 10) g)
		INSERT INTO agent_postings (entry_id, workspace_id, account, amount_ulxc, kind, ref, created_at)
		SELECT e.id, $1, l.account, l.amount * CASE WHEN e.g % 2 = 0 THEN -1 ELSE 1 END,
		       CASE WHEN e.g % 2 = 0 THEN 'spend' ELSE 'fund' END, CASE WHEN e.g = 4 THEN 'invoice-Acme-42' ELSE 'r' || e.g END,
		       '2026-10-20T09:00:00Z'::timestamptz + e.g * interval '1 minute'
		  FROM e CROSS JOIN (VALUES ('agent:' || $2, 500), ('workspace', -500)) AS l(account, amount)`, ws, a.ID); err != nil {
		t.Fatal(err)
	}
	base := "/v1/workspaces/" + ws + "/agents/" + a.ID + "/statement?from=2026-10-20&to=2026-10-21"
	all := read(base)
	if len(all.Lines) != 10 {
		t.Fatalf("the agent's statement has %d lines; want its 10 postings", len(all.Lines))
	}

	page1 := read(base + "&kind=spend&limit=3")
	page2 := read(base + "&kind=spend&limit=3&cursor=" + page1.NextCursor)
	if len(page1.Lines) != 3 || page1.NextCursor == "" || len(page2.Lines) != 2 || page2.NextCursor != "" {
		t.Fatalf("spend pages: %d lines with cursor %q, then %d with %q; want 3 then the last 2", len(page1.Lines), page1.NextCursor,
			len(page2.Lines), page2.NextCursor)
	}
	seen, lines := map[int64]bool{}, append(page1.Lines, page2.Lines...)
	for i, l := range lines {
		if l.Kind != "spend" {
			t.Fatalf("kind=spend returned a %s line: %+v", l.Kind, l)
		}
		if seen[l.PostingID] {
			t.Fatalf("posting %d is on both pages", l.PostingID)
		}
		seen[l.PostingID] = true
		if i > 0 && !l.At.Before(lines[i-1].At) {
			t.Fatalf("line %d at %s is not below the one before it", i, l.At)
		}
	}
	if len(page1.Accounts) != len(all.Accounts) || page1.Accounts[0] != all.Accounts[0] {
		t.Fatalf("a filtered page's totals %+v are not the period's %+v", page1.Accounts, all.Accounts)
	}

	if st := read(base + "&kind=fund,spend"); len(st.Lines) != 10 {
		t.Fatalf("kind=fund,spend returned %d lines; want all 10", len(st.Lines))
	}
	if st := read(base + "&q=acme"); len(st.Lines) != 1 || st.Lines[0].Ref != "invoice-Acme-42" {
		t.Fatalf("q=acme returned %+v; want the one line with that ref", st.Lines)
	}
	if st := read(base + "&kind=fund&q=acme"); len(st.Lines) != 0 {
		t.Fatalf("kind=fund&q=acme returned %+v; want none: that line is a spend", st.Lines)
	}

	req := httptest.NewRequest(http.MethodGet, base+"&kind=,", nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "kind") {
		t.Fatalf("kind=, = %d %s; want 400", w.Code, w.Body.String())
	}
}
