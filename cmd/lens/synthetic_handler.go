package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/plans"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/workspace"
)

// B17.1 — SYNTHETIC ACCOUNTS THAT CAN NEVER TOUCH REAL MONEY OR REAL USERS.
//
// Two operator routes, registered ONLY when LENS_SYNTHETIC_KEY is set (otherwise a 404):
//
//	POST /v1/synthetic/workspaces        {"count":100} — creates that many synthetic workspaces, each
//	                                     with test credits and a token the harness uses as that user;
//	                                     "plan" (free, the default, team, business or enterprise) puts
//	                                     each on that plan (B35.1).
//	POST /v1/synthetic/workspaces/{wsID}/plan  {"plan":"team"} — moves a synthetic workspace to another
//	                                     plan, to test an upgrade or a downgrade; any other is refused 403.
//	POST /v1/synthetic/workspaces/reset  — every synthetic workspace, or only those named in
//	                                     {"workspaces":["s…",…]}: its stored answers deleted, its test
//	                                     credits restored. Set-based (B26.1): a handful of statements and
//	                                     one Redis pass however many there are.
//
// And, while the key is set, synthetic workspaces older than seven days are deleted every six hours
// (runSyntheticPurge, B26.1).
//
// B25.7 adds four more that bring a test workspace's slow money due inside one tester run
// (synthetic_due_handler.go), and B54.45 one that publishes a DKIM key for a .test domain
// (synthetic_dkim_handler.go).
//
// A synthetic workspace's plan gates it and sets its platform fee exactly as a paying workspace's does
// (internal/plans reads it after a contract and a subscription), but it creates no Stripe object, invoice or
// payment, and its live-money gate is always off whatever the plan says.
//
// The key is compared in constant time, calls are rate-limited, and every call — refused ones too —
// is written to synthetic_operations. What makes a workspace synthetic, and what that forbids, is in
// internal/workspace/synthetic.go and the proxy's partition.

const (
	syntheticKeyHeader = "X-Talyvor-Synthetic-Key"
	// syntheticCreditULXC is each synthetic workspace's test credit: 1,000 LXC, an admin grant, never
	// cash-backed.
	syntheticCreditULXC = int64(1_000) * 1_000_000
	syntheticTokenTTL   = 24 * time.Hour
	syntheticMaxCount   = 1000
	syntheticCallsPer   = time.Minute
	syntheticMaxCalls   = 10
	syntheticMaxBody    = 64 << 10 // every synthetic call's body is a few fields; past it, 413
	// syntheticPurgeAge is how old a synthetic workspace is when the clean-up deletes it (B26.1).
	syntheticPurgeAge   = 7 * 24 * time.Hour
	syntheticPurgeEvery = 6 * time.Hour
)

// syntheticScopes is what a synthetic user's token may do: ask questions, read its analytics, and
// manage its keys and stored answers — what a signed-in owner does.
var syntheticScopes = []string{auth.ScopeProxy, auth.ScopeAnalytics, auth.ScopeKeys}

type syntheticWorkspaces interface {
	CreateSynthetic(ctx context.Context, id, name string) error
	ResolveSynthetic(ctx context.Context, named []string) (ids, notSynthetic []string, err error)
}

type syntheticCredits interface {
	GrantLXC(ctx context.Context, workspaceID string, lxcAmount int64, reason string, metadata map[string]interface{}) (int64, error)
	GrantLXCUpTo(ctx context.Context, workspaceIDs []string, target int64, reason string, metadata map[string]interface{}) (int, error)
}

type syntheticAnswers interface {
	DeleteAllOf(ctx context.Context, wsIDs []string) (storedanswers.Counts, error)
}

type syntheticAudit interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type syntheticDeps struct {
	workspaces syntheticWorkspaces
	credits    syntheticCredits
	answers    syntheticAnswers
	audit      syntheticAudit
	mint       tokenMinter
	plans      plans.Querier      // B35.1: sets and reads a synthetic workspace's plan
	due        syntheticDueDeps   // B25.7; unset, its routes are not registered
	dkim       *syntheticDKIMKeys // B54.45; unset, its route is not registered
}

// callLimiter admits at most max calls in any window of per.
type callLimiter struct {
	mu    sync.Mutex
	max   int
	per   time.Duration
	calls []time.Time
}

func (l *callLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.calls[:0]
	for _, t := range l.calls {
		if now.Sub(t) < l.per {
			kept = append(kept, t)
		}
	}
	l.calls = kept
	if len(l.calls) >= l.max {
		return false
	}
	l.calls = append(l.calls, now)
	return true
}

