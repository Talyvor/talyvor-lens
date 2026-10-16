package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/anomaly"
	"github.com/talyvor/lens/internal/learner"
	"github.com/talyvor/lens/internal/povi"
)

// B42.1: the reads that used to walk every workspace. Two workspaces each hold one of every row
// these routes read; every value carries its workspace's id, so "does the body name wsalpha" is
// "did a wsalpha row reach this caller". Against real Postgres, because the claim is about a WHERE.
func TestGlobalReads_ScopedToTheCallersWorkspace(t *testing.T) {
	pool := usagePool(t)
	ctx := context.Background()
	for _, ws := range []string{"wsalpha", "wsbravo"} {
		for h := 2; h < 50; h++ { // 48 past hours (≥ the 24 buckets Detect needs) then a spike now
			if _, err := pool.Exec(ctx, `INSERT INTO token_events (workspace_id, provider, model, input_tokens, output_tokens, cost_usd, team, feature, prompt_hash, created_at)
				VALUES ($1,'openai','gpt-4o-mini',10,10,$2,'t','f',$3, NOW() - make_interval(hours => $4))`, ws, float64(1+h%2), "hash-"+ws, h); err != nil {
				t.Fatalf("seed token_events: %v", err)
			}
		}
		for _, q := range []string{
			`INSERT INTO token_events (workspace_id, provider, model, input_tokens, output_tokens, cost_usd, team, feature, prompt_hash) VALUES ($1,'openai','gpt-4o-mini',10,10,1000,'t','f','hash-'||$1)`,
			`INSERT INTO prompt_embeddings (provider, model, prompt_hash, response, hit_count, workspace_id) VALUES ('openai','gpt-4o-mini','pe-'||$1,'r',5,$1)`,
			`INSERT INTO povi_stakes (node_id, workspace_id, amount) VALUES ('node-'||$1, $1, 1)`,
			// both challenges on ONE node: ?node= alone used to be the only filter
			`INSERT INTO povi_challenges (id, request_id, node_id, workspace_id, result) VALUES ('ch-'||$1, 'req-'||$1, 'node-shared', $1, 'pass')`,
		} {
			if _, err := pool.Exec(ctx, q, ws); err != nil {
				t.Fatalf("seed %q: %v", q, err)
			}
		}
	}

	s := &Server{
		pool:            pool,
		analyser:        learner.New(runEmbeddedNATS(t), pool),
		anomalyDetector: anomaly.New(pool),
		alertManager:    alerts.New(nil, nil, []alerts.SpendRule{{ID: "rule-wsalpha", Team: "wsalpha", WindowHours: 1, WarningUSD: 1}}),
		poviStakes:      povi.NewStakeManager(povi.NewNodeStakeStore(pool), nil, nil, 0, 0, nil),
		poviChallenges:  povi.NewChallengeStore(pool),
		economyEnabled:  true,
	}
	r := newRouter(t, s)

	routes := []struct {
		path      string
		adminOnly bool
	}{
		{"/v1/api/anomalies/scan", false},
		{"/v1/api/cache/top-patterns", false},
		{"/v1/api/models/recommendations", false},
		{"/v1/api/povi/stakes", false},
		{"/v1/api/povi/challenges?node=node-shared", false},
		{"/v1/api/alerts/circuits", true},
		{"/v1/api/alerts/rules", true},
		{"/v1/api/distill/summary", true},
	}
	callers := []struct {
		name, ws   string
		admin      bool
		sees       []string // workspaces whose rows must appear
		mustNotSee []string
	}{
		{"workspace A", "wsalpha", false, []string{"wsalpha"}, []string{"wsbravo"}},
		{"workspace B", "wsbravo", false, []string{"wsbravo"}, []string{"wsalpha"}},
		{"admin key", "", true, []string{"wsalpha", "wsbravo"}, nil},
	}
	for _, rt := range routes {
		for _, c := range callers {
			t.Run(rt.path+"/"+c.name, func(t *testing.T) {
				w := httptest.NewRecorder()
				r.ServeHTTP(w, apiReq(rt.path, c.ws, c.admin))
				body := w.Body.String()
				if rt.adminOnly {
					want := http.StatusForbidden
					if c.admin {
						want = http.StatusOK
					}
					if w.Code != want {
						t.Fatalf("status = %d, want %d; body=%s", w.Code, want, body)
					}
					if c.admin && rt.path == "/v1/api/alerts/rules" && !strings.Contains(body, "wsalpha") {
						t.Errorf("the admin key no longer sees the rules: %s", body)
					}
					return
				}
				if w.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200; body=%s", w.Code, body)
				}
				for _, ws := range c.sees {
					if !strings.Contains(body, ws) {
						t.Errorf("%s does not see %s's rows: %s", c.name, ws, body)
					}
				}
				for _, ws := range c.mustNotSee {
					if strings.Contains(body, ws) {
						t.Errorf("%s reads %s's rows: %s", c.name, ws, body)
					}
				}
			})
		}
	}
}