func mountSyntheticRoutes(r chi.Router, key string, d syntheticDeps) {
	if key == "" {
		return
	}
	limit := &callLimiter{max: syntheticMaxCalls, per: syntheticCallsPer}
	r.Post("/v1/synthetic/workspaces", syntheticGuard(key, "create", limit, d, d.create))
	r.Post("/v1/synthetic/workspaces/reset", syntheticGuard(key, "reset", limit, d, d.reset))
	r.Post("/v1/synthetic/workspaces/{wsID}/plan", syntheticGuard(key, "plan", &callLimiter{max: syntheticMaxDueCalls, per: syntheticCallsPer}, d, d.setPlan))
	mountSyntheticDueRoutes(r, key, d)
	if d.dkim != nil {
		r.Post("/v1/synthetic/dkim-keys", syntheticGuard(key, "dkim-key", &callLimiter{max: syntheticMaxCalls, per: syntheticCallsPer}, d, d.dkim.publish))
	}
}

// syntheticGuard checks the key and the rate, runs the action, and records the call either way.
func syntheticGuard(key, action string, limit *callLimiter, d syntheticDeps,
	run func(w http.ResponseWriter, r *http.Request) (int, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n, outcome := 0, "ok"
		defer func() { d.record(r, action, n, outcome) }()
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(syntheticKeyHeader)), []byte(key)) != 1 {
			outcome = "unauthorized"
			writeJSONErr(w, http.StatusUnauthorized, "the synthetic operator key is required")
			return
		}
		if !limit.allow(time.Now()) {
			outcome = "rate_limited"
			w.Header().Set("Retry-After", "60")
			writeJSONErr(w, http.StatusTooManyRequests, fmt.Sprintf("at most %d synthetic calls a minute", syntheticMaxCalls))
			return
		}
		// The body is read here, under the cap, so no action decodes an unbounded one.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, syntheticMaxBody))
		if tooBig := (*http.MaxBytesError)(nil); errors.As(err, &tooBig) {
			outcome = "too_large"
			writeJSONErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a synthetic call's body is at most %d bytes", syntheticMaxBody))
			return
		} else if err != nil {
			outcome = "error: bad request"
			writeJSONErr(w, http.StatusBadRequest, "the request body is unreadable")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		n, outcome = run(w, r)
	}
}

func (d syntheticDeps) record(r *http.Request, action string, n int, outcome string) {
	if d.audit == nil {
		return
	}
	if _, err := d.audit.Exec(r.Context(),
		`INSERT INTO synthetic_operations (action, accounts, remote_addr, outcome) VALUES ($1, $2, $3, $4)`,
		action, n, r.RemoteAddr, outcome); err != nil {
		slog.Error("synthetic: audit write failed", "action", action, "outcome", outcome, "err", err)
	}
}

type syntheticUser struct {
	WorkspaceID string `json:"workspace_id"`
	Token       string `json:"token"`
	ExpiresAt   string `json:"expires_at"`
}

func (d syntheticDeps) create(w http.ResponseWriter, r *http.Request) (int, string) {
	in := struct {
		Count int    `json:"count"`
		Plan  string `json:"plan"`
	}{Count: 100}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return 0, "error: bad request"
		}
	}
	if in.Count < 1 || in.Count > syntheticMaxCount {
		writeJSONErr(w, http.StatusBadRequest, fmt.Sprintf("count must be 1 to %d", syntheticMaxCount))
		return 0, "error: bad count"
	}
	if in.Plan != "" && !slices.Contains(plans.Order, in.Plan) {
		writeJSONErr(w, http.StatusBadRequest, fmt.Sprintf("plan must be one of %s", strings.Join(plans.Order, ", ")))
		return 0, "error: bad plan"
	}
	users := make([]syntheticUser, 0, in.Count)
	expires := time.Now().Add(syntheticTokenTTL).UTC().Format(time.RFC3339)
	for i := 0; i < in.Count; i++ {
		id, err := syntheticID()
		if err == nil {
			err = d.workspaces.CreateSynthetic(r.Context(), id, fmt.Sprintf("Synthetic user %s", id[1:7]))
		}
		if err == nil && in.Plan != "" {
			err = d.putOnPlan(r.Context(), id, in.Plan)
		}
		if err == nil {
			_, err = d.credits.GrantLXC(r.Context(), id, syntheticCreditULXC, "synthetic test credits",
				map[string]interface{}{"synthetic": true})
		}
		var tok string
		if err == nil {
			tok, err = d.mint(id, id, syntheticScopes, syntheticTokenTTL)
		}
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, fmt.Sprintf("created %d, then: %v", len(users), err))
			return len(users), "error: " + err.Error()
		}
		users = append(users, syntheticUser{WorkspaceID: id, Token: tok, ExpiresAt: expires})
	}
	if in.Plan == "" {
		in.Plan = plans.Free
	}
	slog.Info("synthetic: workspaces created", "count", len(users), "plan", in.Plan)
	writeJSONOK(w, http.StatusCreated, map[string]any{"created": len(users), "plan": in.Plan, "workspaces": users})
	return len(users), "ok"
}

// errNotSynthetic is a plan asked for a workspace that is not an active synthetic one.
var errNotSynthetic = errors.New("not an active synthetic workspace")

// putOnPlan records plan as synthetic workspace id's plan. It changes nothing on any other workspace.
func (d syntheticDeps) putOnPlan(ctx context.Context, id, plan string) error {
	if d.plans == nil {
		return errors.New("synthetic plans need the database")
	}
	err := d.plans.QueryRow(ctx, `UPDATE workspaces SET synthetic_plan = $2, updated_at = NOW()
		WHERE id = $1 AND synthetic AND active RETURNING id`, id, plan).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotSynthetic
	}
	return err
}

// setPlan moves a synthetic workspace to another plan and answers the plan and gates it is now on — after a
// contract or a test-mode subscription it bought, which still come first.
func (d syntheticDeps) setPlan(w http.ResponseWriter, r *http.Request) (int, string) {
	ws := chi.URLParam(r, "wsID")
	var in struct {
		Plan string `json:"plan"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return 0, "error: bad request"
	}
	if !slices.Contains(plans.Order, in.Plan) {
		writeJSONErr(w, http.StatusBadRequest, fmt.Sprintf("plan must be one of %s", strings.Join(plans.Order, ", ")))
		return 0, "error: bad plan"
	}
	switch err := d.putOnPlan(r.Context(), ws, in.Plan); {
	case errors.Is(err, errNotSynthetic):
		writeJSONErr(w, http.StatusForbidden, "only a test (synthetic) workspace's plan can be set here")
		return 0, "refused: not a test workspace"
	case err != nil:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	plan, err := plans.Of(r.Context(), d.plans, ws)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	slog.Info("synthetic: plan set", "workspace", ws, "plan", in.Plan, "on", plan.Plan)
	writeJSONOK(w, http.StatusOK, map[string]any{"workspace_id": ws, "plan": plan})
	return 1, "ok"
}

func (d syntheticDeps) reset(w http.ResponseWriter, r *http.Request) (int, string) {
	var in struct {
		Workspaces []string `json:"workspaces"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return 0, "error: bad request"
		}
	}
	ids, notSynthetic, err := d.workspaces.ResolveSynthetic(r.Context(), in.Workspaces)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	if len(notSynthetic) > 0 {
		writeJSONErr(w, http.StatusBadRequest, fmt.Sprintf("%d named workspaces are not active synthetic workspaces, nothing was reset: %s",
			len(notSynthetic), strings.Join(notSynthetic[:min(len(notSynthetic), 5)], ", ")))
		return 0, "error: not synthetic"
	}
	if _, err := d.answers.DeleteAllOf(r.Context(), ids); err != nil {
		writeJSONErr(w, http.StatusInternalServerError, fmt.Sprintf("stored answers of %d: %v", len(ids), err))
		return 0, "error: " + err.Error()
	}
	if _, err := d.credits.GrantLXCUpTo(r.Context(), ids, syntheticCreditULXC, "synthetic test credits restored",
		map[string]interface{}{"synthetic": true}); err != nil {
		writeJSONErr(w, http.StatusInternalServerError, fmt.Sprintf("credits of %d: %v", len(ids), err))
		return 0, "error: " + err.Error()
	}
	slog.Info("synthetic: workspaces reset", "count", len(ids), "named", len(in.Workspaces) > 0)
	writeJSONOK(w, http.StatusOK, map[string]any{"reset": len(ids)})
	return len(ids), "ok"
}

type syntheticPurger interface {
	PurgeStaleSynthetic(ctx context.Context, cutoff time.Time) (workspace.PurgeResult, error)
}

// runSyntheticPurge deletes the synthetic workspaces older than syntheticPurgeAge, and their Redis copies,
// at start and every syntheticPurgeEvery until ctx ends.
func runSyntheticPurge(ctx context.Context, ws syntheticPurger, answers syntheticAnswers) {
	t := time.NewTicker(syntheticPurgeEvery)
	defer t.Stop()
	for {
		if err := purgeStaleSynthetic(ctx, ws, answers, time.Now()); err != nil {
			slog.Warn("synthetic: stale workspaces not purged", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func purgeStaleSynthetic(ctx context.Context, ws syntheticPurger, answers syntheticAnswers, now time.Time) error {
	res, err := ws.PurgeStaleSynthetic(ctx, now.Add(-syntheticPurgeAge))
	if err != nil {
		return err
	}
	if len(res.Purged) > 0 {
		if _, err := answers.DeleteAllOf(ctx, res.Purged); err != nil {
			return fmt.Errorf("purged %d, then their stored answers: %w", len(res.Purged), err)
		}
	}
	if len(res.Purged)+len(res.Kept) > 0 {
		slog.Info("synthetic: stale workspaces purged", "purged", len(res.Purged),
			"kept_crossing_real", len(res.Kept), "tables_unreached", res.Unreached)
	}
	return nil
}

// syntheticID is "s" and 26 random base32 characters — the shape of a provisioned "u…" id, never
// equal to one.
func syntheticID() (string, error) {
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "s" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))[:26], nil
}
